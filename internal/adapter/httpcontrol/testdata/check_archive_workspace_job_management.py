import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1], wait_until="networkidle")

    links = page.locator("#menu .menu-link")
    expect(links).to_have_count(2)
    expect(links.locator(".menu-label")).to_have_text(["작업 관리", "연결 관리"])
    expect(links.first).to_have_attribute("data-menu", "jobs")
    expect(links.first).to_have_attribute("aria-current", "page")

    # Running Jobs remain at top; queued/resource-waiting Jobs have their own queue.
    expect(page.locator("#active-jobs .section-title")).to_have_text("진행 중 1")
    expect(page.locator('#active-jobs .job-row[data-job-id="JOB-RUNNING"]')).to_be_visible()
    expect(page.locator('#active-jobs .job-row[data-job-id="JOB-REVIEW"]')).to_have_count(0)

    # Queue admission is visible as its own operational list instead of being
    # mixed into running work. It is still a projection of persisted Job/Step.
    queue = page.locator("#queued-jobs")
    expect(queue.locator(".section-title")).to_have_text("작업 큐 1")
    queued = queue.locator('.job-row[data-job-id="JOB-QUEUED"]')
    expect(queued).to_be_visible()
    expect(queued.locator(".job-state")).to_have_text("대기열")
    expect(page.locator('#active-jobs .job-row[data-job-id="JOB-QUEUED"]')).to_have_count(0)

    # Job failures and human waits are the collapsible attention list. Folder,
    # Worker and Host problems stay in alerts and their owning detail screens.
    attention = page.locator("#attention-jobs")
    expect(attention).to_have_attribute("open", "")
    expect(attention.locator("summary")).to_have_text("확인 필요1")
    expect(attention.locator('.job-row[data-job-id="JOB-REVIEW"]')).to_be_visible()
    expect(attention.locator('.job-row[data-job-id="JOB-REVIEW"] .job-state')).to_have_text("승인 대기")
    expect(attention.locator(".job-row")).to_have_count(1)
    attention.locator("summary").click()
    expect(attention).not_to_have_attribute("open", "")
    attention.locator("summary").click()
    expect(attention.locator('.job-row[data-job-id="JOB-REVIEW"]')).to_be_visible()

    expect(page.locator("#completed-jobs")).to_have_attribute("open", "")
    browser.close()
