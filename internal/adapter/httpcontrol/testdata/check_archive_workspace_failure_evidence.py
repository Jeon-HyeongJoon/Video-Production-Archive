import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="dark", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/jobs/JOB-RUNNING", wait_until="networkidle")
    disclosure = page.locator('#job-steps .step-item[data-step="render"] .failure-evidence')
    expect(disclosure.locator("summary")).to_have_text("실패 근거 2 · 상세 1")
    expect(disclosure.locator("li")).to_have_count(2)
    expect(disclosure.locator(".failure-tail").first).not_to_be_visible()
    collapsed = page.locator('#job-steps .step-item[data-step="render"]').inner_text()
    if "unsupported codec profile" in collapsed or "evidence://process-log" in collapsed:
        raise AssertionError(f"failure detail leaked before disclosure: {collapsed!r}")
    disclosure.locator("summary").click()
    expect(disclosure.locator("li").first).to_contain_text("EVD-WORKER-AVAILABLE")
    expect(disclosure.locator("li").first).to_contain_text("RENDER_RECORDING")
    expect(disclosure.locator("li").first).to_contain_text("22 · AVAILABLE")
    expect(disclosure.locator(".failure-tail").first).to_have_text("unsupported codec profile")
    expect(disclosure.locator("li").nth(1)).to_contain_text("LEGACY_UNRESOLVABLE")
    expect(disclosure.locator("li").nth(1)).to_contain_text("기록되지 않음")
    rendered = disclosure.inner_text()
    if any(secret in rendered for secret in ("C:\\", "/private/", "credential", "secret-value")):
        raise AssertionError(f"unsafe failure detail leaked: {rendered!r}")
    browser.close()
