import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    # Search starts from the box in the top bar on any screen.
    page.goto(sys.argv[1] + "/#/status", wait_until="networkidle")
    expect(page.locator('#menu [data-menu="search"]')).to_have_count(0)
    page.locator("#search-text").fill("가을")
    page.locator("#search-text").press("Enter")
    expect(page).to_have_url(sys.argv[1] + "/#/search?q=%EA%B0%80%EC%9D%84")
    results = page.locator("#search-results .result-row")
    expect(results).to_have_count(1)
    expect(results.first).to_contain_text("가을 행사")
    expect(page.locator('#search-scopes [data-scope="assets"] .num')).to_have_text("1")
    expect(page.locator('#search-scopes [data-scope="text"] .num')).to_have_text("0")

    detail = page.locator("#record-detail")
    expect(detail).to_contain_text("clip.mov")
    expect(detail).to_contain_text("42 B")
    expect(detail).to_contain_text("전체 행사 기록")
    expect(detail.locator(".keyword-list .ref-chip")).to_have_text(["강연"])
    expect(detail).to_contain_text("담당자 작성")
    if "camera/" in page.locator("body").inner_text():
        raise AssertionError("search screen exposed a storage path")

    page.locator("#search-text").fill("없는 말")
    page.locator("#search-text").press("Enter")
    expect(results).to_have_count(0)
    expect(page.locator("#search-results .empty")).to_have_text("결과 없음")
    browser.close()
