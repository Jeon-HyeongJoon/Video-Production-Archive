import os
import re
import sys
from datetime import datetime, timezone
from typing import Any

from playwright.sync_api import expect, sync_playwright

# Per-scene projection facts the screens must show without inference.
MENUS = ["작업 관리", "연결 관리"]
DRIVE = "#/drives/VOLUME-1"
SCENES: dict[str, dict[str, Any]] = {
    "pre-run": {"route": DRIVE, "problems": ("3", "fault"), "phase": "intake", "prepare": "아카이브 실행 준비"},
    "stage-one": {
        "route": DRIVE, "problems": ("6", "fault"), "phase": "stage1",
        "progress": ("24%", "보관 191 / 795 영상", "보관 폴더 2 / 8"), "stt": ("STT · 1단계 4 폴더 남음", True),
        "words": {"INPUT": "분류 확인 필요", "DEVICE": "저장소 연결 대기"}, "panels": ["broken", "hold", "source-b"], "stored": 1, "committed": 1,
        "detail": ("broken", ["분류", "렌더"]),
    },
    "stt-ready": {
        "route": DRIVE, "problems": ("6", "fault"), "phase": "stt",
        "progress": ("99%", "보관 790 / 795 영상", "보관 폴더 6 / 8"), "stt": ("STT 시작 · 4 폴더", False),
        "words": {"INPUT": "분류 확인 필요"}, "panels": ["broken", "hold"], "stored": 5, "committed": 1,
        "detail": ("broken", ["분류", "렌더"]),
    },
    "complete": {
        "route": DRIVE, "problems": ("6", "fault"), "phase": "done",
        "progress": ("99%", "보관 790 / 795 영상", "보관 폴더 6 / 8 · STT 4 폴더"), "stt": ("STT 완료", True), "catalog": "CSV 795행 · 10. 31. 00:00 기록",
        "words": {"INPUT": "분류 확인 필요"}, "panels": ["broken", "hold", "ready"], "stored": 5, "committed": 1,
        "detail": ("ready", ["분류", "QC", "보관", "오디오 추출", "STT", "자막", "전사 텍스트", "패키지 추가"]),
    },
    # DSH-004/025: grouped work management and one addressable ArchiveRun detail.
    "jobs": {"route": "#/jobs", "menu": "jobs", "problems": ("6", "fault"), "rows": 3},
    "job-detail": {"route": "#/runs/ARCHIVE-RUN-1", "menu": "jobs", "problems": ("6", "fault")},
}


