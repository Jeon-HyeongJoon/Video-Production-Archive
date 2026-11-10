import sys

from playwright.sync_api import expect, sync_playwright

# width, height and where the sidebar and the Agent pane go: beside the screen,
# the Agent as a drawer from the right edge, or also the sidebar as a strip above
VIEWPORTS = [(390, 844, "strip"), (820, 1180, "drawer"), (1024, 768, "beside"), (1440, 900, "beside"), (2560, 1440, "beside")]
# The Agent pane floats 8px inside the page edge.
INSET = 9


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: check_archive_workspace_layout.py URL")
    failures: list[str] = []
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch()
        for width, height, placement in VIEWPORTS:
            page = browser.new_page(viewport={"width": width, "height": height}, color_scheme="light", locale="ko-KR")
            # Allowed internal HTTP peers are not necessarily secure browser contexts.
            page.add_init_script("Object.defineProperty(globalThis.crypto, 'randomUUID', { value: undefined, configurable: true })")
            page.goto(sys.argv[1] + "/#/drives/VOLUME-1", wait_until="networkidle")
            expect(page.locator("#drive-folders .folder-row")).to_have_count(8)
            if placement != "beside":
                # Too narrow for two columns: the pane waits behind a handle on the right edge.
                page.locator("#agent-handle").click()
                page.wait_for_timeout(300)
            layout = page.evaluate("""(inset) => {
              const box = (selector) => document.querySelector(selector).getBoundingClientRect();
              const progress = box('#drive-progress'), problems = box('#drive-problems'), folders = box('#drive-folders');
              const view = box('#view'), pane = box('#archivist-pane'), side = box('.sidebar'), thread = document.querySelector('#archivist-thread');
              return {
                overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
                wrappedMenus: [...document.querySelectorAll('#menu .menu-link')].filter((link) => link.getClientRects().length !== 1 || link.offsetHeight > 60).map((link) => link.textContent),
                ordered: progress.top < problems.top && problems.top < folders.top,
                sideLeft: side.right <= view.left + 1 && side.top <= 1 && Math.abs(side.bottom - innerHeight) <= 1,
                sideAbove: side.bottom <= view.top + 1 && side.width >= innerWidth - 33,
                beside: pane.left >= view.right - 1 && pane.top <= inset && Math.abs(pane.bottom - innerHeight) <= inset,
                drawer: Math.abs(pane.right - innerWidth) <= inset && pane.left > 0 && pane.top <= inset && Math.abs(pane.bottom - innerHeight) <= inset,
                paneVisible: pane.width > 200 && pane.height > 200,
                ownScroll: getComputedStyle(thread).overflowY === 'auto',
              };
            }""", INSET)
            placed = {
                "beside": layout["beside"] and layout["sideLeft"],
                "drawer": layout["drawer"] and layout["sideLeft"],
                "strip": layout["drawer"] and layout["sideAbove"],
            }[placement]
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
