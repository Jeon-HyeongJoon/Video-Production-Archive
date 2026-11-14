import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.goto(sys.argv[1] + "/#/connections", wait_until="networkidle")

    expect(page.locator('#menu [data-menu="connections"]')).to_have_attribute("aria-current", "page")
    expect(page.locator("#connections-title")).to_have_text("장비·에이전트")
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
    expect(primary).to_contain_text("연결됨")
    expect(primary).to_have_attribute("href", "#/drives/VOLUME-1")
    secondary = page.locator('#connection-drives [data-drive-id="VOLUME-2"]')
    expect(secondary).to_contain_text("확인 불가")

    # Every drive surface uses the same projected live state. A fresh Worker
    # observation without this volume changes all of them to detached on the
    # next five-second refresh; archive progress remains historical.
    def detach_source(route):
        response = route.fetch()
        body = response.json()
        body["data"] = [
            {**drive, "connection_state": "DISCONNECTED"} if drive["workspace_drive_id"] == "VOLUME-2" else drive
            for drive in body["data"]
        ]
        route.fulfill(response=response, json=body)

    page.route("**/api/v1/archive-intake", detach_source)
    secondary = page.locator('#connection-drives [data-drive-id="VOLUME-2"]')
    expect(secondary).to_contain_text("연결 해제됨", timeout=8_000)
    expect(page.locator('#alert-panel')).to_be_hidden()
    page.locator('#alert-button').click()
    expect(page.locator('#alert-panel')).to_contain_text("드라이브 연결 해제됨")
    page.locator('#alert-button').click()

    agents = page.locator("#connection-agents [data-session-id]")
    expect(agents).to_have_count(1)
    expect(agents.first).to_contain_text("Archive Archivist")
    expect(agents.first).to_contain_text("archivist-windows")
    expect(agents.first).to_contain_text("codex")
    hosts = page.locator("#connection-agents [data-host-id]")
    expect(hosts).to_have_count(2)
    expect(hosts.filter(has_text="archivist-mac")).to_contain_text("꺼짐")

    secondary.click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-2")
    expect(page.locator('#menu [data-menu="connections"]')).to_have_attribute("aria-current", "page")
    expect(page.locator('#drive-overview [data-fact="connection"]')).to_contain_text("연결 해제됨")
    expect(page.locator('#prepare-run, #drive-prep, #start-archive-run')).to_have_count(0)
    browser.close()