def check_contract(page) -> None:
    contract = page.evaluate("""() => {
      const html = document.documentElement;
      const parseHex = (value) => { const match=value.trim().match(/^#([0-9a-f]{6})$/i); return match ? [0,2,4].map((o)=>parseInt(match[1].slice(o,o+2),16)) : [0,0,0]; };
      const neutralNames = ['--canvas','--surface','--raised','--sunken','--ink','--soft','--muted','--line','--hair','--strong','--idle'];
      // Both themes keep an achromatic ground and readable text; the dark one is the default.
      const theme = (name) => {
        const before = html.dataset.theme;
        html.dataset.theme = name;
        const root = getComputedStyle(html);
        const value = (token) => parseHex(root.getPropertyValue(token));
        const luminance = (token) => value(token).map((v)=>{ const c=v/255; return c<=0.03928?c/12.92:((c+0.055)/1.055)**2.4; }).reduce((sum,v,i)=>sum+v*[0.2126,0.7152,0.0722][i],0);
        const contrast = (front, back) => { const [a,b]=[luminance(front),luminance(back)].sort((x,y)=>y-x); return (a+0.05)/(b+0.05); };
        const result = {
          canvas: value('--canvas').reduce((sum,v)=>sum+v,0)/3,
          chroma: Math.max(...neutralNames.map((token) => { const rgb=value(token); return Math.max(...rgb)-Math.min(...rgb); })),
          text: Math.min(contrast('--ink','--surface'), contrast('--muted','--surface'), contrast('--muted','--canvas'), contrast('--soft','--surface')),
          mark: contrast('--idle','--surface'),
        };
        html.dataset.theme = before;
        return result;
      };
      const radiusSelectors = ['.panel','.btn','.option','.chooser-col','.seg','.problem-card','.problem-panel','.overview','.drive-card','.folder-table','.data-table','.worker-row','.capacity-item','.action-card','.message.user','.token','.flow-node','.menu-link','.tree-item','.topbar-field','.alert-button','.toast'];
      const radii = radiusSelectors.flatMap((selector) => [...document.querySelectorAll(selector)].map((element) => parseFloat(getComputedStyle(element).borderTopLeftRadius) || 0));
      const visibleText = []; const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      while (walker.nextNode()) { const parent = walker.currentNode.parentElement; if (walker.currentNode.textContent.trim() && parent && parent.getClientRects().length && !parent.closest('.sr-only')) visibleText.push(walker.currentNode); }
      const box = (selector) => document.querySelector(selector).getBoundingClientRect();
      const rgb = (value) => (value.match(/\d+(?:\.\d+)?/g) || []).slice(0, 3).map(Number);
      return {
        theme: html.dataset.theme, dark: theme('dark'), light: theme('light'), maxRadius: Math.max(0, ...radii),
        minTextSize: Math.min(...visibleText.map((text)=>parseFloat(getComputedStyle(text.parentElement).fontSize))),
        menus: [...document.querySelectorAll('#menu .menu-link .menu-label')].map((label)=>label.textContent.trim()),
        side: box('.sidebar').right <= box('#view').left + 1 && document.querySelectorAll('#tree .workspace-card').length > 0,
        sideDetail: {sidebarRight: box('.sidebar').right, viewLeft: box('#view').left, workspaces: document.querySelectorAll('#tree .workspace-card').length},
        pane: (() => { const pane = box('#archivist-pane'); return pane.width > 0 && pane.left >= box('#view').right - 1 && pane.top <= 1; })(),
        agentDark: (() => { const rgbValue=rgb(getComputedStyle(document.querySelector('.pane-body')).backgroundColor); return rgbValue.length === 3 && Math.max(...rgbValue) <= 12; })(),
        agentHead: rgb(getComputedStyle(document.querySelector('.pane-head')).backgroundColor),
        viewWidth: box('#view').width,
        explanatory: visibleText.filter((text)=>!text.parentElement.closest('#archivist-pane')).some((text)=>/했습니다|입니다|하세요/.test(text.textContent)),
      };
    }""")
    dark, light = contract["dark"], contract["light"]
    if (
        contract["theme"] != "dark" or dark["canvas"] > 40 or light["canvas"] < 200
        or max(dark["chroma"], light["chroma"]) > 12 or min(dark["text"], light["text"]) < 4.5 or min(dark["mark"], light["mark"]) < 3
        or contract["maxRadius"] > 2 or contract["minTextSize"] < 12 or not contract["agentDark"] or contract["agentHead"] != [27, 27, 27]
        or contract["menus"] != MENUS or not contract["side"] or not contract["pane"] or contract["viewWidth"] > 1440 or contract["explanatory"]
    ):
        raise AssertionError(f"Archive Workspace visual contract failed: {contract}")


