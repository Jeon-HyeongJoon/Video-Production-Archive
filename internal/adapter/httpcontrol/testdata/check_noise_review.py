import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(locale="ko-KR")
    posted = []
    revision = {"decision": "review-1"}

    def inventory(route):
        response = route.fetch()
        payload = response.json()
        drive = next(d for d in payload["data"] if d["workspace_drive_id"] == "VOLUME-1")
        folder = next(f for f in drive["folders"] if f["folder"] == "hold")
        folder.update(
            decision_id=revision["decision"],
            noise_review={
                "kind": "CAMERA_PROXY", "decision_authority": "HUMAN",
                "matched_count": 125, "unconfirmed_count": 1, "unconfirmed_reasons": {"UNPAIRED_PROXY": 1},
                "matched_examples": [{"relative_path": "SUB/C01S01.mp4", "matched_original": "CLIP/C01.mp4"}],
                "unconfirmed_examples": [{"relative_path": "SUB/C02S01.mp4", "reason_code": "UNPAIRED_PROXY"}],
            },
            work_options=[{"disposition": "HOLD"}, {"disposition": "EXCLUDE_FROM_RUN"}],
        )
        route.fulfill(json=payload)

    def override(route):
        posted.append(route.request.post_data_json)
        route.fulfill(json={"data": {}})

    page.route("**/api/v1/archive-intake", inventory)
    page.route("**/api/v1/archive-intake/VOLUME-1/override", override)
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1?folder=hold", wait_until="networkidle")
    panel = page.locator('#drive-problems .problem-panel[data-folder-id="hold"]')
    submit = panel.locator("[data-action=hold]")
    expect(panel).to_contain_text("대응 원본 확인 125개 · 미확인 1개")
    expect(panel).to_contain_text("이 폴더의 영상 전체에 적용됩니다")
    expect(panel.get_by_text("판단 근거 보기")).to_be_visible()
    expect(panel.get_by_text("SUB/C01S01.mp4", exact=False)).to_be_hidden()
    panel.get_by_text("판단 근거 보기").click()
    expect(panel.get_by_text("SUB/C01S01.mp4", exact=False)).to_be_visible()
    expect(panel).to_contain_text("CLIP/C01.mp4")
    expect(panel).to_contain_text("SUB/C02S01.mp4")
    expect(panel).to_contain_text("대응 원본 미확인 · 1개")
    expect(panel.locator('[aria-pressed="true"]')).to_have_count(0)
    expect(submit).to_be_disabled()
    panel.locator('[data-choice="exclude"]').click()
    expect(submit).to_be_enabled()
    revision["decision"] = "review-2"
    expect(panel.locator('[aria-pressed="true"]')).to_have_count(0, timeout=15000)
    expect(submit).to_be_disabled()
    assert not posted
    panel.locator('[data-choice="exclude"]').click()
    with page.expect_response(lambda response: response.url.endswith("/api/v1/archive-intake/VOLUME-1/override") and response.request.method == "POST"):
        submit.click()
    expect(submit).to_be_disabled()
    assert len(posted) == 1 and posted[0]["expected_decision_id"] == "review-2"
    assert posted[0]["disposition"] == "EXCLUDE_FROM_RUN"
    browser.close()
