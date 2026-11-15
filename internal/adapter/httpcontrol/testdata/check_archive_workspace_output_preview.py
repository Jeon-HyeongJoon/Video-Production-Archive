import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="dark", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/create", wait_until="networkidle")
    page.locator('#prep-drives [data-drive-id="VOLUME-1"]').click()
    page.locator('#prep-folders [data-folder-id="event-a"] input').check()
    page.locator('#prep-folders [data-folder-id="source-a"] input').check()
    page.locator('#prep-archives [data-storage-id="archive-main"]').click()

    preview = page.locator("#expected-results")
    expect(preview).to_be_visible()
    expect(preview.locator(".expected-results-head")).to_contain_text("archive-main")
    expect(preview.locator(".expected-results-head")).to_contain_text("2~191개 결과 폴더")
    expect(preview.locator('.expected-result-row[data-folder-id="event-a"] code')).to_have_text("2026-10-30_event-a__XXXXXXXXXXXX/")
    expect(preview.locator('.expected-result-row[data-folder-id="event-a"] small')).to_contain_text("2026-10-30_C0001__XXXXXXXXXXXX/")
    expect(preview.locator('.expected-result-row[data-folder-id="source-a"] code')).to_have_text("2026-10-30_source-a__XXXXXXXXXXXX/")
    expect(preview.locator(".expected-result-files")).to_contain_text("video-info.csv")
    expect(preview.locator(".expected-result-files")).to_contain_text("transcript.txt")
    expect(preview.locator(".expected-result-files")).to_contain_text("subtitle.srt")
    expect(preview.locator(".expected-result-files")).to_contain_text("archive-catalog.csv")
    expect(preview.locator(".expected-capacity")).to_have_text("예상 70 GB안전 여유 3.5 GB필요 74 GB현재 여유 860 GB")
    visible = preview.inner_text()
    if any(value in visible for value in ("C:\\", "/Volumes/", "storage_root")):
        raise AssertionError(f"expected output preview exposed an absolute root: {visible}")
    browser.close()
