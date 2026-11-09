import re
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="dark", locale="ko-KR")
    base_url = sys.argv[1]
    created = []
    page.on("request", lambda request: created.append(request) if request.method == "POST" and request.url.endswith("/api/v1/jobs") else None)

    page.goto(base_url + "/#/jobs", wait_until="networkidle")
    expect(page.locator("#new-job-form")).to_have_count(0)
    page.locator("#new-job-open").click()
    expect(page.locator("#new-job-open")).to_have_attribute("aria-expanded", "true")
    page.locator('#new-work-kind [data-kind="TEMPLATE"]').click()
    expect(page.locator("#new-job-template option")).to_have_count(4)
    page.locator("#new-job-title").fill("2026 가을 행사 기록")
    page.locator("#new-job-template").select_option("occasion-recording")
    page.locator("#new-job-submit").click()

    expect(page).to_have_url(re.compile(re.escape(base_url) + r"/#/jobs/JOB-[0-9a-f-]+"))
    expect(page.locator("#job-title")).to_have_text("2026 가을 행사 기록")
    summary = page.locator("#job-summary")
    expect(summary).to_contain_text("occasion-recording")
    expect(page.locator("#job-steps .stage-row")).not_to_have_count(0)
    if len(created) != 1:
        raise AssertionError(f"job create requests={len(created)}")

    page.goto(base_url + "/#/jobs", wait_until="networkidle")
    expect(page.locator('.work-row[data-kind="JOB"]', has_text="2026 가을 행사 기록")).to_have_count(1)
    browser.close()
