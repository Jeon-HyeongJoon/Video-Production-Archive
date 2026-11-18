import sys

from playwright.sync_api import expect, sync_playwright


def failing(folder):
    # The next projection reports one more failed folder than the screen has seen.
    def handle(route):
        response = route.fetch()
        payload = response.json()
        for item in payload["data"][0]["folders"]:
            if item["folder"] == folder:
                item["stages"][-1]["state"] = "FAILED"
        route.fulfill(response=response, json=payload)
    return handle


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.clock.install()
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    banner = page.locator("#fresh-banner")
    button = page.locator("#alert-button")
    # Problems already present when the screen opens are counted but not new.
    expect(page.locator("#alert-count")).to_have_text("6")
    expect(button).to_have_attribute("data-new", "false")
    expect(banner).to_be_hidden()
    button.click()
    panel = page.locator("#alert-panel")
    expect(panel.locator(".alert-item")).to_have_count(6)
    expect(panel.locator(".new-tag")).to_have_count(0)
    button.click()
    expect(panel).to_be_hidden()

    page.route("**/api/v1/archive-intake", failing("intro"))
    page.clock.run_for(5_100)
    expect(banner).to_be_visible()
    expect(page.locator("#fresh-text")).to_have_text("VOLUME-1 / intro")
    expect(banner).to_contain_text("새 처리 실패")
    # The toast rises at the bottom of the screen, over nothing else.
    placed = page.evaluate("""() => { const toast = document.querySelector('#fresh-banner').getBoundingClientRect(), view = document.querySelector('#view').getBoundingClientRect();
      return toast.right <= view.right && toast.bottom <= innerHeight && toast.top > view.top + view.height / 2; }""")
    if not placed:
        raise AssertionError("new-problem toast is not at the bottom of the screen")
    expect(page.locator("#alert-count")).to_have_text("7")
    expect(button).to_have_attribute("data-new", "true")
    button.click()
    expect(panel.locator(".alert-item")).to_have_count(7)
    fresh = panel.locator('.alert-item[data-key="fault:VOLUME-1:intro"]')
    expect(fresh.locator(".new-tag")).to_have_text("새")
    fresh.click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1?folder=intro")
    expect(panel).to_be_hidden()
    expect(page.locator('#drive-problems .problem-panel[data-folder-id="intro"]')).to_have_attribute("data-open", "true")
    # Opening an alert acknowledges that occurrence. The problem remains in
    # its drive, but the bell no longer keeps notifying it.
    expect(page.locator("#alert-count")).to_have_text("6")
    button.click()
    expect(panel.locator('.alert-item[data-key="fault:VOLUME-1:intro"]')).to_have_count(0)
    button.click()
    page.reload(wait_until="networkidle")
    expect(page.locator("#alert-count")).to_have_text("6")
    button.click()
    expect(panel.locator('.alert-item[data-key="fault:VOLUME-1:intro"]')).to_have_count(0)
    button.click()

    # The banner leads to the same place, or goes away without moving.
    page.unroute("**/api/v1/archive-intake")
    page.route("**/api/v1/archive-intake", failing("render"))
    page.clock.run_for(5_100)
    expect(banner).to_be_visible()
    page.locator("#fresh-close").click()
    expect(banner).to_be_hidden()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1?folder=intro")
    expect(page.locator("#alert-count")).to_have_text("6")
    page.unroute("**/api/v1/archive-intake")
    page.route("**/api/v1/archive-intake", failing("event-b"))
    page.clock.run_for(5_100)
    page.locator("#fresh-go").click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1?folder=event-b")
    expect(banner).to_be_hidden()
    browser.close()
