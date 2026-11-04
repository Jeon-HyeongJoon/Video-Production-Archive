import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.clock.install()
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1?folder=hold", wait_until="networkidle")
    stale = page.locator("#stale-banner")
    hold = page.locator('#drive-problems .problem-panel[data-folder-id="hold"]')
    hold.locator('[data-choice="render-6000"]').click()
    submit = page.locator("#hold-submit")
    expect(submit).to_be_enabled()
    expect(stale).to_be_hidden()

    # Two missed refreshes are tolerated; the third marks the screen stale.
    page.route("**/api/v1/**", lambda route: route.abort())
    page.clock.run_for(10_100)
    expect(stale).to_be_hidden()
    page.clock.run_for(5_000)
    expect(stale).to_be_visible()
    expect(page.locator("#stale-text")).to_contain_text("이후 갱신 없음 · 다시 연결 중 · 판정·실행 잠김")
    expect(submit).to_be_disabled()
    expect(page.locator("[data-action]:enabled")).to_have_count(0)

    page.unroute("**/api/v1/**")
    page.clock.run_for(5_000)
    expect(stale).to_be_hidden()
    expect(page.locator("#hold-submit")).to_be_enabled()
    browser.close()
