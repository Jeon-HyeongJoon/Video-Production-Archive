import sys

from playwright.sync_api import sync_playwright


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: check_archive_workspace_archive_name.py URL")
    base_url = sys.argv[1]
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(headless=True)
        page = browser.new_page(viewport={"width": 1440, "height": 960})
        page.goto(f"{base_url}/#/drives/VOLUME-1")
        row = page.locator('.folder-row[data-folder-id="ready"]')
        row.wait_for()
        row.click()
        detail = page.locator('.folder-detail[data-folder-id="ready"] .detail-meta')
        detail.wait_for()
        visible = detail.inner_text()
        trace = detail.get_attribute("title") or ""
        if visible != "2026-10-30 · ready.mp4":
            raise AssertionError(f"human archive result is not primary: {visible!r}")
        if "VP-20261030-READY" in visible or "JOB-READY" in visible:
            raise AssertionError(f"opaque ID leaked into the primary result: {visible!r}")
        if "VP-20261030-READY" not in trace or "JOB-READY" not in trace:
            raise AssertionError(f"trace IDs were not retained as secondary information: {trace!r}")

        response = page.request.get(f"{base_url}/api/v1/archive-intake")
        payload = response.json()
        drive = next(item for item in payload["data"] if item["workspace_drive_id"] == "VOLUME-1")
        archived = next(item for item in drive["folders"] if item["folder"] == "ready")
        if archived.get("archive_date") != "2026-10-30" or archived.get("archive_video_file_name") != "ready.mp4":
            raise AssertionError(f"archive presentation projection is incomplete: {archived!r}")
        if "C:\\" in str(archived):
            raise AssertionError(f"storage root leaked into archive presentation: {archived!r}")
        browser.close()


if __name__ == "__main__":
    main()