def check_drive(page, scene) -> None:
    expect(page.locator('#drive-overview [data-fact="state"]')).to_have_attribute("data-phase", scene["phase"])
    if "prepare" in scene:
        expect(page.locator("#prepare-run")).to_have_text(scene["prepare"])
        expect(page.locator("#drive-progress")).to_have_count(0)
        expect(page.locator("#drive-folders")).to_have_count(0)
        return
    pct, counts, folders = scene["progress"]
    expect(page.locator("#drive-progress .progress-pct")).to_have_text(pct)
    # Video and folder counts, the fixed scope and the CSV record wait on hover.
    hover = page.locator("#drive-progress")
    expect(hover).to_have_attribute("title", re.compile(re.escape(counts)))
    expect(hover).to_have_attribute("title", re.compile(re.escape(folders)))
    expect(hover).to_have_attribute("title", re.compile(re.escape("범위 r7 고정 · 8 폴더 · 795 영상 · 211 GB")))
    text, disabled = scene["stt"]
    expect(page.locator("#start-stt")).to_have_text(text)
    expect(page.locator("#start-stt")).to_be_disabled() if disabled else expect(page.locator("#start-stt")).to_be_enabled()
    if "catalog" in scene:
        expect(hover).to_have_attribute("title", re.compile(re.escape(scene["catalog"])))
    # Problems a person resolves here come first; the failed one opens itself.
    panels = page.locator("#drive-problems .problem-panel")
    expect(panels).to_have_count(len(scene["panels"]))
    expect(panels.first).to_have_attribute("data-folder-id", scene["panels"][0])
    expect(panels.first).to_have_attribute("data-open", "true")
    for folder in scene["panels"]:
        expect(page.locator(f'#drive-problems .problem-panel[data-folder-id="{folder}"]')).to_have_count(1)
    rows = page.locator("#drive-folders .folder-row")
    expect(rows).to_have_count(8)
    expect(page.locator('#drive-folders [data-folder-id="broken"] .status-text')).to_have_text("실패")
    expect(page.locator("#drive-folders .status-text[data-wait-reason]")).to_have_count(len(scene["words"]))
    for reason, value in scene["words"].items():
        status = page.locator(f'#drive-folders .status-text[data-wait-reason="{reason}"]')
        expect(status).to_have_text(value)
        expect(status).to_have_attribute("data-signal", "decide")
    # Resource waits, step names and item counts stay off the rows; only the
    # disposition column may name a render.
    expect(page.locator('#drive-folders .status-text[data-wait-reason="MEMORY"]')).to_have_count(0)
    row_text = " ".join(page.evaluate("""() => [...document.querySelectorAll('#drive-folders .folder-row')].map((row) =>
      [...row.children].filter((cell) => !cell.classList.contains('folder-disp')).map((cell) => cell.innerText).join(' '))"""))
    if any(step in row_text for step in ("렌더", "QC", "오디오 추출", "자막", "전사 텍스트")):
        raise AssertionError(f"folder rows exposed step names: {row_text}")
    expect(page.locator('#drive-folders .folder-row .archive-badge[data-state="STORED"]')).to_have_count(scene["stored"])
    expect(page.locator('#drive-folders .folder-row .archive-badge[data-state="COMMITTED"]')).to_have_count(scene["committed"])
    folder, stages = scene["detail"]
    if folder != scene["panels"][0]:
        page.locator(f'#drive-folders .folder-row[data-folder-id="{folder}"]').click()
    detail = page.locator("#folder-detail")
    expect(detail).to_have_attribute("data-folder-id", folder)
    expect(detail.locator(".stage-row")).to_have_count(0)
    detail.locator(".steps-toggle").click()
    expect(detail.locator(".stage-row b")).to_have_text(stages)
    detail.locator(".steps-toggle").click()


def check_jobs(page, scene) -> None:
    expect(page.locator('#active-jobs .work-row[data-work-id="JOB-RUNNING"]')).to_have_count(1)
    expect(page.locator('#queued-jobs .work-row[data-work-id="JOB-QUEUED"]')).to_have_count(1)
    expect(page.locator("#attention-jobs")).to_have_attribute("open", "")
    expect(page.locator("#attention-jobs > summary")).to_have_text("확인 필요1")
    expect(page.locator('#attention-jobs .work-row[data-work-id="JOB-REVIEW"]')).to_have_count(1)
    expect(page.locator("#completed-jobs")).to_have_attribute("open", "")
    expect(page.locator("#completed-jobs > summary")).to_have_text("완료1")
    completed = page.locator('#completed-jobs .work-row[data-work-id="ARCHIVE-RUN-1"]')
    expect(completed).to_have_attribute("href", "#/runs/ARCHIVE-RUN-1")
    expect(completed).to_contain_text("8 폴더 · 795 영상")


