import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1440, "height": 960}, locale="ko-KR", timezone_id="Asia/Seoul")
    base_url = sys.argv[1]
    media_requests = []
    page.on("request", lambda request: media_requests.append(request.url) if request.url.endswith("/api/v1/jobs/JOB-SOURCE/media") else None)

    page.goto(base_url + "/#/runs/ARCHIVE-RUN-1", wait_until="networkidle")
    disclosure = page.locator('#run-children .child-work[data-job-id="JOB-SOURCE"]')
    expect(disclosure).to_have_attribute("data-open", "false")
    expect(disclosure.locator(".video-row")).to_have_count(0)
    assert media_requests == [], f"video information loaded before expansion: {media_requests}"

    disclosure.locator(".child-recent").click()
    expect(disclosure).to_have_attribute("data-open", "true")
    expect(disclosure.locator(".video-row")).to_have_count(2)
    first = disclosure.locator('.video-row[data-asset-id="ASSET-C0001"]')
    expect(first).to_contain_text("2026. 10. 30. · C0001.MP4")
    expect(first).to_contain_text("source/CLIP/C0001.MP4")
    expect(first).to_contain_text("1:01")
    expect(first).to_contain_text("154 MB")
    expect(first).to_contain_text("mp4")
    expect(first).to_contain_text("h264 · 1920×1080 · 19.8 Mbps")
    expect(first).to_contain_text("aac · 48 kHz · 2ch")
    assert len(media_requests) == 1, f"unexpected media request count: {media_requests}"

    disclosure.locator(".child-recent").click()
    expect(disclosure).to_have_attribute("data-open", "false")
    expect(disclosure.locator(".video-row")).to_have_count(0)
    disclosure.locator(".child-recent").click()
    expect(disclosure.locator(".video-row")).to_have_count(2)
    assert len(media_requests) == 1, f"cached disclosure requested media again: {media_requests}"

    disclosure.locator('a[href="#/jobs/JOB-SOURCE"]').click()
    expect(page).to_have_url(base_url + "/#/jobs/JOB-SOURCE")
    job_media = page.locator("#job-media")
    expect(job_media).to_have_attribute("data-open", "false")
    expect(job_media.locator(".video-row")).to_have_count(0)
    job_media.locator(".media-toggle").click()
    expect(job_media.locator(".video-row")).to_have_count(2)
    assert len(media_requests) == 2, f"Job detail did not load its own disclosure: {media_requests}"
    browser.close()
