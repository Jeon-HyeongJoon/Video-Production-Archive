import sys

from playwright.sync_api import expect, sync_playwright


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: render_dashboard.py URL OUTPUT.png")

    url, output = sys.argv[1:]
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch()
        page = browser.new_page(
            viewport={"width": 1440, "height": 900},
            color_scheme="dark",
            device_scale_factor=1,
            locale="ko-KR",
            timezone_id="Asia/Seoul",
        )
        page.goto(url, wait_until="networkidle")
        expect(page.locator("#health")).to_have_text("LIVE · v1-test")
        expect(page.locator("#jobs .card")).to_have_count(2)
        expect(page.locator("#workers .card")).to_have_count(2)

        search = page.locator("#asset-search")
        search.locator('input[name="text"]').fill("가을")
        with page.expect_response(lambda response: "/api/v1/assets?" in response.url and response.ok):
            search.locator('button[type="submit"]').click()
        expect(page.locator("#assets .result")).to_have_count(1)
        expect(page.locator("#assets .result h3")).to_have_text("가을 행사")

        page.evaluate("document.activeElement.blur()")
        page.evaluate("document.fonts.ready")
        page.screenshot(path=output, full_page=True, animations="disabled")
        browser.close()


if __name__ == "__main__":
    main()
