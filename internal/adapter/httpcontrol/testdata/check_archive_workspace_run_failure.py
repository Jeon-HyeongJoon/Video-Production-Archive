import sys

from playwright.sync_api import expect, sync_playwright


RUN_ID = "ARCHIVE-RUN-FAILURE-EVIDENCE"


def work_list(route):
    response = route.fetch()
    payload = response.json()
    payload["data"].append({
        "work_id": RUN_ID,
        "kind": "ARCHIVE_RUN",
        "state": "FAILED",
        "created_at": "2026-11-01T09:00:00Z",
        "last_event_at": "2026-11-01T09:10:00Z",
        "worker_id": "WORKER-1",
        "source_volume_id": "VOLUME-1",
        "source_storage_id": "source-main",
        "archive_storage_id": "archive-main",
        "folder_count": 1,
        "video_count": 2,
        "child_job_count": 2,
        "progress": 0.5,
    })
    route.fulfill(response=response, json=payload)


def work_detail(route):
    route.fulfill(json={"data": {
        "summary": {
            "work_id": RUN_ID,
            "kind": "ARCHIVE_RUN",
            "state": "FAILED",
            "created_at": "2026-11-01T09:00:00Z",
            "last_event_at": "2026-11-01T09:10:00Z",
            "worker_id": "WORKER-1",
            "source_volume_id": "VOLUME-1",
            "source_storage_id": "source-main",
            "archive_storage_id": "archive-main",
            "folder_count": 1,
            "video_count": 2,
            "child_job_count": 2,
            "progress": 0.5,
        },
        "folder_ids": ["broken"],
        "children": [],
        "completed_work": [],
        "failures": [{
            "job_id": "JOB-BROKEN-EVIDENCE",
            "folder": "broken",
            "step_id": "qc",
            "error_class": "QC_CONTENT_MISMATCH",
            "log_reference": "worker://qc-failed",
        }],
    }})


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 900}, locale="ko-KR")
    page.route("**/api/v1/work", work_list)
    page.route(f"**/api/v1/work/{RUN_ID}", work_detail)
    page.goto(sys.argv[1] + f"/#/runs/{RUN_ID}", wait_until="networkidle")

    failures = page.locator("#run-failures")
    expect(failures).to_be_visible()
    expect(failures).to_contain_text("실패 기록 1")
    expect(failures).to_contain_text("broken")
    expect(failures).to_contain_text("QC_CONTENT_MISMATCH")
    expect(failures).to_contain_text("worker://qc-failed")
    expect(failures.locator('.failure-record[href="#/jobs/JOB-BROKEN-EVIDENCE"]')).to_be_visible()
    expect(page.locator("#run-children .child-work")).to_have_count(0)
    browser.close()
