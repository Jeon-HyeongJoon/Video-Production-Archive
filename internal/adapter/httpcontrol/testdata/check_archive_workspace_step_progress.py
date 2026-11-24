import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/jobs/JOB-RUNNING", wait_until="networkidle")
    disclosure = page.locator('#job-steps .step-item[data-step="render"] .progress-evidence')
    expect(disclosure.locator("summary")).to_have_text("실행 근거 2")
    expect(disclosure.locator("li")).to_have_count(2)
    expect(disclosure.locator("li").first).not_to_be_visible()
    disclosure.locator("summary").click()
    expect(disclosure.locator("li").first).to_contain_text("2개 계획 · 전체 0:10 · 청크 계획")
    expect(disclosure.locator("li").nth(1)).to_contain_text("1/2 · 0:00–0:05 · 처리 완료")
    rendered = disclosure.inner_text()
    if any(secret in rendered for secret in ("stdout", "stderr", "C:\\", "/private/", "credential")):
        raise AssertionError(f"unsafe process data leaked into progress disclosure: {rendered!r}")
    def live_evidence(route):
        response = route.fetch()
        payload = response.json()
        step = next(step for step in payload["data"]["steps"] if step["id"] == "render")
        base = step["progress_evidence"][0]
        updates = [
            {"operation": "RENDER_RECORDING", "status": "CHECKING_INPUTS"},
            {"operation": "RENDER_RECORDING", "status": "OUTPUT_POSITION", "output_millis": 7500, "input_index": 2, "input_count": 3},
            {"operation": "TRANSCRIBE_AUDIO", "status": "PROCESSING", "input_index": 2, "input_count": 3,
             "chunk_index": 1, "chunk_count": 2, "source_end_millis": 5000, "process_percent": 80},
        ]
        step["progress_evidence"] = [dict(base, evidence_id=f"EVD-{i}", update=u) for i, u in enumerate(updates)]
        step["progress_history_omitted"] = 2
        step["items"] = {"operation": "RENDER_RECORDING", "current": 2, "total": 3, "completed": 1, "position_millis": 7500,
                         "duration_millis": 12000, "name": "현장.mp4", "attempt": 2}
        route.fulfill(response=response, json=payload)

    page.route("**/api/v1/jobs/JOB-RUNNING", live_evidence)
    page.reload(wait_until="networkidle")
    expect(page.locator('#job-steps [data-step="render"] .item-progress')).to_have_text("영상 1/3개 완료 · 2번째 현장.mp4 · 0:08 / 0:12 · 2번째 시도")
    expect(disclosure.locator("li").first).not_to_be_visible()
    disclosure.locator("summary").click()
    expect(disclosure).to_contain_text("이전 2건은 이벤트 원장에 보관")
    expect(disclosure.locator("li").nth(0)).to_contain_text("입력 검증 진입")
    expect(disclosure.locator("li").nth(1)).to_contain_text("입력 2/3 · 출력 위치")
    expect(disclosure.locator("li").nth(2)).to_contain_text("전사 중 · 엔진 80%")
    browser.close()
