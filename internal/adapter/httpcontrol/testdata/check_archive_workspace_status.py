import re
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1], wait_until="networkidle")
    expect(page.locator('#menu [data-menu="status"]')).to_have_attribute("aria-current", "page")
    # Every problem is its own card, failures first; nothing is grouped.
    cards = page.locator("#problem-cards .problem-card")
    expect(cards).to_have_count(6)
    expect(cards.locator(".problem-subject")).to_have_text([
        "VOLUME-1 / broken", "WORKER-2", "archivist-mac", "VOLUME-1 / hold", "VOLUME-1 / source-b", "인터뷰 요약 검수",
    ])
    # Each card names its state by icon shape as well as colour.
    expect(cards.first.locator('.sicon[data-kind="fault"]')).to_have_count(1)
    expect(page.locator('#problem-cards .problem-card[data-folder-id="hold"] .sicon[data-kind="decide"]')).to_have_count(1)
    expect(page.locator("#alert-count")).to_have_text("6")
    # Resource waits are progress, not problems.
    if "event-b" in " ".join(cards.all_inner_texts()):
        raise AssertionError("a MEMORY wait became a problem card")
    # Facts behind a card wait on hover.
    expect(page.locator('#problem-cards .problem-card[data-folder-id="broken"]')).to_have_attribute("title", re.compile("렌더 단계"))
    expect(page.locator('#problem-cards .problem-card[data-kind="device"]')).to_have_attribute("title", re.compile("archive-main"))

    # 지금 하는 일 draws each running drive as five facts over its flow of steps.
    expect(page.locator("#doing-rows .overview")).to_have_count(1)
    work = page.locator('#doing-rows .overview[data-drive-id="VOLUME-1"]')
    expect(work.locator('[data-fact="state"]')).to_contain_text("1단계 정리 중")
    expect(work.locator('[data-fact="progress"] .progress-pct')).to_have_text("24%")
    expect(work.locator('[data-fact="progress"]')).to_have_attribute("title", re.compile(re.escape("보관 191 / 795 영상")))
    expect(work.locator('[data-fact="worker"]')).to_contain_text("WORKER-1")
    expect(work.locator('[data-fact="archive"]')).to_contain_text("archive-main")
    expect(work.locator('[data-fact="problems"] .mini')).to_have_text(["1", "2"])
    expect(work.locator(".flow-node")).to_have_count(12)
    expect(work.locator('.flow-node[data-step="render"]')).to_have_attribute("data-state", "now")
    expect(work.locator('.flow-node[data-step="render"] .flow-badge[data-signal="fault"]')).to_have_text("1")
    expect(work.locator('.flow-node[data-step="classification"] .flow-badge[data-signal="decide"]')).to_have_text("1")
    expect(work.locator('.flow-node[data-step="gate"]')).to_have_attribute("data-state", "closed")
    work.locator('.flow-node[data-step="render"]').hover()
    expect(work.locator('.flow-node[data-step="render"] .flow-tip')).to_contain_text("실패 1 폴더")
    # Jobs that belong to no drive follow as one table.
    jobs = page.locator("#doing-rows .doing-jobs .doing-item")
    expect(jobs).to_have_count(2)
    expect(page.locator('#doing-rows .doing-item[data-job-id="JOB-REVIEW"]')).to_contain_text("승인 대기")

    # Each card leads to the place that resolves it, with that problem opened.
    page.locator('#problem-cards .problem-card[data-folder-id="broken"]').click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1?folder=broken")
    expect(page.locator('#drive-problems .problem-panel[data-folder-id="broken"]')).to_have_attribute("data-open", "true")
    page.go_back()
    page.locator('#problem-cards .problem-card[data-kind="device"]').click()
    expect(page).to_have_url(sys.argv[1] + "/#/drives/VOLUME-1?folder=source-b")
    expect(page.locator('#drive-problems .problem-panel[data-folder-id="source-b"]')).to_contain_text("archive-main 연결 시 자동 재개")
    page.go_back()
    page.locator('#problem-cards .problem-card[data-kind="worker"]').click()
    expect(page).to_have_url(sys.argv[1] + "/#/workers?worker=WORKER-2")
    expect(page.locator('#worker-list .worker-row[data-worker-id="WORKER-2"]')).to_have_attribute("data-focus", "true")
    page.go_back()
    page.locator('#problem-cards .problem-card[data-kind="host"]').click()
    expect(page).to_have_url(sys.argv[1] + "/#/settings")
    expect(page.locator('#host-table [data-host-id="archivist-mac"]')).to_contain_text("꺼짐")
    browser.close()
