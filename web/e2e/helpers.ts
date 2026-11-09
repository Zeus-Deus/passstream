import { expect, type Page, type TestInfo } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const out = fileURLToPath(new URL("../screenshots", import.meta.url));

export const env = () => ({
  base: process.env.PS_BASE!,
  key: process.env.PS_KEY!,
  showcase: process.env.PS_SHOWCASE!,
  ended: process.env.PS_ENDED!,
  control: process.env.PS_CONTROL!,
});

/** web/screenshots/<project>/<name>.png */
export async function shot(page: Page, info: TestInfo, name: string, fullPage = false) {
  const path = join(out, info.project.name, `${name}.png`);
  mkdirSync(dirname(path), { recursive: true });
  if (!fullPage) {
    await page.screenshot({ path, animations: "disabled" });
    return;
  }
  // Grow the viewport to the page instead of fullPage capture, so fixed bars
  // (header, phone tab bar) sit where a user would see them.
  const vp = page.viewportSize()!;
  await page.evaluate(() => window.scrollTo(0, 0));
  const height = await page.evaluate(() => document.documentElement.scrollHeight);
  await page.setViewportSize({ width: vp.width, height: Math.max(vp.height, height) });
  await page.screenshot({ path, animations: "disabled" });
  await page.setViewportSize(vp);
}

/** Nothing may force a horizontal scroll, down to 320 px. */
export async function noHorizontalOverflow(page: Page) {
  const [scroll, width] = await page.evaluate(() => [document.documentElement.scrollWidth, window.innerWidth]);
  expect(scroll, "horizontal overflow").toBeLessThanOrEqual(width);
}

export async function login(page: Page) {
  const { base, key } = env();
  await page.goto(`${base}/dashboard`);
  await page.getByLabel("API key").fill(key);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Who's watching your screen." })).toBeVisible();
}
