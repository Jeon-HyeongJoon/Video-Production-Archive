import json
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")

    def without_transcription(route):
        response = route.fetch()
        payload = response.json()
        for drive in payload.get("data") or []:
            drive["archive_run_state"] = "COMPLETED"
            drive["eligible_transcription_count"] = 0
            drive["stage_two_remaining"] = 0
            for folder in drive.get("folders") or []:
                folder["transcription_job_id"] = ""
                folder["can_add_transcription"] = False
                folder["stages"] = [
                    stage for stage in folder.get("stages") or []
                    if stage.get("stage_id") not in {"audio", "transcript", "subtitle", "transcript_text", "add"}
                ]
        route.fulfill(
            status=response.status,
            headers={**response.headers, "content-type": "application/json"},
            body=json.dumps(payload),
        )

    page.route("**/api/v1/archive-intake*", without_transcription)
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
    page.evaluate("""async () => { const store = await import('/assets/js/store.js'); await store.refresh(); }""")
    projected = page.evaluate("""async () => (await fetch('/api/v1/archive-intake')).json()""")
    assert not any(
        folder.get("transcription_job_id")
        for drive in projected.get("data") or []
        for folder in drive.get("folders") or []
    ), projected
    client_drive = page.evaluate("""async () => {
      const store = await import('/assets/js/store.js');
      const drive = store.driveView('VOLUME-1');
      return (drive?.folders || []).map((folder) => ({name: folder.folder, stt: folder.transcription_job_id || ''}));
    }""")
    assert not any(folder["stt"] for folder in client_drive), client_drive
    stage = page.locator('#drive-overview .stage-chip[data-stage="two"]')
    expect(stage).to_have_attribute("data-state", "skip")
    expect(stage).to_have_attribute("aria-label", "자막·텍스트 · 대상 없음")
    page.locator("#drive-overview .stage-toggle").click()
    expect(page.locator('#drive-overview .flow-node[data-step="gate"]')).to_have_attribute("data-state", "skip")
    expect(page.locator('#drive-overview .flow-node[data-step="subtitle"]')).to_have_attribute("data-state", "todo")
    browser.close()
