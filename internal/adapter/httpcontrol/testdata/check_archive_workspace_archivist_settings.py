import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1], wait_until="networkidle")
    expect(page.locator('#archivist-pane .pane-head a[aria-label="Agent 추가 · 설정"]')).to_have_count(0)
    page.locator("#open-settings").click()
    expect(page.locator("#archivist-add")).to_be_visible()
    expect(page.locator("#archivist-list .archivist-row")).to_have_count(1)
    expect(page.locator('#archivist-list [data-action="archivist-remove"]')).to_have_count(1)
    expect(page.locator('#archivist-list [data-action="archivist-remove"]')).to_have_text("제거")
    expect(page.locator("#archivist-add input, #archivist-add select")).to_have_count(3)
    expect(page.locator('label:has(#archivist-host-select) span')).to_have_text("실행 PC")
    expect(page.locator("#archivist-host-select option")).to_have_text(["archivist-windows · 연결됨", "archivist-mac · 꺼짐"])
    expect(page.locator("#archivist-cli-select option")).to_have_text(["claude", "codex"])
    expect(page.locator("#archivist-add-submit")).to_be_enabled()
    page.locator("#archivist-host-select").select_option("archivist-mac")
    expect(page.locator("#archivist-cli-select option")).to_have_text(["사용 가능한 실행 도구 없음"])
    expect(page.locator("#archivist-cli-select")).to_be_disabled()
    expect(page.locator("#archivist-add-submit")).to_be_disabled()
    forbidden = page.locator('#archivist-add [name="backend"], #archivist-add [name="workspace_root_id"], #archivist-add [name="job_id"], #archivist-add [name="control_mode"]')
    expect(forbidden).to_have_count(0)
    browser.close()
