import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="dark", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")

    expect(page.locator("#drive-contents")).to_be_visible()
    expect(page.locator("#drive-contents .section-title")).to_contain_text("영상 폴더")
    expect(page.locator('#drive-folders [data-folder-id="event-a"]')).to_have_count(1)
    expect(page.locator(".source-video-list")).to_have_count(0)

    page.locator('#drive-folders [data-folder-id="event-a"]').click()
    records = page.locator('#folder-detail[data-folder-id="event-a"] .source-video-row:not(.source-video-head)')
    expect(records).to_have_count(2)
    expect(records.nth(0)).to_contain_text("C0001.MP4")
    expect(records.nth(0)).to_contain_text("event-a/C0001.MP4")
    expect(records.nth(0)).to_contain_text("3.0 GB")

    expect(page.locator("#prepare-run, #drive-prep, #start-archive-run, #archive-scope")).to_have_count(0)

    # The only way on is the top-level work creation surface, opened with this drive chosen.
    page.locator("#open-new-work").click()
    expect(page).to_have_url(sys.argv[1] + "/#/create")
    expect(page.locator('#prep-drives [data-drive-id="VOLUME-1"]')).to_have_attribute("aria-pressed", "true")
    expect(page.locator("#archive-scope")).to_be_visible()
    browser.close()
