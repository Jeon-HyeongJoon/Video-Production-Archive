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

    # The instruction stays visible; its references fold behind one row.
    expect(thread.locator(".message.user")).to_have_count(1)
    refs = thread.locator(".message.user .refs-fold")
    expect(refs).not_to_have_attribute("open", "")
    expect(refs.locator("summary")).to_have_text("첨부한 작업 5")
    expect(refs.locator(".ref-chip")).to_have_text(["WORKER-1", "VOLUME-1", "source-main", "archive-main", "8 폴더"])

    # Recalls, progress, tool calls and settled cards fold behind 작업 내역.
    fold = thread.locator("details.work-fold")
    expect(fold).to_have_count(1)
    expect(fold).not_to_have_attribute("open", "")
    start_card = fold.locator('[data-card-id="ACTION-CALL-ARCHIVE-RUN-1"]')
    expect(start_card).to_contain_text("영상 정리·보관 시작")
    expect(start_card).to_contain_text("VOLUME-1 → archive-main · 8 폴더")
    expect(start_card.locator("button")).to_have_count(0)
    expect(start_card).to_be_hidden()
    expect(fold.locator(".message.system").first).to_be_hidden()
    expect(thread.locator(".message.archivist > .message-body:visible")).to_have_count(1)

    if scene == "stage-one":
        # A running work shows only its last line beside the latest answer.
        expect(fold.locator("summary")).to_have_text("처리 내역 · 실행 2 · 자동 이어서 1")
        expect(thread.locator(".message.archivist > .message-body:visible")).to_have_text("선택한 8 폴더의 분류·렌더 시작을 요청했습니다.")
        live = thread.locator(".turn-live")
        expect(live).to_have_count(1)
        expect(live).to_contain_text("자동 진행")
        expect(live).to_contain_text("보류 발생 · hold")
        expect(live.locator(".token")).to_have_text("시작 대기")
        expect(page.locator("#archivist-availability")).to_have_text("처리 중")
        expect(page.locator("#archivist-message")).to_be_disabled()
        expect(page.locator("#archivist-disabled-reason")).to_have_text("응답 대기 · 시작 대기")
        fold.locator("summary").click()
        expect(fold.locator(".message.system")).to_have_count(1)
        expect(fold.locator(".message.system").first).to_be_visible()
        expect(fold.locator(".turn-entry")).to_have_count(1)
        expect(fold.locator(".turn-trace .tool-row")).to_have_count(2)
    elif scene == "stt-ready":
        expect(fold.locator("summary")).to_have_text("처리 내역 · 실행 5 · 자동 이어서 2")
        expect(thread.locator(".message.archivist > .message-body:visible")).to_contain_text("1단계가 끝났습니다")
        expect(thread.locator(".turn-live")).to_have_count(0)
        expect(page.locator("#archivist-availability")).to_have_text("요청 가능")
        expect(page.locator("#archivist-message")).to_be_enabled()
        stt_card = thread.locator('[data-card-id="ACTION-CALL-STT-1"]')
        expect(stt_card).to_contain_text("자막·텍스트 만들기")
        expect(stt_card).to_contain_text("실행 전 확인 필요")
        expect(stt_card).to_contain_text("ARCHIVE-RUN-1 · 대상 4 폴더")
        expect(stt_card.locator("button")).to_have_count(2)

        # A message sent from persistent work detail pins its kind, ID, and
        # revision instead of relying on text in the prompt.
        page.goto(sys.argv[1] + "/#/runs/ARCHIVE-RUN-1", wait_until="networkidle")
        expect(page.locator("#selection-chips .selection-chip")).to_have_text(["영상 정리·보관 · ARCHIVE-RUN-1 · r1"])

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
        payload = turn_request.value.post_data_json
        if payload["prompt"] != "첫 줄\n둘째 줄" or payload.get("work_reference") != {"kind": "ARCHIVE_RUN", "work_id": "ARCHIVE-RUN-1", "revision": 1}:
            raise AssertionError(f"Enter submitted the wrong prompt: {turn_request.value.post_data_json}")
        expect(message).to_have_value("")
    else:
        # Completed work keeps the initial instruction and final report visible.
        # Progress, recalls, prior answers and settled cards remain inspectable.
        expect(fold.locator("summary")).to_contain_text("처리 내역 · 실행 6 ·")
        expect(thread.locator(".work-result > .message-body")).to_contain_text("완료했습니다")
        expect(fold.locator(".message.system").first).to_be_hidden()
        expect(fold.locator(".turn-entry")).to_have_count(4)
        expect(thread.locator('.action-card[data-state="PENDING"]')).to_have_count(0)
        fold.locator("summary").click()
        expect(fold.locator(".message.system").first).to_be_visible()
        expect(fold.locator(".turn-entry").first).to_be_visible()

    browser.close()
