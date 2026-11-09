import { expect, test, type Page } from "@playwright/test";
import { DEV } from "../playwright.config";
import { noHorizontalOverflow, shot } from "./helpers";

const STATES: [string, (p: Page) => Promise<void>][] = [
  ["pin", (p) => expect(p.getByTestId("pin-boxes")).toBeVisible()],
  ["pin-6", (p) => expect(p.getByTestId("pin-boxes").locator("> div > span")).toHaveCount(6)],
  ["pin-passphrase", (p) => expect(p.getByRole("heading", { name: "Enter the passphrase." })).toBeVisible()],
  ["pin-error", (p) => expect(p.getByTestId("pin-hint")).toHaveText("2 tries left before a pause.")],
  ["pin-locked", (p) => expect(p.getByTestId("pin-hint")).toContainText("Try again in")],
  ["waiting", (p) => expect(p.getByTestId("state-waiting")).toBeVisible()],
  ["knock", (p) => expect(p.getByText("Tell the host your code:")).toBeVisible()],
  ["connecting", (p) => expect(p.getByTestId("state-connecting")).toBeVisible()],
  ["live", (p) => expect(p.getByTestId("live-badge")).toBeVisible()],
  ["live-degraded", (p) => expect(p.getByTestId("hud")).toBeVisible()],
  ["paused", (p) => expect(p.getByTestId("paused-cover")).toContainText("Paused")],
  ["reconnecting", (p) => expect(p.getByTestId("state-badge")).toContainText("Reconnecting")],
  ["ended", (p) => expect(p.getByTestId("state-ended")).toBeVisible()],
  ["removed", (p) => expect(p.getByTestId("state-removed-kicked")).toBeVisible()],
  ["locked", (p) => expect(p.getByTestId("state-removed-locked")).toBeVisible()],
  ["unsupported", (p) => expect(p.getByTestId("state-unsupported")).toBeVisible()],
];

async function videoPlaying(page: Page) {
  await expect.poll(() => page.evaluate(() => (document.querySelector("video") as HTMLVideoElement | null)?.videoWidth ?? 0)).toBeGreaterThan(0);
}

for (const [state, ready] of STATES) {
  test(`viewer state: ${state}`, async ({ page }, info) => {
    await page.goto(`${DEV}/dev/viewer-ui?state=${state}`);
    await ready(page);
    if (["live", "live-degraded", "paused", "reconnecting"].includes(state)) {
      await videoPlaying(page);
      await expect(page.getByTestId("player")).toHaveAttribute("data-layout", info.project.name === "desktop" ? "framed" : "immersive");
    }
    await noHorizontalOverflow(page);
    await shot(page, info, `viewer-${state}`);
  });
}

