import re
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1], wait_until="networkidle")

    links = page.locator("#menu .menu-link")
    expect(links).to_have_count(2)
    expect(links.locator(".menu-label")).to_have_text(["작업", "장비·에이전트"])
    expect(links.first).to_have_attribute("data-menu", "jobs")
    expect(links.first).to_have_attribute("aria-current", "page")

    # One multi-folder ArchiveRun is a single user-facing work item. Its
    # internal folder Jobs are not flattened beside the standalone Job.
    expect(page.locator("#active-jobs .section-title")).to_have_text("진행 중 2")
    archive_run = page.locator('#active-jobs .work-row[data-work-id="ARCHIVE-RUN-1"]')
    expect(archive_run).to_be_visible()
    # The row names the drive, its first folder and the rest as a count; the
    # work ID stays faint beside them.
    expect(archive_run.locator(".job-name b")).to_have_text("VOLUME-1")
    expect(archive_run.locator(".work-scope")).to_contain_text("broken 외 7개 · 영상 795")
    expect(archive_run.locator(".faint-id")).to_have_text("ARCHIVE-RUN-1")
    # A row opens into its folders; the work screen is one link further.
    archive_run.click()
    expect(archive_run).to_have_attribute("aria-expanded", "true")
    block = page.locator('#active-jobs .work-block[data-work-id="ARCHIVE-RUN-1"]')
    expect(block.locator(".child-row")).to_have_count(8)
    expect(block.locator(".children-head a")).to_have_attribute("href", "#/runs/ARCHIVE-RUN-1")
    archive_run.click()
    expect(archive_run).to_have_attribute("aria-expanded", "false")
    expect(page.locator('#active-jobs .work-row[data-work-id="JOB-RUNNING"]')).to_be_visible()
    expect(page.locator('#active-jobs .work-row[data-work-id="JOB-REVIEW"]')).to_have_count(0)

    # Queue admission is visible as its own operational list instead of being
    # mixed into running work. It is still a projection of persisted Job/Step.
    queue = page.locator("#queued-jobs")
    expect(queue.locator(".section-title")).to_have_text("시작 대기 1")
    queued = queue.locator('.work-row[data-work-id="JOB-QUEUED"]')
    expect(queued).to_be_visible()
    expect(queued.locator(".job-state")).to_have_text("시작 대기")
    expect(page.locator('#active-jobs .work-row[data-work-id="JOB-QUEUED"]')).to_have_count(0)

    # Job failures and human waits are the collapsible attention list. Folder,
    # Worker and Host problems stay in alerts and their owning detail screens.
    attention = page.locator("#attention-jobs")
    toggle = attention.locator(".lane-toggle")
    expect(attention).to_have_attribute("data-open", "true")
    expect(toggle).to_have_text("직접 확인할 항목 1")
    expect(attention.locator('.work-row[data-work-id="JOB-REVIEW"]')).to_be_visible()
    expect(attention.locator('.work-row[data-work-id="JOB-REVIEW"] .job-state')).to_have_text("직접 확인 필요")
    expect(attention.locator(".work-row")).to_have_count(1)
    toggle.click()
    expect(attention).to_have_attribute("data-open", "false")
    expect(attention.locator(".work-row")).to_have_count(0)
    toggle.click()
    expect(attention.locator('.work-row[data-work-id="JOB-REVIEW"]')).to_be_visible()

    # Progress shows only on running rows; the Worker waits on each row's hover.
    expect(page.locator("#active-jobs .work-row .meter")).to_have_count(2)
    expect(page.locator("#queued-jobs .meter, #attention-jobs .meter, #completed-jobs .meter")).to_have_count(0)
    expect(archive_run).to_have_attribute("title", re.compile("Worker WORKER-1"))

    # Finished work starts folded; its count stays on the summary.
    expect(page.locator("#completed-jobs")).to_have_attribute("data-open", "false")
    # Search and the bell sit on top of the sidebar and settings at its foot;
    # there is no top bar, and the theme choice lives inside settings.
    expect(page.locator(".sidebar .side-tools #search-text")).to_be_visible()
    expect(page.locator(".sidebar .side-tools #alert-button")).to_be_visible()
    expect(page.locator(".sidebar .side-foot #open-settings")).to_be_visible()
    expect(page.locator(".topbar, #theme-switch")).to_have_count(0)
    browser.close()
