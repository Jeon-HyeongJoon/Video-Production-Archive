import re
import sys

from playwright.sync_api import expect, sync_playwright


def prepared(page, drive):
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    page.locator("#new-job-open").click()
    expect(page.locator('#new-work-kind [data-kind="ARCHIVE"]')).to_have_attribute("aria-pressed", "true")
    page.locator(f'#prep-drives [data-drive-id="{drive}"]').click()
    expect(page.locator("#archive-scope")).to_be_visible()


scene = sys.argv[2]
with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    if scene == "pre-run":
        starts = []
        def start_archive(route, request):
            starts.append(request.post_data_json)
            route.fulfill(status=201, content_type="application/json", body='{"data":{"archive_run_id":"ARCHIVE-RUN-CREATED","folder_count":1}}')
        page.route("**/api/v1/archive-intake/runs", start_archive)
        # Job Management fixes the source drive and therefore its Worker; only
        # that Worker's archive candidates appear.
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
        page.locator("#start-archive-run").click()
        expect(page).to_have_url(sys.argv[1] + "/#/runs/ARCHIVE-RUN-CREATED")
        if len(starts) != 1 or starts[0]["session_id"] != "INTAKE-1" or starts[0]["source_volume_id"] != "VOLUME-1" or starts[0]["archive_storage_id"] != "archive-main" or starts[0]["folder_ids"] != ["event-a"]:
            raise AssertionError(f"archive work create payload={starts}")

        # Drive detail is inventory/history only and never exposes creation.
        page.goto(sys.argv[1] + "/#/drives/VOLUME-2", wait_until="networkidle")
        expect(page.locator('#drive-overview [data-fact="connection"]')).to_contain_text("확인 불가")
        expect(page.locator("#prepare-run, #drive-prep, #start-archive-run, #archive-scope")).to_have_count(0)
    elif scene == "complete":
        # A failed folder is not retried in place: once its run is over, it goes
        # into a new work with its drive, the folder and the original archive chosen.
        page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
        broken = page.locator('#drive-problems .problem-panel[data-folder-id="broken"]')
        expect(broken).to_have_attribute("data-open", "true")
        broken.locator('[data-action="redo"]').click()
        expect(page).to_have_url(sys.argv[1] + "/#/jobs")
        expect(page.locator('#new-work-kind [data-kind="ARCHIVE"]')).to_have_attribute("aria-pressed", "true")
        expect(page.locator('#prep-drives [data-drive-id="VOLUME-1"]')).to_have_attribute("aria-pressed", "true")
        expect(page.locator('#prep-folders [data-folder-id="broken"] input[type="checkbox"]')).to_be_checked()
        expect(page.locator('#prep-folders input[type="checkbox"]:checked:not(#select-all)')).to_have_count(1)
        expect(page.locator('#prep-archives [data-storage-id="archive-main"]')).to_have_attribute("aria-pressed", "true")
        expect(page.locator("#selection-summary")).to_contain_text("1 폴더")
    else:
        # A failed folder of a running ArchiveRun waits for the run to end.
        page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
        expect(page.locator('#drive-problems .problem-panel[data-folder-id="broken"] [data-action="redo"]')).to_be_disabled()

        # A running ArchiveRun is not a source choice for another new work.
        page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
        page.locator("#new-job-open").click()
        expect(page.locator('#prep-drives [data-drive-id="VOLUME-1"]')).to_have_count(0)
        expect(page.locator("#archive-scope, #start-archive-run")).to_have_count(0)

        # Its drive detail retains records and fixed-scope history only.
        page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
        expect(page.locator("#drive-folders .folder-row")).to_have_count(8)
        expect(page.locator("#prepare-run, #drive-prep, #start-archive-run, #archive-scope")).to_have_count(0)
        expect(page.locator("input[type=checkbox]")).to_have_count(0)
        expect(page.locator("#drive-progress")).to_have_attribute("title", re.compile(re.escape("범위 r7 고정 · 8 폴더 · 795 영상 · 211 GB")))
        expect(page.locator("#drive-title")).to_have_attribute("title", re.compile(re.escape("→ archive-main")))
        expect(page.locator("#selection-chips .selection-chip")).to_have_text(["아카이브 실행 · ARCHIVE-RUN-1 · r1", "WORKER-1", "VOLUME-1", "source-main", "archive-main", "8 폴더"])
        # The references stay folded behind one row until a person opens it.
        expect(page.locator("#pane-refs")).not_to_have_attribute("open", "")
        expect(page.locator("#pane-refs > summary")).to_have_text("참조 6")
    browser.close()
