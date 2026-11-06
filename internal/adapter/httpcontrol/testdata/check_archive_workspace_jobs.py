import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    # Every persisted Job is one scrollable workspace navigation card. The
    # card exposes identity and context without inventing another aggregate.
    workspaces = page.locator("#tree .workspace-card")
    expect(workspaces).to_have_count(4)
    running = page.locator('#tree .workspace-card[data-job-id="JOB-RUNNING"]')
    expect(running).to_contain_text("JOB-RUNNING")
    expect(running).to_contain_text("드라이브 없음")
    expect(running).to_contain_text("worker-main")
    expect(running.locator(".workspace-state")).to_have_text("실행 중")
    source = page.locator('#tree .workspace-card[data-job-id="JOB-SOURCE"]')
    expect(source).to_contain_text("JOB-SOURCE")
    expect(source).to_contain_text("VOLUME-1")
    expect(source).to_contain_text("WORKER-1")
    expect(running).to_have_attribute("href", "#/jobs/JOB-RUNNING")
    running.click()
    expect(page).to_have_url(sys.argv[1] + "/#/jobs/JOB-RUNNING")
    expect(page.locator(".page-head .token")).to_have_text("실행 중 · RUNNING")
    expect(page.locator('#job-steps .stage-row[data-step="render"] .stage-state')).to_have_text("RUNNING · 64% · worker-main")
    page.locator("#cancel-job").click()
    expect(page.locator("#cancel-job-confirm")).to_be_visible()

    # The same persistent Job appears in 작업 관리 확인 필요 and opens its detail.
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")
    row = page.locator('#attention-jobs .job-row[data-job-id="JOB-REVIEW"]')
    expect(row).to_contain_text("승인 대기")
    row.click()
    expect(page).to_have_url(sys.argv[1] + "/#/jobs/JOB-REVIEW")
    expect(page.locator('#job-steps .stage-row[data-step="approve-summary"] .stage-state')).to_have_text("WAITING · APPROVAL · 82%")
    expect(page.locator('#job-steps .stage-row[data-step="approve-summary"]')).to_have_attribute("data-signal", "decide")
    browser.close()
