import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")

    # The run stays completed while the review recommendation asks for a person.
    expect(page.locator('#drive-overview [data-fact="state"]')).to_have_attribute("data-phase", "done")
    expect(page.locator('#drive-overview [data-fact="state"]')).to_contain_text("완료")
    ready = page.locator('#drive-folders .folder-row[data-folder-id="ready"]')
    expect(ready).to_have_attribute("data-signal", "decide")
    expect(ready.locator(".status-text")).to_have_text("전사 검토 권장")
    review = page.locator('#drive-problems .problem-panel[data-folder-id="ready"][data-kind="review"]')
    expect(review).to_contain_text("전사 검토 권장")
    review.locator(".problem-head").click()
    quality = review.locator(".transcript-quality")
    expect(quality).to_contain_text("TRANSCRIPT-ASSET-READY")
    expect(quality).to_contain_text("긴 구간 · 낮은 문자 밀도")
    expect(quality).to_contain_text("19.1초 · 1구간 · 9자")
    expect(page.locator("#alert-count")).to_have_text("6")
    page.locator('#menu [data-menu="status"]').click()
    expect(page.locator('#problem-cards .problem-card[data-kind="review"][data-folder-id="ready"]')).to_contain_text("전사 검토 권장")
    browser.close()
