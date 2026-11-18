import sys

from playwright.sync_api import expect, sync_playwright


UNRESOLVED = "ARCHIVE-RUN-OLD-UNRESOLVED"
RESOLVED = "ARCHIVE-RUN-OLD-RESOLVED"


def work_projection(route):
    response = route.fetch()
    payload = response.json()
    payload["data"].extend([
        {
            "work_id": UNRESOLVED,
            "kind": "ARCHIVE_RUN",
            "state": "FAILED",
            "created_at": "2026-10-28T09:00:00Z",
            "last_event_at": "2026-10-28T09:05:00Z",
            "worker_id": "WORKER-2",
            "source_volume_id": "VOLUME-2",
            "source_storage_id": "source-review",
            "archive_storage_id": "archive-nas",
            "folder_count": 1,
            "child_job_count": 2,
            "progress": 0.5,
        },
        {
            "work_id": RESOLVED,
            "kind": "ARCHIVE_RUN",
            "state": "FAILED",
            "replacement_work_id": "ARCHIVE-RUN-2",
            "created_at": "2026-10-27T09:00:00Z",
            "last_event_at": "2026-10-27T09:05:00Z",
            "worker_id": "WORKER-2",
            "source_volume_id": "VOLUME-2",
            "source_storage_id": "source-review",
            "archive_storage_id": "archive-nas",
            "folder_count": 1,
            "child_job_count": 2,
            "progress": 0.5,
        },
    ])
    route.fulfill(response=response, json=payload)


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.route("**/api/v1/work", work_projection)
    page.goto(sys.argv[1] + "/#/jobs", wait_until="networkidle")

    # The unresolved old run remains an actionable row in Task Management,
    # but a failure does not occupy the short live workspace.
    attention = page.locator("#attention-jobs")
    expect(attention.locator(f'[data-work-id="{UNRESOLVED}"]')).to_have_count(2)
    expect(page.locator(f'#tree [data-work-id="{UNRESOLVED}"]')).to_have_count(0)
    page.locator("#alert-button").click()
    expect(page.locator(f'#alert-panel [data-key="work:{UNRESOLVED}"]')).to_be_visible()

    # A failed run whose folders were handed to a later run is history, not a
    # remaining task. Its failed outcome is preserved behind the completed row.
    expect(page.locator(f'#attention-jobs [data-work-id="{RESOLVED}"]')).to_have_count(0)
    expect(page.locator(f'#tree [data-work-id="{RESOLVED}"]')).to_have_count(0)
    expect(page.locator(f'#alert-panel [data-key="work:{RESOLVED}"]')).to_have_count(0)
    page.locator("#alert-button").click()
    page.locator("#completed-jobs .lane-toggle").click()
    resolved = page.locator(f'#completed-jobs .work-row[data-work-id="{RESOLVED}"]')
    expect(resolved).to_be_visible()
    expect(resolved.locator(".job-state")).to_have_text("새 작업으로 넘김")
    browser.close()
