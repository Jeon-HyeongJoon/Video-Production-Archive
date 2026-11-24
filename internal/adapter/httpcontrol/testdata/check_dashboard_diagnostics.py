import sys

from playwright.sync_api import expect, sync_playwright


def turn(number, work="JOB-1", cause="NO_INPUT", action="CHECK_INPUT", *, legacy=False, attempt=1):
    row = {
        "turn_id": f"TURN-{number}", "submitted_by": "SYSTEM", "tool_policy": "REMEDIATION", "state": "COMPLETED",
        "work_reference": {"kind": "JOB", "work_id": work, "revision": 4 if legacy else number},
        "prompt": f"자동 점검 요청 {number}", "final_response": {"summary": f"진단 보고 원문 {number}"},
        "created_at": f"2026-11-24T12:{number:02d}:00Z", "updated_at": f"2026-11-24T12:{number:02d}:20Z",
    }
    if not legacy:
        row["diagnostic_check"] = {"subjects": [f"{work}/render/{attempt}"], "failed_step": "render",
                                   "states": ["FAILED"], "causes": [cause], "actions": [action]}
    return row


def sessions(route):
    payload = route.fetch().json()
    session = payload["data"][0]
    human = dict(turn(2), submitted_by="HUMAN", tool_policy="DIAGNOSTIC", prompt="직접 요청한 점검")
    pending = dict(turn(9), state="QUEUED")
    session["turns"] = [turn(1), human, turn(3), turn(4, cause="DISK_FULL", action="FREE_SPACE"),
                        turn(5, "JOB-2"), turn(6, "JOB-2"), turn(7, legacy=True), turn(8, legacy=True), pending,
                        turn(10, attempt=2)]
    session["action_cards"] = [{"action_card_id": "CHECK-DECISION", "archivist_turn_id": "TURN-1",
                                "command_type": "archive_run.start", "state": "PENDING", "approval_required": True}]
    route.fulfill(json=payload)


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.route("**/api/v1/archivist-sessions", sessions)
    page.goto(sys.argv[1], wait_until="networkidle")
    thread = page.locator("#archivist-thread")
    groups = thread.locator(".diagnostic-history")
    expect(groups).to_have_count(3)
    changed = groups.filter(has_text="3회 점검")
    expect(changed.locator(".check-summary")).to_contain_text("영상 변환 실패")
    expect(changed.locator(".check-summary")).to_contain_text("원인 · 가능한 조치 변경")
    expect(changed.get_by_text("진단 보고 원문 4")).to_be_visible()
    changed.get_by_text("변경 근거", exact=True).click()
    expect(changed.locator(".check-diff li")).to_have_text(["원인 제외 · NO_INPUT", "원인 추가 · DISK_FULL",
                                                           "가능한 조치 제외 · CHECK_INPUT", "가능한 조치 추가 · FREE_SPACE"])
    expect(thread.locator('[data-card-id="CHECK-DECISION"] button[data-action="card-execute"]')).to_be_visible()
    expect(thread.get_by_text("직접 요청한 점검")).to_be_visible()
    expect(thread.locator('[data-turn-id="TURN-9"].turn-live')).to_be_visible()
    expect(thread.locator('.system-source[data-turn-id="TURN-10"]')).to_be_visible()
    expect(groups.filter(has_text="상태 변화 없음").locator(".check-summary")).to_contain_text("2회 점검")
    expect(groups.filter(has_text="변화 비교 자료 없음").locator(".check-summary")).to_contain_text("2회 점검")
    changed.locator(".check-summary").press("Enter")
    reports = changed.locator(".check-entries > details")
    expect(reports).to_have_count(3)
    reports.last.locator(":scope > summary").click()
    expect(reports.last.get_by_text("진단 보고 원문 1")).to_be_visible()
    reports.last.locator(".system-source > summary").click()
    expect(reports.last.get_by_text("자동 점검 요청 1")).to_be_visible()
    browser.close()
