import re
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="light", locale="ko-KR")
    page.goto(sys.argv[1] + "/#/connections", wait_until="networkidle")

    capacity = page.locator('#worker-list [data-worker-id="WORKER-1"] .capacity-grid')
    expect(capacity).to_contain_text("CPU38%")
    expect(capacity).to_contain_text("GPU71%")
    expect(capacity).to_contain_text("메모리14 GB")
    expect(capacity).to_contain_text("작업 공간395 GB")
    expect(capacity.locator(".capacity-item", has_text="메모리")).to_have_attribute("title", re.compile(re.escape("예약 4.0 GB")))
    expect(capacity.locator(".capacity-item", has_text="작업 공간")).to_have_attribute("title", re.compile(re.escape("예약 25 GB")))
    expect(capacity).to_contain_text("고부하 작업1/1새 작업 대기 · 처리 순서")
    expect(capacity).to_contain_text("일반 작업0/2새 작업 가능")
    expect(capacity).to_contain_text("상태 갱신")
    expect(capacity).to_contain_text("새 작업 가능")
    offline = page.locator('#worker-list [data-worker-id="WORKER-2"] .capacity-grid')
    expect(offline).to_contain_text("고부하 작업0/1새 작업 불가 · 연결되지 않음")

    projected = page.evaluate("""async () => {
      const response = await fetch('/api/v1/workers');
      return (await response.json()).data[0];
    }""")
    expected = {
        "reserved_memory_bytes": 4 << 30,
        "reserved_scratch_bytes": 25 << 30,
        "capacity_available_memory_bytes": 14 << 30,
        "capacity_scratch_free_bytes": 395 << 30,
        "heavy_slots_used": 1,
        "heavy_slots_maximum": 1,
        "light_slots_used": 0,
        "light_slots_maximum": 2,
        "additional_work_possible": True,
    }
    for key, value in expected.items():
        if projected.get(key) != value:
            raise AssertionError(f"{key}: got {projected.get(key)!r}, want {value!r}")
    if projected["heavy_admission"] != {"available": False, "wait_reason": "SLOT", "code": "RESOURCE"}:
        raise AssertionError(projected["heavy_admission"])
    if projected["light_admission"] != {"available": True}:
        raise AssertionError(projected["light_admission"])

    browser.close()
