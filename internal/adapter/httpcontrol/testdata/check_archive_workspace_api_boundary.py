import sys
from urllib.parse import urlparse

from playwright.sync_api import sync_playwright


base_url = sys.argv[1].rstrip("/")

with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    context = browser.new_context(locale="ko-KR", timezone_id="Asia/Seoul")
    page = context.new_page()
    backend_requests = []

    def remember(request):
        if request.resource_type in {"fetch", "xhr"}:
            backend_requests.append(urlparse(request.url).path)

    page.on("request", remember)
    page.goto(f"{base_url}/#/jobs", wait_until="networkidle")
    page.locator("#view[data-screen='jobs']").wait_for()

    root = context.request.get(f"{base_url}/")
    source = root.text()
    if root.status != 200:
        raise AssertionError(f"workspace shell returned {root.status}")
    if '/assets/js/main.js' not in source or 'id="app"' not in source:
        raise AssertionError("component entry or static mount point missing")
    forbidden = ["ARCHIVE-RUN-1", "JOB-RUNNING", "window.__", "application/json", "/assets/js/flow.js"]
    leaked = [value for value in forbidden if value in source]
    if leaked:
        raise AssertionError(f"server-rendered state or legacy frontend leaked into shell: {leaked}")

    if not backend_requests:
        raise AssertionError("component frontend made no backend API requests")
    outside_api = [path for path in backend_requests if not path.startswith("/api/v1/")]
    if outside_api:
        raise AssertionError(f"frontend used a non-API backend path: {outside_api}")
    required = {
        "/api/v1/workers",
        "/api/v1/archive-intake",
        "/api/v1/archivist-hosts",
        "/api/v1/archivist-sessions",
        "/api/v1/jobs",
        "/api/v1/work",
    }
    missing = sorted(required.difference(backend_requests))
    if missing:
        raise AssertionError(f"workspace did not load required API projections: {missing}")

    legacy = context.request.get(f"{base_url}/assets/js/flow.js")
    if legacy.status != 404:
        raise AssertionError(f"legacy frontend module still served: {legacy.status}")

    browser.close()