test("PIN entry auto-submits on the last digit and clears on failure", async ({ page }) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=pin`);
  const input = page.getByTestId("pin-input");
  await input.focus();
  await page.keyboard.type("482");
  await expect(page.getByRole("button", { name: "Join" })).toBeDisabled();
  await page.keyboard.type("1");
  await expect(page.getByRole("button", { name: "Checking…" })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveText("That PIN didn't match.");
  await expect(input).toHaveValue("");
  // Paste with separators.
  await page.evaluate(() => navigator.clipboard?.writeText?.("12 34").catch(() => undefined));
  await input.fill("12 34");
  await expect(page.getByRole("button", { name: "Checking…" })).toBeVisible();
});

test("the player state uses the full dynamic viewport on phones", async ({ page }, info) => {
  test.skip(info.project.name === "desktop", "phones only");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const box = await page.getByTestId("player").boundingBox();
  const vp = page.viewportSize()!;
  expect(box).toMatchObject({ x: 0, y: 0, width: vp.width, height: vp.height });
  expect(await page.getByTestId("stage").evaluate((e) => getComputedStyle(e).touchAction)).toBe("none");
});

test("pinch zooms, double tap fits", async ({ page, context }, info) => {
  test.skip(info.project.name === "desktop", "touch only");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const layer = page.getByTestId("zoom-layer");
  const cdp = await context.newCDPSession(page);
  const vp = page.viewportSize()!;
  const cx = vp.width / 2,
    cy = vp.height / 2;
  const touch = (type: string, d: number) =>
    cdp.send("Input.dispatchTouchEvent", {
      type,
      touchPoints: type === "touchEnd" ? [] : [{ x: cx - d, y: cy, id: 1 }, { x: cx + d, y: cy, id: 2 }],
    });
  await touch("touchStart", 30);
  for (const d of [45, 60, 80, 100, 120]) await touch("touchMove", d);
  await touch("touchEnd", 0);
  await expect.poll(async () => Number(await layer.getAttribute("data-scale"))).toBeGreaterThan(2);
  await shot(page, info, "viewer-live-zoomed");
  await page.touchscreen.tap(cx, cy);
  await page.touchscreen.tap(cx, cy);
  await expect(layer).toHaveAttribute("data-scale", "1.00");
});

test("desktop keys and theatre", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "desktop only");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  await page.keyboard.press("z");
  await expect.poll(async () => Number(await page.getByTestId("zoom-layer").getAttribute("data-scale"))).toBeGreaterThan(1);
  await page.keyboard.press("z");
  await expect(page.getByTestId("zoom-layer")).toHaveAttribute("data-scale", "1.00");
  await page.keyboard.press("m");
  expect(await page.evaluate(() => document.querySelector("video")!.muted)).toBe(false);
  await page.getByRole("button", { name: "Theatre mode" }).click();
  await expect(page.getByTestId("player")).toHaveAttribute("data-layout", "immersive");
  await shot(page, info, "viewer-live-theatre");
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("player")).toHaveAttribute("data-layout", "framed");
});

test("the viewer's row code and colour are tappable at every width", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const narrow = page.viewportSize()!.width < 560;
  const chip = page.getByTestId(narrow ? "chrome-code-compact" : "chrome-code");
  await expect(page.getByTestId(narrow ? "chrome-code" : "chrome-code-compact")).toBeHidden();
  await expect(chip).toBeVisible();
  await expect(chip).toHaveAccessibleName(/^Your code K7P, \w+\. Connection details$/);
  const box = (await chip.boundingBox())!;
  expect(box.height).toBeGreaterThanOrEqual(44);
  expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize()!.width);
  await chip.click();
  await expect(page.getByTestId("hud-code")).toHaveText(/Your code\s*K7P\s*· \w+/);
  await noHorizontalOverflow(page);
  await shot(page, info, "viewer-live-code");
});

test("keyboard focus reveals auto-hidden controls and holds them", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "keyboard");
  await page.goto(`${DEV}/dev/viewer-ui?state=live&hide=800`);
  await videoPlaying(page);
  const info_ = page.getByRole("button", { name: "Connection details", exact: true });
  const bar = page.locator(".transition-opacity", { has: info_ });
  await expect(bar).toHaveCSS("opacity", "0");
  await info_.focus();
  await expect(bar).toHaveCSS("opacity", "1");
  await page.waitForTimeout(1500);
  await expect(bar).toHaveCSS("opacity", "1");
  await page.keyboard.press("Tab");
  await expect(page.locator(":focus")).toHaveAccessibleName("Zoom in");
  await page.waitForTimeout(1500);
  await expect(bar).toHaveCSS("opacity", "1");
  await page.locator(":focus").blur();
  await expect(bar).toHaveCSS("opacity", "0");
});

test("privacy notice", async ({ page }, info) => {
  await page.goto(`${DEV}/privacy`);
  await expect(page.getByRole("heading", { name: "Who sees what when you watch." })).toBeVisible();
  await noHorizontalOverflow(page);
  await shot(page, info, "viewer-privacy", true);
});

test("reduced motion stops the breathing badge", async ({ browser }, info) => {
  test.skip(info.project.name !== "iphone-390", "one run");
  const ctx = await browser.newContext({ reducedMotion: "reduce", viewport: { width: 390, height: 844 } });
  const page = await ctx.newPage();
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  const dur = await page.getByTestId("live-badge").locator("span").first().evaluate((e) => getComputedStyle(e).animationDuration);
  expect(parseFloat(dur)).toBeLessThan(0.01);
  await ctx.close();
});
