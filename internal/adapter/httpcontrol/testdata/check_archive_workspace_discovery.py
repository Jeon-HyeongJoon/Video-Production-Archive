import json
import sys

from playwright.sync_api import expect, sync_playwright


with sync_playwright() as playwright:
    browser = playwright.chromium.launch()
    page = browser.new_page(viewport={"width": 1440, "height": 900}, color_scheme="dark", locale="ko-KR")
    state = {"ready": False}

    def archive_intake(route):
        response = route.fetch()
        payload = response.json()
        drive = next(item for item in payload["data"] if item["workspace_drive_id"] == "VOLUME-1")
        template = (drive.get("discovered_folders") or drive.get("folders"))[0]
        folders = []
        for index in range(40):
            folder = dict(template)
            folder["folder"] = f"2026-10-31/촬영 원본과 편집 결과를 함께 보관하는 아주 긴 폴더 이름 {index + 1:02d}"
            folder["video_count"] = index + 1
            folder["estimated_output_bytes"] = (index + 1) * 1024 * 1024
            folders.append(folder)
        visible = folders if state["ready"] else folders[:1]
        drive["folder_discovery_state"] = "READY" if state["ready"] else "SCANNING"
        drive["folder_discovery_revision"] = 77 if state["ready"] else 0
        drive["input_revision"] = 77 if state["ready"] else 0
        drive["discovered_folders"] = visible
        drive["discovered_folder_count"] = len(visible)
        drive["discovered_file_count"] = sum(folder["video_count"] for folder in visible)
        route.fulfill(status=response.status, headers={"content-type": "application/json"}, body=json.dumps(payload))

    page.route("**/api/v1/archive-intake", archive_intake)
    page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
    page.locator("#prepare-run").click()
    expect(page.locator('#drive-overview [data-fact="found"]')).to_contain_text("검색 중 · 1 폴더 · 1 영상")
    expect(page.locator("#prep-folders .chooser-head")).to_contain_text("검색 중 · 1 폴더 · 1 영상")
    expect(page.locator("#select-all")).to_be_disabled()
    expect(page.locator("#prep-folders input[type=checkbox]:not(#select-all)")).to_be_disabled()
    expect(page.locator("#start-reason")).to_have_text("폴더 검색 중")
    expect(page.locator("#start-archive-run")).to_be_disabled()

    state["ready"] = True
    page.reload(wait_until="networkidle")
    page.locator("#prepare-run").click()
    expect(page.locator('#drive-overview [data-fact="found"]')).to_contain_text("40 폴더 · 820 영상")
    expect(page.locator("#prep-folders .chooser-head")).to_contain_text("탐색 r77 · 40")
    expect(page.locator("#select-all")).to_be_enabled()
    expect(page.locator("#prep-folders input[type=checkbox]:not(#select-all)")).to_have_count(40)

    archive_box = page.locator("#prep-archives").bounding_box()
    folder_box = page.locator("#prep-folders").bounding_box()
    if not archive_box or not folder_box or archive_box["y"] + archive_box["height"] > folder_box["y"] + 1:
        raise AssertionError(f"archive and folder choices are not vertical: archive={archive_box} folder={folder_box}")

    long_name = page.locator("#prep-folders .folder-pick-name").first
    name_layout = long_name.evaluate("""element => ({
      text: element.textContent, overflow: getComputedStyle(element).overflow,
      textOverflow: getComputedStyle(element).textOverflow,
      scrollWidth: element.scrollWidth, clientWidth: element.clientWidth,
    })""")
    if "아주 긴 폴더 이름" not in name_layout["text"] or name_layout["textOverflow"] == "ellipsis" or name_layout["scrollWidth"] > name_layout["clientWidth"] + 1:
        raise AssertionError(f"folder name is clipped or broken: {name_layout}")

    folder_list = page.locator("#prep-folders")
    select_all = page.locator("#prep-folders .folder-all")
    before = select_all.bounding_box()
    folder_list.evaluate("element => { element.scrollTop = element.scrollHeight; }")
    after = select_all.bounding_box()
    if not before or not after or abs(before["y"] - after["y"]) > 1:
        raise AssertionError(f"select all did not stay fixed at the list top: before={before} after={after}")

    # Selecting one item redraws the preparation summary. The long folder list
    # must stay where the person was working instead of jumping back to row 1.
    scroll_before_pick = folder_list.evaluate("element => element.scrollTop")
    page.locator("#prep-folders .folder-pick:not(.folder-all) input").last.check()
    expect(page.locator("#selection-summary")).to_contain_text("1 폴더")
    scroll_after_pick = folder_list.evaluate("element => element.scrollTop")
    if scroll_before_pick <= 0 or abs(scroll_before_pick - scroll_after_pick) > 1:
        raise AssertionError(
            f"folder selection lost its scroll position: before={scroll_before_pick} after={scroll_after_pick}"
        )

    browser.close()
