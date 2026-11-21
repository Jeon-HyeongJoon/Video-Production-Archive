import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/jobs/JOB-RUNNING", wait_until="networkidle")
    disclosure = page.locator('#job-steps .step-item[data-step="render"] .progress-evidence')
    expect(disclosure.locator("summary")).to_have_text("실행 근거 2")
    expect(disclosure.locator("li")).to_have_count(2)
    expect(disclosure.locator("li").first).not_to_be_visible()
    disclosure.locator("summary").click()
    expect(disclosure.locator("li").first).to_contain_text("2개 계획 · 전체 0:10 · 청크 계획")
    expect(disclosure.locator("li").nth(1)).to_contain_text("1/2 · 0:00–0:05 · 처리 완료")
    rendered = disclosure.inner_text()
    if any(secret in rendered for secret in ("stdout", "stderr", "C:\\", "/private/", "credential")):
        raise AssertionError(f"unsafe process data leaked into progress disclosure: {rendered!r}")
    browser.close()
