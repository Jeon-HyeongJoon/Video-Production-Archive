import sys

from playwright.sync_api import expect, sync_playwright


scene = sys.argv[2]
with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1], wait_until="networkidle")
    # The pane is part of every screen; nothing opens it.
    expect(page.locator("#archivist-panel")).to_be_visible()
    expect(page.locator("#archivist-session-select option:checked")).to_have_text("codex · Archive Archivist")
    thread = page.locator("#archivist-thread")
    if page.evaluate("getComputedStyle(document.querySelector('#archivist-thread')).overflowY") != "auto":
        raise AssertionError("Agent thread does not scroll on its own")

    expect(thread.locator(".message.user")).to_have_count(1)
    expect(thread.locator(".message.user .ref-chip")).to_have_text(
        ["WORKER-1", "VOLUME-1", "source-main", "archive-main", "8 폴더"]
    )
    start_card = thread.locator('[data-card-id="ACTION-CALL-ARCHIVE-RUN-1"]')
    expect(start_card).to_contain_text("아카이브 실행 시작")
    expect(start_card).to_contain_text("VOLUME-1 → archive-main · 8 폴더")
    expect(start_card.locator("button")).to_have_count(0)

    if scene in ("stage-one", "stt-ready"):
        # The ArchiveRun work stays expanded until its explicit completion
        # recall settles; completed earlier Turns do not fold alone.
        expect(thread.locator("details.work-fold")).to_have_count(0)
        expected_turns = 2 if scene == "stage-one" else 3
        expect(thread.locator(".message.system")).to_have_count(expected_turns - 1)
        expect(thread.locator(".turn-entry")).to_have_count(1 if scene == "stage-one" else 3)
        expect(thread.locator(".turn-live .tool-row")).to_have_count(2 if scene == "stage-one" else 5)

    if scene == "stage-one":
        expect(thread.locator(".message.system")).to_contain_text("재호출 · 보류 발생 · hold")
        expect(thread.locator(".message.system .token")).to_have_text("대기열")
        expect(page.locator("#archivist-availability")).to_have_text("처리 중")
        expect(page.locator("#archivist-message")).to_be_disabled()
        expect(page.locator("#archivist-disabled-reason")).to_have_text("응답 대기 · 대기열")
    elif scene == "stt-ready":
        expect(page.locator("#archivist-availability")).to_have_text("사용 가능")
        expect(page.locator("#archivist-message")).to_be_enabled()
        stt_card = thread.locator('[data-card-id="ACTION-CALL-STT-1"]')
        expect(stt_card).to_contain_text("STT 시작")
        expect(stt_card).to_contain_text("결정 대기")
        expect(stt_card).to_contain_text("ARCHIVE-RUN-1 · 대상 4 폴더")
        expect(stt_card.locator("button")).to_have_count(2)

        def capture_turn(route):
            route.fulfill(status=201, content_type="application/json", body='{"data":{"turn_id":"TURN-KEYBOARD"}}')

        page.route("**/api/v1/archivist-sessions/*/turns", capture_turn)
        message = page.locator("#archivist-message")
        message.fill("첫 줄")
        message.press("Shift+Enter")
        message.type("둘째 줄")
        expect(message).to_have_value("첫 줄\n둘째 줄")
        with page.expect_request("**/api/v1/archivist-sessions/*/turns") as turn_request:
            message.press("Enter")
        if turn_request.value.post_data_json["prompt"] != "첫 줄\n둘째 줄":
            raise AssertionError(f"Enter submitted the wrong prompt: {turn_request.value.post_data_json}")
        expect(message).to_have_value("")
    else:
        # Completed work keeps the initial instruction and final report visible.
        # Progress, recalls, prior answers and settled cards remain inspectable.
        fold = thread.locator("details.work-fold")
        expect(fold).to_have_count(1)
        expect(fold).not_to_have_attribute("open", "")
        expect(fold.locator("summary")).to_contain_text("작업 내역 · 도구 6 ·")
        expect(thread.locator(".work-result > .message-body")).to_contain_text("완료했습니다")
        expect(fold.locator(".message.system").first).to_be_hidden()
        expect(fold.locator(".turn-entry")).to_have_count(4)
        expect(thread.locator('.action-card[data-state="PENDING"]')).to_have_count(0)
        fold.locator("summary").click()
        expect(fold.locator(".message.system").first).to_be_visible()
        expect(fold.locator(".turn-entry").first).to_be_visible()

    browser.close()
