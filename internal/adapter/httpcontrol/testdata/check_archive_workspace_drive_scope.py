import sys

from playwright.sync_api import expect, sync_playwright


def one_of_many(route):
    response = route.fetch()
    payload = response.json()
    drive = payload["data"][0]
    stored = next(folder for folder in drive["folders"] if folder.get("archive_state"))
    drive["folders"] = [stored]
    drive["folder_count"] = 1
    drive["discovered_folder_count"] = 221
    drive["discovered_file_count"] = 395
    drive["archive_run_state"] = "COMPLETED"
    route.fulfill(response=response, json=payload)


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.route("**/api/v1/archive-intake", one_of_many)
    page.goto(sys.argv[1] + "/#/connections", wait_until="networkidle")

    row = page.locator('#connection-drives [data-drive-id="VOLUME-1"]')
    expect(row).to_contain_text("최근 작업 완료")
    expect(row).to_contain_text("보관 1 / 1 폴더 · 발견 221 폴더")
    expect(row).not_to_contain_text("보관 진행 100%")

    row.click()
    overview = page.locator("#drive-overview")
    expect(overview).to_contain_text("최근 작업 완료")
    expect(overview.locator('[data-fact="progress"]')).to_contain_text("최근 작업")
    expect(overview.locator('[data-fact="progress"]')).to_contain_text("1 / 1 폴더")
    expect(overview.locator('[data-fact="progress"]')).to_contain_text("발견 221 폴더")
    expect(overview.locator(".overview-meter")).to_have_count(0)
    expect(overview).not_to_contain_text("100%")
    browser.close()
