import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 960}, locale="ko-KR")
    base_url = sys.argv[1]
    page.goto(base_url + "/#/drives/VOLUME-1", wait_until="networkidle")

    completed = page.locator("#completed-work")
    expect(completed).to_be_visible()
    expect(completed.locator(".section-title")).to_contain_text("완료 내역 7")

    source = completed.locator('[data-package-id="VP-20261030-SOURCE"]')
    expect(source.locator(".completed-result b")).to_have_text("source.mp4")
    expect(source.locator(".completed-kind")).to_contain_text("소스 시퀀스")
    expect(source.locator(".completed-source")).to_have_text("86 → 1")
    expect(source.locator(".completed-place")).to_contain_text("확정됨")
    expect(source.locator(".completed-place")).to_contain_text("archive-main")
    expect(source.locator(".archive-relative-path")).to_have_text("packages/2026/10/30/VP-20261030-SOURCE/revisions/000001")
    trace = source.get_attribute("title") or ""
    if "VP-20261030-SOURCE" not in trace or "JOB-SOURCE" not in trace:
        raise AssertionError(f"completed result lost trace IDs: {trace!r}")

    older = completed.locator('[data-package-id="VP-20261029-LECTURE"]')
    expect(older.locator(".completed-result b")).to_have_text("lecture.mp4")
    expect(older).to_contain_text("보관됨")

    response = page.request.get(base_url + "/api/v1/archive-intake")
    drive = next(item for item in response.json()["data"] if item["workspace_drive_id"] == "VOLUME-1")
    history = drive.get("completed_work") or []
    if len(history) != 7:
        raise AssertionError(f"completed work projection is incomplete: {history!r}")
    if not any(item["archive_run_id"] == "ARCHIVE-RUN-0" for item in history):
        raise AssertionError(f"older ArchiveRun result disappeared: {history!r}")
    if any(item.get("folder") in {"hold", "broken"} for item in history):
        raise AssertionError(f"non-result folder appeared as completed work: {history!r}")
    if not all(item.get("archive_relative_path", "").startswith("packages/") for item in history):
        raise AssertionError(f"completed work lost package-relative locations: {history!r}")
    if "C:\\" in str(history) or "/Volumes/" in str(history):
        raise AssertionError(f"storage root leaked into completed work: {history!r}")
    browser.close()
