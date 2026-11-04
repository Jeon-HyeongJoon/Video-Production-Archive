import sys

from playwright.sync_api import expect, sync_playwright

# width, height and whether the sidebar and the Agent pane sit beside the screen
VIEWPORTS = [(390, 844, False), (820, 1180, True), (1024, 768, True), (1440, 900, True), (2560, 1440, True)]


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: check_archive_workspace_layout.py URL")
    failures: list[str] = []
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch()
        for width, height, beside in VIEWPORTS:
            page = browser.new_page(viewport={"width": width, "height": height}, color_scheme="light", locale="ko-KR")
            # Allowed internal HTTP peers are not necessarily secure browser contexts.
            page.add_init_script("Object.defineProperty(globalThis.crypto, 'randomUUID', { value: undefined, configurable: true })")
            page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
            expect(page.locator("#drive-folders .folder-row")).to_have_count(8)
            if not beside:
                # Too narrow for two columns: the pane waits behind a handle on the right edge.
                page.locator("#agent-handle").click()
                page.wait_for_timeout(300)
            layout = page.evaluate("""() => {
              const box = (selector) => document.querySelector(selector).getBoundingClientRect();
              const progress = box('#drive-progress'), problems = box('#drive-problems'), folders = box('#drive-folders');
              const view = box('#view'), pane = box('#archivist-pane'), side = box('.sidebar'), thread = document.querySelector('#archivist-thread');
              return {
                overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
                wrappedMenus: [...document.querySelectorAll('#menu .menu-link')].filter((link) => link.getClientRects().length !== 1 || link.offsetHeight > 60).map((link) => link.textContent),
                ordered: progress.top < problems.top && problems.top < folders.top,
                sideLeft: side.right <= view.left + 1 && side.top >= 55 && Math.abs(side.bottom - innerHeight) <= 1,
                sideAbove: side.bottom <= view.top + 1 && side.width >= innerWidth - 1,
                beside: pane.left >= view.right - 1 && pane.top <= 1 && Math.abs(pane.bottom - innerHeight) <= 1,
                drawer: Math.abs(pane.right - innerWidth) <= 1 && pane.left > 0 && pane.top <= 1 && Math.abs(pane.bottom - innerHeight) <= 1,
                paneVisible: pane.width > 200 && pane.height > 200,
                ownScroll: getComputedStyle(thread).overflowY === 'auto',
              };
            }""")
            placed = (layout["beside"] and layout["sideLeft"]) if beside else (layout["drawer"] and layout["sideAbove"])
            if (
                layout["overflow"] != 0 or layout["wrappedMenus"] or not layout["ordered"]
                or not placed or not layout["paneVisible"] or not layout["ownScroll"]
            ):
                failures.append(f"{width}x{height}: {layout}")
            page.close()
        browser.close()
    if failures:
        raise AssertionError("Archive Workspace layout contract failed:\n" + "\n".join(failures))


if __name__ == "__main__":
    main()
