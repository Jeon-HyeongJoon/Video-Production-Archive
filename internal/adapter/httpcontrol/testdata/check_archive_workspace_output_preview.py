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
    expect(preview.locator(".preview-root")).to_contain_text("archive-main/")
    expect(preview.locator(".preview-root")).to_contain_text("2~191개 결과 폴더")
    folder = preview.locator('.expected-result-row[data-folder-id="event-a"]')
    expect(folder.locator(".result-directory > b")).to_have_text("2026-10-30_event-a__XXXXXXXXXXXX/")
    expect(folder.locator(".result-directory code")).to_have_text("event-a.mp4")
    expect(folder).to_contain_text("video-info.csv")
    expect(folder).to_contain_text(".archive-package.json")
    expect(folder).to_contain_text("subtitle.srt · transcript.txt")
    expect(preview.locator(".catalog-node")).to_contain_text("archive-catalog.csv")
    expect(preview.locator(".expected-capacity")).to_have_text("예상 70 GB안전 여유 3.5 GB필요 74 GB현재 여유 860 GB")
    visible = preview.inner_text()
    if any(value in visible for value in ("C:\\", "/Volumes/", "storage_root")):
        raise AssertionError(f"expected output preview exposed an absolute root: {visible}")
    page.get_by_role("button", name="개별 영상일 때").click()
    expect(folder.locator(".result-directory > b")).to_have_text(["2026-10-30_C0001__XXXXXXXXXXXX/", "2026-10-30_C0002__XXXXXXXXXXXX/"])
    expect(folder.locator(".result-directory code")).to_have_text(["C0001.mp4", "C0002.mp4"])
    expect(folder).to_contain_text("이름 미확인")
    page.locator('#prep-folders [data-folder-id="event-a"] input').uncheck()
    expect(folder).to_have_count(0)
    page.locator('#prep-archives [data-storage-id="archive-main"]').click()
    expect(preview.locator(".preview-root")).to_contain_text("저장 위치 선택 전")
    browser.close()
