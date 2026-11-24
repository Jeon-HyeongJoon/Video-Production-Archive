import os
import re
import sys
from datetime import datetime, timezone
from typing import Any

from playwright.sync_api import expect, sync_playwright

# Per-scene projection facts the screens must show without inference.
MENUS = ["작업 생성", "작업 관리", "장비·에이전트"]
DRIVE = "#/drives/VOLUME-1"
SCENES: dict[str, dict[str, Any]] = {
    "pre-run": {"route": DRIVE, "problems": ("3", "fault"), "phase": "intake", "stages": ["todo", "todo", "todo"]},
    "stage-one": {
        "route": DRIVE, "problems": ("6", "fault"), "phase": "stage1", "stages": ["fault", "todo", "part"],
        "stage_problems": [("fault", "영상 변환 1"), ("decide", "처리 방식 결정 1"), ("decide", "결과 저장 1")],
        "progress": ("2 / 8 폴더", "최근 작업 보관 191 / 795 영상", "최근 작업 폴더 2 / 8"), "stt": None,
        "words": {"INPUT": "처리 방법 선택 필요", "DEVICE": "저장 위치 연결 필요"}, "panels": ["broken", "hold", "source-b"], "stored": 1, "committed": 1,
        "detail": ("broken", ["처리 방식 결정", "영상 변환"]),
    },
    "stt-ready": {
        "route": DRIVE, "problems": ("5", "fault"), "phase": "stt", "stages": ["fault", "now", "part"],
        "stage_problems": [("fault", "영상 변환 1"), ("decide", "처리 방식 결정 1")],
        "progress": ("6 / 8 폴더", "최근 작업 보관 790 / 795 영상", "최근 작업 폴더 6 / 8"), "stt": None,
        "words": {"INPUT": "처리 방법 선택 필요"}, "panels": ["broken", "hold"], "stored": 5, "committed": 1,
        "detail": ("broken", ["처리 방식 결정", "영상 변환"]),
    },
    "complete": {
        "route": DRIVE, "problems": ("6", "fault"), "phase": "done", "stages": ["fault", "done", "done"],
        "stage_problems": [("fault", "영상 변환 1"), ("decide", "처리 방식 결정 1")],
        "progress": ("6 / 8 폴더", "최근 작업 보관 790 / 795 영상", "최근 작업 폴더 6 / 8 · 자막·텍스트 4 폴더"), "stt": None, "catalog": "CSV 795행 · 10. 31. 00:00 기록",
        "words": {"INPUT": "처리 방법 선택 필요"}, "panels": ["broken", "hold", "ready"], "stored": 5, "committed": 1,
        "detail": ("ready", ["처리 방식 결정", "결과 검사", "결과 저장", "음성 준비", "음성을 텍스트로 변환", "자막 생성", "텍스트 파일 생성", "결과에 추가"]),
    },
    # DSH-004/025: grouped work management and one addressable ArchiveRun detail.
    "jobs": {"route": "#/jobs", "menu": "jobs", "problems": ("6", "fault"), "rows": 3},
    "job-detail": {"route": "#/runs/ARCHIVE-RUN-1", "menu": "jobs", "problems": ("6", "fault")},
    "output-preview": {"route": "#/create", "menu": "create", "problems": ("3", "fault")},
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
      // Work surfaces keep sharp corners; the Agent panel is a rounded conversation set apart from them.
      const radiusSelectors = ['.panel','.btn','.option','.chooser-col','.seg','.problem-panel','.overview','.folder-table','.table','.results','.worker-card','.capacity-item','.token','.step-node','.menu-link','.workspace-card','.side-search','.bell','.alert-toast','.work-block','.new-work'];
      const radii = radiusSelectors.flatMap((selector) => [...document.querySelectorAll(selector)].filter((element) => !element.closest('#archivist-pane')).map((element) => parseFloat(getComputedStyle(element).borderTopLeftRadius) || 0));
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
        pane: (() => { const pane = box('#archivist-pane'); return pane.width > 0 && pane.left >= box('#view').right - 1 && pane.top <= 9; })(),
        // The Agent panel sits on the work surface colour of the chosen theme.
        agentSurface: rgb(getComputedStyle(document.querySelector('#archivist-pane')).backgroundColor),
        topbar: document.querySelectorAll('.topbar').length,
        viewWidth: box('#view').width,
        explanatory: visibleText.filter((text)=>!text.parentElement.closest('#archivist-pane')).some((text)=>/했습니다|입니다|하세요/.test(text.textContent)),
      };
    }""")
    dark, light = contract["dark"], contract["light"]
    if (
        contract["theme"] != "dark" or dark["canvas"] > 40 or light["canvas"] < 200
        or max(dark["chroma"], light["chroma"]) > 12 or min(dark["text"], light["text"]) < 4.5 or min(dark["mark"], light["mark"]) < 3
        or contract["maxRadius"] > 2 or contract["minTextSize"] < 12 or contract["agentSurface"] != [27, 27, 27] or contract["topbar"]
        or contract["menus"] != MENUS or not contract["side"] or not contract["pane"] or contract["viewWidth"] > 1440 or contract["explanatory"]
    ):
        raise AssertionError(f"Archive Workspace visual contract failed: {contract}")


def check_stage_summary(page, scene) -> None:
    # The stage summary names three stages and the steps holding a problem; the
    # step matrix opens only on request.
    matrix = page.locator("#drive-overview .stage-matrix")
    expect(matrix).to_have_attribute("data-open", "false")
    expect(matrix.locator(".flow-node")).to_have_count(0)
    expect(matrix.locator(".stage-chip .stage-name")).to_have_text(["영상 정리·저장", "자막·텍스트", "영상 목록"])
    for chip, state in zip(matrix.locator(".stage-chip").all(), scene["stages"]):
        expect(chip).to_have_attribute("data-state", state)
    problems = scene.get("stage_problems", [])
    expect(matrix.locator(".stage-problem")).to_have_text([text for _, text in problems])
    for index, (signal, _) in enumerate(problems):
        expect(matrix.locator(".stage-problem").nth(index)).to_have_attribute("data-signal", signal)
    toggle = matrix.locator(".stage-toggle")
    expect(toggle).to_have_text("처리 과정 보기")
    toggle.click()
    expect(page.locator("#drive-overview .stage-matrix")).to_have_attribute("data-open", "true")
    expect(page.locator("#drive-overview .flow-node")).to_have_count(12)
    page.locator("#drive-overview .stage-toggle").click()
    expect(page.locator("#drive-overview .flow-node")).to_have_count(0)


def check_drive(page, scene) -> None:
    expect(page.locator('#drive-overview [data-fact="state"]')).to_have_attribute("data-phase", scene["phase"])
    # Problems are counted once, in 확인 필요; an online Worker and a decided
    # archive wait on the title's hover.
    expect(page.locator('#drive-overview [data-fact="problems"]')).to_have_count(0)
    expect(page.locator('#drive-overview [data-fact="worker"]')).to_have_count(0)
    expect(page.locator('#drive-overview [data-fact="archive"]')).to_have_count(1 if scene["phase"] == "intake" else 0)
    check_stage_summary(page, scene)
    if scene["phase"] == "intake":
        expect(page.locator("#prepare-run, #drive-prep, #start-archive-run")).to_have_count(0)
        expect(page.locator("#drive-progress")).to_have_count(0)
        expect(page.locator("#drive-folders .folder-row")).to_have_count(4)
        expect(page.locator("#drive-contents .section-title")).to_contain_text("영상 폴더")
        return
    pct, counts, folders = scene["progress"]
    expect(page.locator("#drive-progress .progress-pct")).to_have_text(pct)
    # Video and folder counts, the fixed scope and the CSV record wait on hover.
    hover = page.locator("#drive-progress")
    expect(hover).to_have_attribute("title", re.compile(re.escape(counts)))
    expect(hover).to_have_attribute("title", re.compile(re.escape(folders)))
    expect(hover).to_have_attribute("title", re.compile(re.escape("전체 발견 8 폴더")))
    expect(hover).to_have_attribute("title", re.compile(re.escape("작업 범위 r7 고정 · 8 폴더 · 795 영상 · 211 GB")))
    # Stage two continues automatically; the title never asks for a second
    # confirmation after the person already started the archive work.
    expect(page.locator("#start-stt")).to_have_count(0)
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
    # A row is the folder's name and state only. Resource waits, step names,
    # disposition and video counts stay off it.
    expect(page.locator('#drive-folders .status-text[data-wait-reason="MEMORY"]')).to_have_count(0)
    expect(page.locator("#drive-folders .folder-head span")).to_have_text(["폴더", "상태", ""])
    row_text = " ".join(page.evaluate("() => [...document.querySelectorAll('#drive-folders .folder-row')].map((row) => row.innerText)"))
    if any(word in row_text for word in ("렌더", "QC", "오디오 추출", "자막", "전사 텍스트", "그대로 보관", "소스 시퀀스", "보류")):
        raise AssertionError(f"folder rows exposed steps or dispositions: {row_text}")
    expect(page.locator('#drive-folders .folder-row[data-folder-id="broken"]')).to_have_attribute("title", "영상별 변환 · 2 영상")
    expect(page.locator('#drive-folders .folder-row .archive-badge[data-state="STORED"]')).to_have_count(scene["stored"])
    expect(page.locator('#drive-folders .folder-row .archive-badge[data-state="COMMITTED"]')).to_have_count(scene["committed"])
    folder, stages = scene["detail"]
    # The folder table opens its own rows; an open problem does not open its row.
    expect(page.locator("#folder-detail")).to_have_count(0)
    page.locator(f'#drive-folders .folder-row[data-folder-id="{folder}"]').click()
    detail = page.locator("#folder-detail")
    expect(detail).to_have_attribute("data-folder-id", folder)
    expect(detail.locator(".detail-facts")).to_contain_text(" 영상")
    expect(detail.locator(".stage-row")).to_have_count(0)
    detail.locator(".steps-toggle").click()
    expect(detail.locator(".stage-row b")).to_have_text(stages)
    detail.locator(".steps-toggle").click()
    if page.locator("#completed-work").count():
        order = page.evaluate("() => document.querySelector('#drive-problems').getBoundingClientRect().top < document.querySelector('#completed-work').getBoundingClientRect().top")
        if not order:
            raise AssertionError("직접 확인할 항목 is not above 저장된 결과")


def check_jobs(page, scene) -> None:
    expect(page.locator("#jobs-title ~ .num")).to_have_count(0)
    expect(page.locator("#jobs-title")).to_have_text("작업 관리")
    expect(page.locator("#new-job-open, #new-job-form, #new-job")).to_have_count(0)
    expect(page.locator("#new-job-form")).to_have_count(0)
    expect(page.locator(".job-cols span")).to_have_text(["작업", "상태", "진행", "추가", ""])
    running = page.locator('#active-jobs .work-row[data-work-id="JOB-RUNNING"]')
    expect(running).to_have_count(1)
    expect(running).to_have_attribute("title", re.compile("Worker WORKER-1"))
    expect(running.locator(".meter")).to_have_count(1)
    expect(page.locator('#queued-jobs .work-row[data-work-id="JOB-QUEUED"]')).to_have_count(1)
    expect(page.locator("#queued-jobs .meter, #attention-jobs .meter, #completed-jobs .meter")).to_have_count(0)
    expect(page.locator("#attention-jobs")).to_have_attribute("data-open", "true")
    expect(page.locator("#attention-jobs .lane-toggle")).to_have_text("직접 확인할 항목 1")
    expect(page.locator('#attention-jobs .work-row[data-work-id="JOB-REVIEW"]')).to_have_count(1)
    expect(page.locator("#completed-jobs")).to_have_attribute("data-open", "false")
    expect(page.locator("#completed-jobs .lane-toggle")).to_have_text("완료된 작업 1")
    expect(page.locator('#completed-jobs .work-row')).to_have_count(0)


def check_job_detail(page, _scene) -> None:
    expect(page.locator('.page-head a[href="#/jobs"]')).to_have_count(0)
    summary = page.locator("#run-summary")
    expect(summary.locator(".run-facts .fact-label")).to_have_text(["가져올 드라이브 → 저장 위치", "폴더", "영상"])
    expect(summary).to_contain_text("VOLUME-1 → archive-main")
    expect(summary).to_have_attribute("title", re.compile("작업 ID ARCHIVE-RUN-1"))
    expect(summary).to_have_attribute("title", re.compile("Worker WORKER-1"))
    # The work's open problems follow its overview, failures first, and are
    # resolved in place: a hold offers its choices here.
    attention = page.locator("#run-attention .problem-panel")
    expect(attention).to_have_count(3)
    expect(attention.first).to_have_attribute("data-signal", "fault")
    expect(attention.first).to_have_attribute("data-open", "true")
    page.locator('#run-attention .problem-panel[data-kind="hold"] .problem-head').click()
    expect(page.locator('#run-attention .problem-panel[data-kind="hold"] #hold-submit')).to_have_count(1)
    page.locator('#run-attention .problem-panel[data-kind="hold"] .problem-head').click()
    expect(page.locator("#run-scope")).to_have_attribute("data-open", "false")
    expect(page.locator("#run-scope .scope-row")).to_have_count(0)
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


def check_output_preview(page, _scene) -> None:
    expect(page.locator("#create-title")).to_have_text("작업 생성")
    page.locator('#prep-drives [data-drive-id="VOLUME-1"]').click()
    page.locator('#prep-folders [data-folder-id="event-a"] input').check()
    page.locator('#prep-folders [data-folder-id="source-a"] input').check()
    page.locator('#prep-archives [data-storage-id="archive-main"]').click()
    preview = page.locator("#expected-results")
    expect(preview).to_be_visible()
    expect(preview.locator(".preview-root")).to_contain_text("archive-main/")
    expect(preview.locator(".preview-root")).to_contain_text("2~191개 결과 폴더")
    folder = preview.locator('.expected-result-row[data-folder-id="event-a"]')
    expect(folder.locator(".result-directory > b")).to_have_text("2026-10-30_event-a__XXXXXXXXXXXX/")
    expect(folder.locator(".result-directory code")).to_have_text("event-a.mp4")
    expect(folder).to_contain_text("video-info.csv")
    expect(folder).to_contain_text(".archive-package.json")
    expect(folder).to_contain_text("subtitle.srt · transcript.txt")
    expect(preview.locator(".catalog-node")).to_contain_text("archive-catalog.csv")
    expect(preview.locator(".expected-capacity")).to_have_text("예상 70 GB안전 여유 3.5 GB필요 74 GB현재 여유 860 GB")
    visible = preview.inner_text()
    if any(value in visible for value in ("C:\\", "/Volumes/", "storage_root")):
        raise AssertionError(f"expected output preview exposed an absolute root: {visible}")


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
        expect(page.locator("#menu .menu-count")).to_have_count(0)
        # A workspace card shows the work's name and state; its ID, drive and
        # Worker wait on hover.
        card = page.locator('#tree .workspace-card[data-work-id="JOB-RUNNING"]')
        expect(card).to_have_attribute("title", "행사 하이라이트 · JOB-RUNNING · 드라이브 없음 · WORKER-1")
        expect(card).not_to_contain_text("JOB-RUNNING")
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
        elif scene_name == "output-preview":
            check_output_preview(page, scene)
        else:
            check_drive(page, scene)
        check_contract(page)
        page.mouse.move(0, 0)
        page.evaluate("document.activeElement.blur()")
        # The checks above may scroll the screen; the image is its first view.
        page.evaluate("document.querySelector('#view').scrollTop = 0")
        page.evaluate("document.fonts.ready")
        page.screenshot(path=output, full_page=True, animations="disabled")
        # The light theme is a settings choice this browser keeps; the default
        # stays dark and no screen outside settings carries a theme control.
        expect(page.locator("#theme-switch")).to_have_count(0)
        page.goto(url + "/#/settings", wait_until="networkidle")
        expect(page.locator('#theme-switch [data-theme-choice="dark"]')).to_have_attribute("aria-pressed", "true")
        page.locator('#theme-switch [data-theme-choice="light"]').click()
        expect(page.locator("html")).to_have_attribute("data-theme", "light")
        page.reload(wait_until="networkidle")
        expect(page.locator("html")).to_have_attribute("data-theme", "light")
        expect(page.locator('#theme-switch [data-theme-choice="light"]')).to_have_attribute("aria-pressed", "true")
        # The Agent panel follows the chosen theme like the work surface.
        expect(page.locator("#archivist-pane")).to_have_css("background-color", "rgb(255, 255, 255)")
        expect(page.locator("#archivist-pane")).to_have_css("color", "rgb(51, 51, 51)")
        browser.close()


if __name__ == "__main__":
    main()
