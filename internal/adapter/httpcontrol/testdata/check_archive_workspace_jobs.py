import re
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/workers", wait_until="networkidle")
    # Jobs that belong to no drive stay their own unit under the Worker running
    # them; these two run on no registered Worker.
    expect(page.locator("#workers-title")).to_have_attribute("title", re.compile("드라이브 밖 작업 2"))
    items = page.locator("#other-jobs .job-row")
    expect(items).to_have_count(2)
    running = page.locator('#other-jobs .job-row[data-job-id="JOB-RUNNING"]')
    expect(running).to_have_attribute("data-lane", "work")
    expect(running).to_contain_text("가을 행사 편집")
    expect(running).to_contain_text("occasion-recording")
    expect(running).to_contain_text("worker-main")
    expect(running.locator(".job-state")).to_have_text("실행 중")
    expect(running.locator(".job-progress")).to_have_text("64%")
    waiting = page.locator('#other-jobs .job-row[data-job-id="JOB-REVIEW"]')
    expect(waiting).to_have_attribute("data-lane", "decide")
    expect(waiting).to_contain_text("인터뷰 요약 검수")
    expect(waiting.locator(".job-state")).to_have_text("승인 대기")
    expect(waiting.locator(".job-state")).to_have_attribute("data-signal", "decide")
    expect(page.locator('#worker-list .job-row[data-job-id]')).to_have_count(0)

    running.click()
    detail = page.locator('#job-detail[data-job-id="JOB-RUNNING"]')
    expect(detail.locator(".detail-head .token")).to_have_text("실행 중 · RUNNING")
    expect(detail.locator(".stage-row")).to_have_count(0)
    detail.locator(".steps-toggle").click()
    expect(detail.locator('.stage-row[data-step="render"] .stage-state')).to_have_text("RUNNING · 64% · worker-main")
    detail.locator("#cancel-job").click()
    expect(detail.locator("#cancel-job-confirm")).to_be_visible()

    # The human wait is a problem on 상태 and leads back to this Job.
    page.goto(sys.argv[1] + "/#/status", wait_until="networkidle")
    row = page.locator('#problem-cards .problem-card[data-kind="job"][data-job-id="JOB-REVIEW"]')
    expect(row).to_contain_text("작업 승인 대기")
    row.click()
    expect(page).to_have_url(sys.argv[1] + "/#/workers?job=JOB-REVIEW")
    detail = page.locator('#job-detail[data-job-id="JOB-REVIEW"]')
    detail.locator(".steps-toggle").click()
    expect(detail.locator('.stage-row[data-step="approve-summary"] .stage-state')).to_have_text("WAITING · APPROVAL · 82%")
    expect(detail.locator('.stage-row[data-step="approve-summary"]')).to_have_attribute("data-signal", "decide")
    browser.close()
