import { readFile } from "node:fs/promises";
import { chromium } from "playwright";

const svg = await readFile("internal/app/web/assets/icon.svg", "utf8");
const dataURL = `data:image/svg+xml;base64,${Buffer.from(svg).toString("base64")}`;
const browser = await chromium.launch();

try {
  for (const size of [192, 512]) {
    const page = await browser.newPage({ viewport: { width: size, height: size } });
    await page.setContent(`<style>*{margin:0}img{display:block;width:${size}px;height:${size}px}</style><img src="${dataURL}">`);
    await page.locator("img").screenshot({ path: `internal/app/web/assets/icon-${size}.png`, omitBackground: true });
  }
} finally {
  await browser.close();
}
