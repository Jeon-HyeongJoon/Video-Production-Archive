import re
import sys

from playwright.sync_api import expect, sync_playwright


def prepared(page, drive):
    page.goto(sys.argv[1] + f"/#/drives/{drive}", wait_until="networkidle")
    page.locator("#prepare-run").click()
    expect(page.locator("#drive-prep")).to_be_visible()


scene = sys.argv[2]
with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    if scene == "pre-run":
        # The drive fixes its Worker; only that Worker's archive candidates appear.
        prepared(page, "VOLUME-1")
        archives = page.locator("#prep-archives")
        expect(archives.locator(".option")).to_have_count(5)
        expect(archives.locator('[data-storage-id="archive-main"]')).to_contain_text("로컬 저장소")
        expect(archives.locator('[data-storage-id="archive-nas"]')).to_contain_text("네트워크 드라이브")
        expect(archives.locator('[data-storage-id="archive-new-drive"]')).to_contain_text("새 드라이브")
        expect(archives.locator('[data-storage-id="archive-ftp"]')).to_contain_text("FTP")
        expect(archives.locator('[data-storage-id="archive-sftp"]')).to_contain_text("SFTP")
        expect(archives.locator('[data-storage-id="archive-ftp"]')).to_be_disabled()
        expect(archives.locator('[data-storage-id="archive-sftp"]')).to_be_disabled()
        expect(archives.locator('[data-storage-id="source-main"], [data-storage-id="source-review"]')).to_have_count(0)
        # Nothing is chosen until the person picks it.
        expect(archives.locator(".option[aria-pressed=true]")).to_have_count(0)
        expect(page.locator("#prep-folders input[type=checkbox]:not(#select-all)")).to_have_count(4)
        expect(page.locator("#selection-summary")).to_have_text("0 폴더 · 0 영상 · 0 GB")
        expect(page.locator("#start-reason")).to_have_text("폴더 선택 없음")
        page.locator('#prep-folders [data-folder-id="event-a"] input[type="checkbox"]').check()
        expect(page.locator("#selection-summary")).to_have_text("1 폴더 · 105 영상 · 42 GB")
        expect(page.locator("#start-reason")).to_have_text("아카이브 선택 없음")
        archives.locator('[data-storage-id="archive-main"]').click()
        expect(archives.locator('[data-storage-id="archive-main"]')).to_have_attribute("aria-pressed", "true")
        expect(page.locator("#start-archive-run")).to_be_enabled()
        expect(page.locator("#selection-chips .selection-chip")).to_have_text(["WORKER-1", "VOLUME-1", "source-main", "archive-main", "1 폴더"])

        # A completed drive on a Worker that is off can be prepared but not started.
        prepared(page, "VOLUME-2")
        expect(page.locator("#prep-archives .option")).to_have_count(1)
        expect(page.locator('#prep-archives [data-storage-id="archive-nas"]')).to_have_attribute("aria-pressed", "false")
        expect(page.locator("#prep-folders input[type=checkbox]:not(#select-all)")).to_have_count(2)
        page.locator("#select-all").check()
        expect(page.locator("#selection-summary")).to_have_text("2 폴더 · 386 영상 · 42 GB")
        expect(page.locator("#start-reason")).to_have_text("아카이브 선택 없음")
        page.locator('#prep-folders [data-folder-id="review-b"] input[type="checkbox"]').uncheck()
        expect(page.locator("#selection-summary")).to_have_text("1 폴더 · 64 영상 · 12 GB")
        page.locator('#prep-archives [data-storage-id="archive-nas"]').click()
        expect(page.locator("#start-reason")).to_have_text("Worker 꺼짐")
        expect(page.locator("#start-archive-run")).to_be_disabled()
        expect(page.locator("#selection-chips .selection-chip")).to_have_text(["WORKER-2", "VOLUME-2", "source-review", "archive-nas", "1 폴더"])
    else:
        # A running ArchiveRun keeps its archive and folder scope locked.
        page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
        expect(page.locator("#drive-folders .folder-row")).to_have_count(8)
        expect(page.locator("#prepare-run, #drive-prep, #start-archive-run")).to_have_count(0)
        expect(page.locator("input[type=checkbox]")).to_have_count(0)
        expect(page.locator("#drive-progress")).to_have_attribute("title", re.compile(re.escape("범위 r7 고정 · 8 폴더 · 795 영상 · 211 GB")))
        expect(page.locator("#drive-title")).to_have_attribute("title", re.compile(re.escape("→ archive-main")))
        expect(page.locator("#selection-chips .selection-chip")).to_have_text(["WORKER-1", "VOLUME-1", "source-main", "archive-main", "8 폴더"])
    browser.close()