def check_job_detail(page, _scene) -> None:
    expect(page.locator("#run-summary")).to_contain_text("ARCHIVE-RUN-1")
    expect(page.locator("#run-scope .data-row")).to_have_count(8)
    child = page.locator('#run-children .child-work[data-job-id="JOB-SOURCE"]')
    expect(child).to_have_count(1)
    expect(child.locator(".video-row")).to_have_count(0)
    child.locator(".child-recent").click()
    expect(child.locator(".video-row")).to_have_count(2)
    expect(child.locator('.video-row[data-asset-id="ASSET-C0001"]')).to_contain_text("C0001.MP4")
    expect(child.locator('.video-row[data-asset-id="ASSET-C0001"]')).to_contain_text("h264 · 1920×1080 · 19.8 Mbps")
    result = page.locator('#run-results [data-package-id="VP-20261030-SOURCE"]')
    expect(result).to_contain_text("source.mp4")
    expect(result).to_contain_text("86 → 1")
    expect(result).to_contain_text("archive-main")


def main() -> None:
    if len(sys.argv) != 4 or sys.argv[3] not in SCENES:
        raise SystemExit("usage: render_archive_workspace.py URL OUTPUT.png SCENE")
    url, output, scene_name = sys.argv[1:]
    scene = SCENES[scene_name]
    with sync_playwright() as playwright:
        launch_options = {}
        if executable := os.environ.get("ARCHIVE_WORKSPACE_BROWSER_EXECUTABLE"):
            launch_options["executable_path"] = executable
        browser = playwright.chromium.launch(**launch_options)
        page = browser.new_page(
            viewport={"width": 1440, "height": 900}, color_scheme="dark", device_scale_factor=1,
            locale="ko-KR", timezone_id="Asia/Seoul",
        )
        page_errors = []
        page.on("pageerror", lambda error: page_errors.append(str(error)))
        page.clock.set_fixed_time(datetime(2026, 10, 30, 12, 0, 5, tzinfo=timezone.utc))
        page.goto(url + "/" + scene["route"], wait_until="networkidle")
        menu = scene.get("menu", "connections")
        expect(page.locator("#menu .menu-link[aria-current=page]")).to_have_attribute("data-menu", menu)
        expect(page.locator("#job-count")).to_have_text("4")
        expect(page.locator("#connection-count")).to_have_text("5")
        expect(page.locator('#tree .workspace-card[data-work-id="JOB-RUNNING"]')).to_contain_text("JOB-RUNNING")
        count, signal = scene["problems"]
        expect(page.locator("#alert-count")).to_have_text(count)
        expect(page.locator("#alert-count")).to_have_attribute("data-signal", signal)
        expect(page.locator("#alert-panel")).to_be_hidden()
        expect(page.locator("#stale-banner")).to_be_hidden()
        expect(page.locator("#fresh-banner")).to_be_hidden()
        expect(page.locator("#archivist-panel")).to_be_visible()
        if page_errors:
            raise AssertionError(f"Archive Workspace page error: {page_errors}")
        if scene_name == "jobs":
            check_jobs(page, scene)
        elif scene_name == "job-detail":
            check_job_detail(page, scene)
        else:
            check_drive(page, scene)
        check_contract(page)
        page.mouse.move(0, 0)
        page.evaluate("document.activeElement.blur()")
        page.evaluate("document.fonts.ready")
        page.screenshot(path=output, full_page=True, animations="disabled")
        # The light theme is a choice this browser keeps; the default stays dark.
        expect(page.locator('#theme-switch [data-theme-choice="dark"]')).to_have_attribute("aria-pressed", "true")
        page.locator('#theme-switch [data-theme-choice="light"]').click()
        expect(page.locator("html")).to_have_attribute("data-theme", "light")
        page.reload(wait_until="networkidle")
        expect(page.locator("html")).to_have_attribute("data-theme", "light")
        expect(page.locator('#theme-switch [data-theme-choice="light"]')).to_have_attribute("aria-pressed", "true")
        expect(page.locator(".pane-head")).to_have_css("background-color", "rgb(255, 255, 255)")
        expect(page.locator(".pane-head")).to_have_css("color", "rgb(51, 51, 51)")
        expect(page.locator(".pane-body")).to_have_css("background-color", "rgb(10, 10, 10)")
        expect(page.locator(".pane-body")).to_have_css("color", "rgb(218, 218, 218)")
        browser.close()


if __name__ == "__main__":
    main()
