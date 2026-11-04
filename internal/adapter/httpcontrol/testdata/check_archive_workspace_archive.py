import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
    stored = page.locator('#drive-folders .folder-row:has(.archive-badge[data-state="STORED"])')
    expect(stored).to_have_count(1)
    expect(stored.locator(".archive-badge")).to_have_text("보관됨")
    stored.click()
    detail = page.locator("#folder-detail")
    expect(detail.locator('.archive-badge[data-state="STORED"]')).to_have_text("보관됨")
    expect(detail.locator(".folder-note")).to_have_text("STT 추가 가능")
    committed = page.locator('#drive-folders .folder-row:has(.archive-badge[data-state="COMMITTED"])')
    expect(committed).to_have_count(1)
    expect(committed.locator(".archive-badge")).to_have_text("확정됨")
    expect(committed.locator(".status-text")).to_have_count(0)
    committed.click()
    expect(detail.locator('.archive-badge[data-state="COMMITTED"]')).to_have_text("확정됨")
    expect(detail.locator(".folder-note")).to_have_count(0)
    # A committed record offers no operation that changes it.
    expect(detail.locator("[data-action], select, input")).to_have_count(0)
    browser.close()
