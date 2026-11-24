import json
import sys
from urllib.parse import parse_qs, urlencode, urlsplit

from playwright.sync_api import expect, sync_playwright


def change_data(page, endpoint, update):
    def intercept(route):
        data = route.fetch().json()["data"]
        update(data)
        route.fulfill(json={"data": data})
    page.route("**/api/v1/" + endpoint, intercept)


def outcome(page, base):
    def failed(works):
        next(w for w in works if w["work_id"] == "ARCHIVE-RUN-1")["state"] = "FAILED"

    def terminal(drives):
        drive = drives[0]
        drive["archive_run_state"] = "COMPLETED"
        drive["folders"] = [next(f for f in drive["folders"] if f["folder"] == "broken")]

    change_data(page, "work", failed)
    change_data(page, "archive-intake", terminal)
    page.goto(base + "/#/jobs")
    row = page.locator('#attention-jobs button[data-work-id="ARCHIVE-RUN-1"]')
    expect(row.locator(".job-state")).to_have_text("실패")
    row.click()
    children = row.locator("..").locator(".children-head")
    expect(children).to_contain_text("실패 1")
    expect(children).to_contain_text("대기 0")
    children.get_by_role("link").click()
    expect(page.locator('#run-summary [data-fact="state"]')).to_have_text("실패")
    expect(page.locator('#run-summary [data-fact="progress"]')).to_contain_text("0 / 1 폴더")
    expect(page.locator('#run-summary [data-stage="one"]')).to_have_attribute("data-state", "fault")
    page.get_by_role("link", name="드라이브 열기").click()
    expect(page.locator('#drive-overview [data-fact="state"]')).to_have_text("실패")
    page.goto(base + "/#/connections")
    expect(page.locator('#connection-drives [data-drive-id="VOLUME-1"] .state')).to_have_text("실패")


def diagnosis(page, base):
    def jobs(rows):
        rows.append({"job_id": "failed-child", "revision": 8, "step_state": "FAILED"})

    def detail(data):
        data["failures"] = [{"job_id": "failed-child", "folder": "broken", "step_id": "render", "error_class": "RENDER_FAILED"}]

    def intake(drives):
        broken = next(f for f in drives[0]["folders"] if f["folder"] == "broken")
        broken["job_id"] = "failed-child"
        broken["stages"] = [{"stage_id": "render", "state": "FAILED"}]

    change_data(page, "archive-intake", intake)
    change_data(page, "jobs", jobs)
    change_data(page, "work/ARCHIVE-RUN-1", detail)
    posts = []

    def capture(route):
        posts.append(route.request.post_data_json)
        route.fulfill(status=201, json={"data": {}})

    page.route("**/api/v1/archivist-sessions/*/turns", capture)
    page.goto(base + "/#/runs/ARCHIVE-RUN-1")
    draft = page.locator("#archivist-message")
    draft.fill("작성 중 지시")
    expect(page.locator('#run-attention [data-folder-id="broken"]')).to_have_count(0)
    expect(page.locator('#run-failures [data-action="redo"]')).to_be_visible()
    with page.expect_response(lambda r: r.request.method == "POST" and r.url.endswith("/turns")):
        page.locator('#run-failures [data-action="diagnose"]').click()
    expect(draft).to_have_value("작성 중 지시")
    assert len(posts) == 1
    turn = posts[0]
    assert turn["diagnosis_only"] and turn["work_reference"] == {"kind": "JOB", "work_id": "failed-child", "revision": 8}
    assert "render" in turn["prompt"] and "근거" in turn["prompt"]


def proxies(page, base):
    def inventory(drives):
        drive = drives[0]
        for folder in drive.get("discovered_folders") or []:
            if folder["folder"] in ("broken", "ready"):
                n = int(folder["folder"] == "ready")
                folder["source_role"] = {"role": "CAMERA_PROXY", "matched_proxies": 3, "unpaired_proxies": n}
        # Scoped decisions can have a different role from directory discovery.
        for folder in drive["folders"]:
            folder["source_role"] = {"role": "RECORDING"}

    def partial(route):
        data = route.fetch().json()["data"]
        if "folder=ready" in route.request.url:
            data[0]["context_evidence"] = {"case": "UNPAIRED_PROXY", "witness": "ready"}
            data[1]["matched_original"] = "original.mov"
        route.fulfill(json={"data": data})

    page.route("**/api/v1/archive-intake/*/files?**", partial)
    change_data(page, "archive-intake", inventory)
    writes = []
    page.on("request", lambda req: writes.append(req.url) if req.method == "POST" else None)
    page.goto(base + "/#/drives/VOLUME-1")
    panel = page.get_by_role("region", name="발견된 프록시 후보")
    expect(panel.get_by_role("button")).to_contain_text("2 폴더")
    expect(panel).to_contain_text("대응 원본 6")
    expect(panel).to_contain_text("미확인 1")
    panel.get_by_role("button").click()
    expect(panel.get_by_role("combobox")).to_have_value("ready")
    expect(panel.locator(".source-video-list")).to_contain_text("ready/C0001.MP4")
    expect(panel.locator(".source-video-list")).not_to_contain_text("ready/C0002.MOV")
    panel.get_by_role("button", name="전체 2").click()
    proof = panel.locator(".file-proof").nth(1)
    expect(proof.locator("small").first).to_be_hidden()
    proof.locator("summary").press("Enter")
    expect(proof.get_by_text("대응 원본 · original.mov")).to_be_visible()
    panel.get_by_role("combobox").select_option("broken")
    expect(panel.locator(".source-video-list")).to_contain_text("broken/C0001.MP4")
    assert not writes


def transfer(page, base):
    path = base + "/api/v1/archive-intake"
    legacy = page.request.get(path).json()["data"][0]
    assert legacy["discovered_folders"][0]["videos"]
    first = page.request.get(path, headers={"X-Archive-Inventory": "{}"}).json()["data"][0]
    assert first["inventory_token"] and first["discovered_folders"] and not first.get("discovered_directories")
    assert all(not f["videos"] for f in first["discovered_folders"] + (first["folders"] or []))
    known = json.dumps({first["workspace_drive_id"]: first["inventory_token"]})
    second = page.request.get(path, headers={"X-Archive-Inventory": known}).json()["data"][0]
    assert second["inventory_unchanged"] and not second["discovered_folders"] and second["scope_is_inventory"] and not second["folders"]
    requests = []
    page.on("request", lambda r: requests.append(r.url) if "/files?" in r.url else None)
    page.goto(base + "/#/drives/VOLUME-1")
    with page.expect_response(lambda r: r.url == path and first["workspace_drive_id"] in r.request.headers.get("x-archive-inventory", "")):
        expect(page.locator("#drive-contents")).to_be_visible()
    assert not requests
    page.locator('#drive-folders button[data-folder-id="event-a"]').click()
    expect(page.locator(".source-video-list")).to_contain_text("event-a/C0001.MP4")
    assert len(requests) == 1
    url = urlsplit(requests[0])
    query = parse_qs(url.query, keep_blank_values=True)
    for key in ["revision", "decision", "run"]:
        stale = dict(query, **{key: ["99999"]})
        assert page.request.get(url._replace(query=urlencode(stale, doseq=True)).geturl()).status == 409


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    {"outcome": outcome, "diagnosis": diagnosis, "proxies": proxies, "transfer": transfer}[sys.argv[2]](page, sys.argv[1])
    browser.close()
