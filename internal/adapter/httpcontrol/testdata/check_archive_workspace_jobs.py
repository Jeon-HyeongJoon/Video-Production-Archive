import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    # The sidebar indexes only unfinished user-facing work. The completed
    # ArchiveRun stays in Task Management instead of occupying the workspace.
    workspaces = page.locator("#tree .workspace-card")
    expect(workspaces).to_have_count(3)
    archive_run = page.locator('#tree .workspace-card[data-work-id="ARCHIVE-RUN-1"]')
    expect(archive_run).to_have_count(0)
    page.locator("#completed-jobs .lane-toggle").click()
    completed = page.locator('#completed-jobs .work-row[data-work-id="ARCHIVE-RUN-1"]')
    expect(completed).to_have_count(1)
    completed.click()
    expect(page.locator('#completed-jobs .work-block[data-work-id="ARCHIVE-RUN-1"] .children-head a')).to_have_attribute("href", "#/runs/ARCHIVE-RUN-1")
    # A card shows the work's name and state; job_id, drive and Worker wait on hover.
    running = page.locator('#tree .workspace-card[data-work-id="JOB-RUNNING"]')
    expect(running).to_have_attribute("title", "행사 하이라이트 · JOB-RUNNING · 드라이브 없음 · WORKER-1")
    expect(running.locator(".workspace-work-name")).to_have_text("행사 하이라이트")
    expect(running.locator(".workspace-state")).to_have_text("처리 중")
    if any(value in running.inner_text() for value in ("JOB-RUNNING", "드라이브 없음", "WORKER-1")):
        raise AssertionError(f"workspace card shows hover-only facts: {running.inner_text()!r}")
    expect(running).to_have_attribute("href", "#/jobs/JOB-RUNNING")
    running.click()
    expect(page).to_have_url(sys.argv[1] + "/#/jobs/JOB-RUNNING")
    expect(page.locator(".page-head .job-head-state")).to_have_text("처리 중")
    expect(page.locator(".page-head .job-head-state")).to_have_attribute("title", "RUNNING")
    expect(page.locator('#job-steps .step-item[data-step="render"] .stage-state')).to_have_text("RUNNING · 64% · worker-main")
    page.locator("#cancel-job").click()
    expect(page.locator("#cancel-job-confirm")).to_be_visible()

    # The same persistent Job appears in 작업의 직접 확인할 항목 list and opens its detail.
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    row = page.locator('#attention-jobs .work-row[data-work-id="JOB-REVIEW"]')
    expect(row).to_contain_text("직접 확인 필요")
    row.click()
    page.locator('#attention-jobs .work-block[data-work-id="JOB-REVIEW"] .children-head a').click()
    expect(page).to_have_url(sys.argv[1] + "/#/jobs/JOB-REVIEW")
    expect(page.locator('#job-steps .step-item[data-step="approve-summary"] .stage-state')).to_have_text("WAITING · APPROVAL · 82%")
    expect(page.locator('#job-steps .step-item[data-step="approve-summary"]')).to_have_attribute("data-signal", "decide")
    browser.close()
