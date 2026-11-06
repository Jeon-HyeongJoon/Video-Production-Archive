import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.goto(sys.argv[1] + "/#/connections", wait_until="networkidle")

    expect(page.locator('#menu [data-menu="connections"]')).to_have_attribute("aria-current", "page")
    expect(page.locator("#connections-title")).to_have_text("연결 관리")
    expect(page.locator("#connection-worker-count")).to_have_text("2")
    expect(page.locator("#connection-drive-count")).to_have_text("2")
    expect(page.locator("#connection-agent-count")).to_have_text("1")

    workers = page.locator("#connection-workers [data-worker-id]")
    expect(workers).to_have_count(2)
    expect(workers.filter(has_text="WORKER-1")).to_contain_text("처리 중")
    expect(workers.filter(has_text="WORKER-2")).to_contain_text("꺼짐")

    drives = page.locator("#connection-drives [data-drive-id]")
    expect(drives).to_have_count(2)
    primary = page.locator('#connection-drives [data-drive-id="VOLUME-1"]')
    expect(primary).to_contain_text("WORKER-1")
    expect(primary).to_have_attribute("href", "#/drives/VOLUME-1")

    agents = page.locator("#connection-agents [data-session-id]")
    expect(agents).to_have_count(1)
    expect(agents.first).to_contain_text("Archive Archivist")
    expect(agents.first).to_contain_text("archivist-windows")
    expect(agents.first).to_contain_text("codex")
    hosts = page.locator("#connection-agents [data-host-id]")
    expect(hosts).to_have_count(2)
    expect(hosts.filter(has_text="archivist-mac")).to_contain_text("꺼짐")

    primary.click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1")
    expect(page.locator('#menu [data-menu="connections"]')).to_have_attribute("aria-current", "page")
    browser.close()
