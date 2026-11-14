import sys

from playwright.sync_api import expect, sync_playwright


CARD_ID = "ACTION-SELECTION-REQUIRED"
pending = True
rejected_reason = ""


def sessions(route):
    response = route.fetch()
    payload = response.json()
    session = payload["data"][0]
    card = {
        "action_card_id": CARD_ID,
        "archivist_turn_id": session["turns"][0]["turn_id"],
        "command_type": "archive_intake.selection.required",
        "target_id": session["session_id"],
        "expected_revision": 0,
        "state": "PENDING" if pending else "REJECTED",
        "approval_required": False,
    }
    session.setdefault("action_cards", []).append(card)
    route.fulfill(response=response, json=payload)


def reject(route):
    global pending, rejected_reason
    rejected_reason = route.request.post_data_json["reason"]
    pending = False
    route.fulfill(status=200, content_type="application/json", body='{"data":{"state":"REJECTED"}}')


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.route("**/api/v1/archivist-sessions", sessions)
    page.route(f"**/api/v1/action-cards/{CARD_ID}/reject", reject)
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")

    card = page.locator(f'[data-card-id="{CARD_ID}"][data-state="PENDING"]')
    expect(card).to_be_visible()
    expect(card).to_contain_text("선택 필요")
    expect(card.locator("button")).to_have_text(["닫기", "작업 대상 선택"])
    page.locator("#alert-button").click()
    expect(page.locator(f'#alert-panel [data-key="card:{CARD_ID}"]')).to_be_visible()
    page.locator("#alert-button").click()

    card.locator('[data-action="card-reject"]').click()
    expect(page.locator(f'[data-card-id="{CARD_ID}"][data-state="PENDING"]')).to_have_count(0)
    page.locator("#alert-button").click()
    expect(page.locator(f'#alert-panel [data-key="card:{CARD_ID}"]')).to_have_count(0)
    if rejected_reason != "선택 요청 닫음":
        raise AssertionError(f"selection close did not preserve its reason: {rejected_reason}")
    browser.close()
