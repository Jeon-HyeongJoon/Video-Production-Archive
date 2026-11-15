import json
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    starts = []

    def hosts(route):
        response = route.fetch()
        payload = response.json()
        for host in payload["data"]:
            if host["archivist_host_id"] == "archivist-mac":
                host["state"] = "OFFLINE"
                host["detected_cli_ids"] = ["codex"]
        route.fulfill(response=response, body=json.dumps(payload), content_type="application/json")

    def sessions(route):
        if route.request.method == "POST":
            return create_session(route)
        response = route.fetch()
        payload = response.json()
        payload["data"].append({
            "session_id": "ARCHIVIST-OFFLINE",
            "name": "꺼진 Codex",
            "archivist_host_id": "archivist-mac",
            "backend": "EMBEDDED_CORE",
            "cli_id": "codex",
            "workspace_root_id": "video-project",
            "turns": [],
            "action_cards": [],
        })
        route.fulfill(response=response, body=json.dumps(payload), content_type="application/json")

    def start(route):
        starts.append(route.request.url)
        route.fulfill(status=202, content_type="application/json", body=json.dumps({
            "data": {"archivist_host_id": "archivist-mac", "state": "CONNECTING"}
        }))

    def create_session(route):
        request = route.request.post_data_json
        route.fulfill(status=201, content_type="application/json", body=json.dumps({"data": {
            "session_id": request["session_id"],
            "name": request["name"],
            "archivist_host_id": request["archivist_host_id"],
            "backend": "EMBEDDED_CORE",
            "cli_id": request["cli_id"],
            "workspace_root_id": "video-project",
            "job_id": "",
        }}))

    page.route("**/api/v1/archivist-hosts", hosts)
    page.route("**/api/v1/archivist-sessions", sessions)
    page.route("**/api/v1/archivist-hosts/*/start", start)
    page.goto(sys.argv[1], wait_until="networkidle")

    with page.expect_request("**/api/v1/archivist-hosts/archivist-mac/start"):
        page.locator("#archivist-session-select").select_option("ARCHIVIST-OFFLINE")
    expect(page.locator("#archivist-availability")).to_have_text("연결 중")
    expect(page.locator("#archivist-disabled-reason")).to_have_text("에이전트를 자동으로 켜고 있음")
    expect(page.locator('[data-action="archivist-start"]')).to_have_text("에이전트 켜는 중")

    page.locator("#open-settings").click()
    page.locator('#archivist-add input[name="name"]').fill("새 Codex")
    page.locator("#archivist-host-select").select_option("archivist-mac")
    with page.expect_request("**/api/v1/archivist-hosts/archivist-mac/start"):
        page.locator("#archivist-add-submit").click()
    expect(page.locator("#archivist-add-status")).to_have_text("추가됨 · 에이전트 연결 중")
    if len(starts) != 2:
        raise AssertionError(f"expected selection and add to start the Host, got {starts}")
    browser.close()
