// Viewer UX on phones: every state at 320–430 px, portrait and landscape,
// touch, DPR 3, in Chromium (device emulation) and Linux WebKit. Screenshots
// of every state at 320 and 390 px go to docs/screenshots/viewer/.
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { DEV } from "../playwright.viewer.config";

const docs = fileURLToPath(new URL("../../docs/screenshots/viewer", import.meta.url));
const engine = (info: TestInfo) => info.project.name.split("-")[0];
const isWebKit = (info: TestInfo) => engine(info) === "webkit";
const landscape = (info: TestInfo) => info.project.name.endsWith("landscape");
const width = (info: TestInfo) => Number(info.project.name.split("-")[1]);

/** docs/screenshots/viewer/<width>[-landscape]/<name>.png (WebKit: webkit-<width>). Only 320 and 390 are kept. */
async function shot(page: Page, info: TestInfo, name: string) {
  const w = width(info);
  if (w !== 320 && w !== 390) return;
  const dir = `${isWebKit(info) ? "webkit-" : ""}${w}${landscape(info) ? "-landscape" : ""}`;
  const path = join(docs, dir, `${name}.png`);
  mkdirSync(dirname(path), { recursive: true });
  // CSS pixels keep the committed set small; the checks themselves run at DPR 3.
  await page.screenshot({ path, animations: "disabled", scale: "css" });
}

async function noOverflow(page: Page) {
  const [sw, sh, w, h] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.scrollHeight, innerWidth, innerHeight]);
  expect(sw, "horizontal overflow").toBeLessThanOrEqual(w);
  return { scrollHeight: sh, height: h };
}

/**
 * Every visible button, link and input is inside the viewport and has a name,
 * and every button and input on every screen has a ≥ 44 px target. The one
 * documented exception: links inside a sentence (WCAG 2.5.8's inline
 * exception), such as the privacy notice link.
 */
async function buttonsUsable(page: Page, minTarget: number) {
  const vp = page.viewportSize()!;
  const bad = await page.evaluate(
    ({ vw, vh, min }) => {
      const out: string[] = [];
      for (const b of document.querySelectorAll<HTMLElement>("button, a[href], input")) {
        const r = b.getBoundingClientRect();
        const cs = getComputedStyle(b);
        const hidden = r.width === 0 || cs.visibility === "hidden" || b.closest(".opacity-0") || b.closest("[aria-hidden=true]");
        if (hidden) continue;
        const name = b.getAttribute("aria-label") || b.textContent?.trim() || (b as HTMLInputElement).labels?.[0]?.textContent?.trim() || b.getAttribute("placeholder");
        if (!name) out.push(`unnamed ${b.outerHTML.slice(0, 80)}`);
        if (r.right > vw + 0.5 || r.left < -0.5) out.push(`off-screen x ${name} ${r.left}..${r.right}`);
        const inline = b.tagName === "A" && !!b.closest("p");
        if (!inline && Math.min(r.width, r.height) < min - 0.5) out.push(`small ${name} ${r.width}x${r.height}`);
        if (b.closest("[data-testid=player]") && (r.bottom > vh + 0.5 || r.top < -0.5)) out.push(`off-screen y ${name}`);
      }
      return out;
    },
    { vw: vp.width, vh: vp.height, min: minTarget },
  );
  expect(bad).toEqual([]);
}

/**
 * Targets that must be fully visible without scrolling: every player button,
 * clipped by every overflow ancestor and the viewport, shows at least
 * `min` × `min` px. The HUD's controls live in its fixed header; only its
 * rows scroll on a short phone.
 */
async function targetsVisible(page: Page, min: number) {
  const bad = await page.evaluate((min) => {
    const out: string[] = [];
    for (const b of document.querySelectorAll<HTMLElement>("[data-testid=player] button")) {
      const r0 = b.getBoundingClientRect();
      if (r0.width === 0 || getComputedStyle(b).visibility === "hidden" || b.closest(".opacity-0") || b.closest("[aria-hidden=true]")) continue;
      let r = { left: r0.left, top: r0.top, right: r0.right, bottom: r0.bottom };
      for (let a = b.parentElement; a; a = a.parentElement) {
        const cs = getComputedStyle(a);
        if (cs.overflowX === "visible" && cs.overflowY === "visible") continue;
        const q = a.getBoundingClientRect();
        r = { left: Math.max(r.left, q.left), top: Math.max(r.top, q.top), right: Math.min(r.right, q.right), bottom: Math.min(r.bottom, q.bottom) };
      }
      r = { left: Math.max(r.left, 0), top: Math.max(r.top, 0), right: Math.min(r.right, innerWidth), bottom: Math.min(r.bottom, innerHeight) };
      const w = r.right - r.left, h = r.bottom - r.top;
      if (Math.min(w, h) < min - 0.5) out.push(`${b.getAttribute("aria-label") || b.textContent?.trim()} visible ${w.toFixed(1)}x${h.toFixed(1)}`);
    }
    return out;
  }, min);
  expect(bad).toEqual([]);
}

async function videoPlaying(page: Page) {
  await expect.poll(() => page.evaluate(() => (document.querySelector("video") as HTMLVideoElement | null)?.videoWidth ?? 0), { timeout: 15_000 }).toBeGreaterThan(0);
}

const PLAYER = ["live", "live-degraded", "paused", "reconnecting", "reconnecting-long", "reconnecting-host"];
const STATES: [string, (p: Page) => Promise<void>][] = [
  ["pin", (p) => expect(p.getByTestId("pin-boxes")).toBeVisible()],
  ["pin-6", (p) => expect(p.getByTestId("pin-boxes").locator("> div > span")).toHaveCount(6)],
  ["pin-12", (p) => expect(p.getByTestId("pin-boxes").locator("> div > span")).toHaveCount(12)],
  ["pin-passphrase", (p) => expect(p.getByRole("heading", { name: "Enter the passphrase." })).toBeVisible()],
  ["pin-checking", (p) => expect(p.getByRole("button", { name: "Checking…" })).toBeVisible()],
  ["pin-error", (p) => expect(p.getByTestId("pin-hint")).toHaveText("2 tries left before a pause.")],
  ["pin-locked", (p) => expect(p.getByTestId("pin-countdown")).toBeVisible()],
  ["waiting", (p) => expect(p.getByTestId("state-waiting")).toBeVisible()],
  ["knock", (p) => expect(p.getByTestId("own-code")).toBeVisible()],
  ["connecting", (p) => expect(p.getByTestId("state-connecting")).toBeVisible()],
  ["connecting-slow", (p) => expect(p.getByTestId("slow")).toBeVisible()],
  ["admitted", (p) => expect(p.getByTestId("state-admitted")).toBeVisible()],
  ["live", (p) => expect(p.getByTestId("live-badge")).toBeVisible()],
  ["live-degraded", (p) => expect(p.getByTestId("hud")).toBeVisible()],
  ["paused", (p) => expect(p.getByTestId("paused-cover")).toBeVisible()],
  ["reconnecting", (p) => expect(p.getByTestId("state-badge")).toContainText("Reconnecting…")],
  ["reconnecting-long", (p) => expect(p.getByTestId("state-badge")).toContainText("Still reconnecting")],
  ["reconnecting-host", (p) => expect(p.getByTestId("state-badge")).toContainText("Waiting for the host")],
  ["ended", (p) => expect(p.getByTestId("state-ended")).toBeVisible()],
  ["removed", (p) => expect(p.getByTestId("state-removed-kicked")).toBeVisible()],
  ["locked", (p) => expect(p.getByTestId("state-removed-locked")).toBeVisible()],
  ["unsupported", (p) => expect(p.getByTestId("state-unsupported")).toBeVisible()],
];

for (const [state, ready] of STATES) {
  test(`state ${state}: fits, usable, screenshot`, async ({ page }, info) => {
    await page.goto(`${DEV}/dev/viewer-ui?state=${state}`);
    await ready(page);
    if (state === "pin-checking") await page.getByTestId("pin-input").fill("4821");
    if (PLAYER.includes(state)) {
      await videoPlaying(page);
      const box = (await page.getByTestId("player").boundingBox())!;
      const vp = page.viewportSize()!;
      expect(box).toMatchObject({ x: 0, y: 0, width: vp.width, height: vp.height });
    }
    await noOverflow(page);
    await buttonsUsable(page, 44);
    await shot(page, info, state);
  });
}

test("the real /v route fails closed without RTCRtpScriptTransform: no socket, no peer connection, a clear message", async ({ page }, info) => {
  // Chromium has the API, so remove it before any script runs. Linux WebKit
  // lacks it natively: the same test runs there unmodified.
  if (!isWebKit(info))
    await page.addInitScript(() => {
      delete (window as { RTCRtpScriptTransform?: unknown }).RTCRtpScriptTransform;
    });
  await page.addInitScript(() => {
    const w = window as unknown as { __pcs: number; RTCPeerConnection: typeof RTCPeerConnection };
    w.__pcs = 0;
    const Orig = w.RTCPeerConnection;
    if (Orig)
      w.RTCPeerConnection = new Proxy(Orig, {
        construct(t, a) {
          w.__pcs++;
          return Reflect.construct(t, a);
        },
      });
  });
  const sockets: string[] = [];
  // Vite's own HMR socket (dev server only, path "/") isn't the app's.
  page.on("websocket", (ws) => new URL(ws.url()).pathname !== "/" && sockets.push(ws.url()));
  const requests: string[] = [];
  page.on("request", (r) => requests.push(new URL(r.url()).pathname));
  await page.goto(`${DEV}/v#c=AAAA&k=BBBB&s=CCCC`);
  const card = page.getByTestId("state-unsupported");
  await expect(card).toBeVisible();
  await expect(card).toContainText("never falls back to an unencrypted stream");
  await expect(card).toContainText("RTCRtpScriptTransform (encrypted media)");
  await expect(page.getByRole("heading", { level: 1 })).toBeFocused();
  await page.waitForTimeout(1500);
  expect(sockets).toEqual([]);
  expect(requests.filter((p) => p.startsWith("/signal") || p.startsWith("/api"))).toEqual([]);
  expect(await page.evaluate(() => (window as unknown as { __pcs: number }).__pcs)).toBe(0);
  expect(await page.evaluate(() => !!(window as { passstream?: unknown }).passstream)).toBe(false);
  await noOverflow(page);
  await shot(page, info, "real-unsupported");
});

test("the real /v route explains an incomplete link", async ({ page }, info) => {
  test.skip(isWebKit(info), "WebKit stops at unsupported first (fail closed)");
  await page.goto(`${DEV}/v#c=only-half`);
  await expect(page.getByTestId("state-invalid")).toContainText("This link is incomplete.");
  await noOverflow(page);
  await shot(page, info, "real-invalid");
});

test("PIN: numeric keypad, auto-submit on the last digit, clears on failure, paste with separators", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=pin`);
  const input = page.getByTestId("pin-input");
  await expect(input).toHaveAttribute("inputmode", "numeric");
  await expect(input).toHaveAttribute("pattern", "[0-9]*");
  await expect(input).toHaveAttribute("autocomplete", "one-time-code");
  await expect(input).toHaveAttribute("enterkeyhint", "go");
  // A tap on the boxes focuses the one real input.
  const boxes = (await page.getByTestId("pin-boxes").boundingBox())!;
  if (info.project.use.hasTouch) await page.touchscreen.tap(boxes.x + boxes.width * 0.6, boxes.y + boxes.height / 2);
  else await input.click();
  await expect(input).toBeFocused();
  await page.keyboard.type("482");
  await expect(page.getByRole("button", { name: "Join" })).toBeDisabled();
  await page.keyboard.type("1");
  await expect(page.getByRole("button", { name: "Checking…" })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveText("That PIN didn't match.");
  await expect(input).toHaveValue("");
  await expect(input).toBeFocused();
  await input.fill("PIN: 12-34");
  await expect(input).toHaveValue("1234");
  await expect(page.getByRole("button", { name: "Checking…" })).toBeVisible();
});

test("PIN: focus order is PIN, name, Join, privacy notice", async ({ page }, info) => {
  test.skip(isWebKit(info), "WebKit's Tab skips links by default");
  await page.goto(`${DEV}/dev/viewer-ui?state=pin-passphrase`);
  await expect(page.getByTestId("pin-input")).toBeFocused();
  // Join is enabled (and so focusable) once the passphrase is long enough.
  await page.keyboard.type("correct horse");
  const order: string[] = [];
  for (let i = 0; i < 4; i++) {
    await page.keyboard.press("Tab");
    order.push(await page.evaluate(() => (document.activeElement as HTMLElement).getAttribute("data-testid") || document.activeElement!.textContent!.trim()));
  }
  expect(order).toEqual(["pin-reveal", "name-input", "Join", "Who sees what, and for how long"]);
});

test("PIN: lockout disables entry and counts down", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=pin-locked`);
  await expect(page.getByTestId("pin-input")).toBeDisabled();
  const a = await page.getByTestId("pin-countdown").textContent();
  await page.waitForTimeout(1300);
  const b = await page.getByTestId("pin-countdown").textContent();
  expect(a).not.toBe(b);
  await expect(page.getByTestId("pin-hint").locator(".sr-only")).toContainText("pausing PIN checks");
  await shot(page, info, "pin-locked-countdown");
});

test("PIN: passphrase show/hide and paste", async ({ page }) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=pin-passphrase`);
  const input = page.getByTestId("pin-input");
  await expect(input).toHaveAttribute("type", "password");
  await input.fill("correct horse battery");
  await page.getByTestId("pin-reveal").click();
  await expect(input).toHaveAttribute("type", "text");
  await expect(page.getByRole("button", { name: "Join" })).toBeEnabled();
});

test("player: double tap zooms in on the tapped point and back to fit", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const layer = page.getByTestId("zoom-layer");
  const vp = page.viewportSize()!;
  // Two taps through real touch events, inside the double-tap window.
  await page.touchscreen.tap(vp.width * 0.3, vp.height * 0.45);
  await page.touchscreen.tap(vp.width * 0.3, vp.height * 0.45);
  await expect.poll(async () => Number(await layer.getAttribute("data-scale"))).toBeGreaterThan(2);
  await expect(page.getByTestId("zoom-level")).toContainText("2.5×");
  await shot(page, info, "live-zoomed");
  await page.touchscreen.tap(vp.width * 0.3, vp.height * 0.45);
  await page.touchscreen.tap(vp.width * 0.3, vp.height * 0.45);
  await expect(layer).toHaveAttribute("data-scale", "1.00");
});

test("player: pinch zooms, one finger pans, zoom pill fits", async ({ page, context }, info) => {
  test.skip(isWebKit(info), "multi-touch needs Chromium's CDP");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const layer = page.getByTestId("zoom-layer");
  const cdp = await context.newCDPSession(page);
  const vp = page.viewportSize()!;
  const cx = vp.width / 2,
    cy = vp.height / 2;
  const pinch = (type: string, d: number) =>
    cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x: cx - d, y: cy, id: 1 }, { x: cx + d, y: cy, id: 2 }] });
  await pinch("touchStart", 30);
  for (const d of [45, 60, 80, 100, 120]) await pinch("touchMove", d);
  await pinch("touchEnd", 0);
  await expect.poll(async () => Number(await layer.getAttribute("data-scale"))).toBeGreaterThan(2);
  const before = await layer.evaluate((e) => e.style.transform);
  const drag = (type: string, x: number) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x, y: cy, id: 3 }] });
  await drag("touchStart", cx);
  for (const x of [cx + 20, cx + 40, cx + 60]) await drag("touchMove", x);
  await drag("touchEnd", 0);
  await expect.poll(() => layer.evaluate((e) => e.style.transform)).not.toBe(before);
  await page.getByTestId("zoom-level").click();
  await expect(layer).toHaveAttribute("data-scale", "1.00");
});

/** The rendered picture's rect relative to the stage, from the DOM (object-fit math redone here). */
async function picture(page: Page) {
  return page.evaluate(() => {
    const stage = document.querySelector("[data-testid=stage]")!.getBoundingClientRect();
    const layer = document.querySelector("[data-testid=zoom-layer]")!.getBoundingClientRect();
    const el = document.querySelector("[data-testid=zoom-layer]") as HTMLElement;
    const v = document.querySelector("[data-testid=video]") as HTMLVideoElement;
    const s = layer.width / stage.width;
    // Settled: the animated (computed) transform has reached the target one.
    const target = /translate3d\((-?[\d.e-]+)px, (-?[\d.e-]+)px, 0(?:px)?\) scale\(([\d.]+)\)/.exec(el.style.transform)!.slice(1).map(Number);
    const m = new DOMMatrix(getComputedStyle(el).transform);
    const settled = Math.abs(m.a - target[2]) < 0.001 && Math.abs(m.e - target[0]) < 0.5 && Math.abs(m.f - target[1]) < 0.5;
    let px = 0, py = 0, pw = stage.width, ph = stage.height;
    if (getComputedStyle(v).objectFit !== "cover") {
      const k = Math.min(stage.width / v.videoWidth, stage.height / v.videoHeight);
      pw = v.videoWidth * k;
      ph = v.videoHeight * k;
      px = (stage.width - pw) / 2;
      py = (stage.height - ph) / 2;
    }
    return { settled, sw: stage.width, sh: stage.height, scale: s, left: layer.left - stage.left + s * px, top: layer.top - stage.top + s * py, width: s * pw, height: s * ph };
  });
}

/** On each axis a picture larger than the stage covers it; a smaller one is centred. Never a new gap, never out of view. */
async function pictureInBounds(page: Page) {
  await expect(async () => {
    const p = await picture(page);
    expect(p.settled).toBe(true);
    for (const [start, len, size] of [[p.left, p.width, p.sw], [p.top, p.height, p.sh]]) {
      if (len >= size - 1) {
        expect(start).toBeLessThanOrEqual(1);
        expect(start + len).toBeGreaterThanOrEqual(size - 1);
      } else expect(Math.abs(start - (size - len) / 2)).toBeLessThanOrEqual(1);
    }
  }).toPass({ timeout: 5000 });
  return picture(page);
}

async function touchDrag(page: Page, cdp: import("@playwright/test").CDPSession, dx: number, dy: number) {
  const vp = page.viewportSize()!;
  const x0 = vp.width / 2, y0 = vp.height / 2;
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: x0, y: y0, id: 7 }] });
  for (let i = 1; i <= 12; i++) await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x0 + (dx * i) / 12, y: y0 + (dy * i) / 12, id: 7 }] });
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}

test("zoom bounds: rotating while zoomed leaves no gap, in both orientations", async ({ page }, info) => {
  test.skip(isWebKit(info), "viewport rotation runs in the Chromium matrix");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const vp = page.viewportSize()!;
  await page.getByRole("button", { name: "Zoom in", exact: true }).click();
  await expect(page.getByTestId("zoom-layer")).toHaveAttribute("data-scale", "2.50");
  await pictureInBounds(page);
  await page.setViewportSize({ width: vp.height, height: vp.width });
  const rotated = await pictureInBounds(page);
  expect(rotated.sw).toBe(vp.height);
  expect(rotated.scale).toBeGreaterThan(2);
  await page.setViewportSize(vp);
  await pictureInBounds(page);
});

test("zoom bounds: maximum pan keeps the picture in view; fill re-clamps", async ({ page, context }, info) => {
  test.skip(isWebKit(info), "multi-touch needs Chromium's CDP");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const cdp = await context.newCDPSession(page);
  await page.getByRole("button", { name: "Zoom in", exact: true }).click();
  await expect(page.getByTestId("zoom-layer")).toHaveAttribute("data-scale", "2.50");
  for (const [dx, dy] of [[4000, 4000], [-4000, -4000], [4000, -4000], [-4000, 4000]]) {
    await touchDrag(page, cdp, dx, dy);
    const p = await pictureInBounds(page);
    // Some of the picture always covers the stage centre.
    expect(p.left).toBeLessThan(p.sw / 2);
    expect(p.left + p.width).toBeGreaterThan(p.sw / 2);
    expect(p.top).toBeLessThan(p.sh / 2);
    expect(p.top + p.height).toBeGreaterThan(p.sh / 2);
  }
  // Fill while zoomed and panned to a corner, then back to fit: still in bounds.
  await page.getByTestId("fill-toggle").click();
  await pictureInBounds(page);
  await touchDrag(page, cdp, -4000, -4000);
  await page.getByTestId("fill-toggle").click();
  await pictureInBounds(page);
});

test("combined controls: degraded or reconnecting, zoomed, with the HUD open, fit without overlap", async ({ page }, info) => {
  for (const state of ["live-degraded", "reconnecting"]) {
    await page.goto(`${DEV}/dev/viewer-ui?state=${state}`);
    await videoPlaying(page);
    await page.getByRole("button", { name: "Zoom in", exact: true }).click();
    await expect(page.getByTestId("zoom-level")).toBeVisible();
    if (!(await page.getByTestId("hud").isVisible())) await page.getByTestId("quality-pill").click();
    await expect(page.getByTestId("hud")).toBeVisible();
    await noOverflow(page);
    await buttonsUsable(page, 44);
    const overlaps = await page.evaluate(() => {
      // The HUD scrolls inside its container on short phones: its visible box is the container's.
      const els = [...document.querySelectorAll<HTMLElement>("[data-testid=controls] button, [data-testid=state-badge], [data-testid=live-badge]"), document.querySelector<HTMLElement>("[data-testid=hud]")!.parentElement!].filter((e) => e.getBoundingClientRect().width > 0 && !e.closest(".opacity-0"));
      const out: string[] = [];
      const label = (e: HTMLElement) => e.getAttribute("aria-label") || e.dataset.testid || e.textContent?.trim().slice(0, 20);
      // What is actually visible: each rect clipped by its overflow ancestors (a button scrolled out of the HUD body shows nothing).
      const visible = (e: HTMLElement) => {
        const r0 = e.getBoundingClientRect();
        let r = { left: r0.left, top: r0.top, right: r0.right, bottom: r0.bottom };
        for (let a = e.parentElement; a; a = a.parentElement) {
          const cs = getComputedStyle(a);
          if (cs.overflowX === "visible" && cs.overflowY === "visible") continue;
          const q = a.getBoundingClientRect();
          r = { left: Math.max(r.left, q.left), top: Math.max(r.top, q.top), right: Math.min(r.right, q.right), bottom: Math.min(r.bottom, q.bottom) };
        }
        return r;
      };
      for (let i = 0; i < els.length; i++)
        for (let j = i + 1; j < els.length; j++) {
          const [a, b] = [els[i], els[j]];
          if (a.contains(b) || b.contains(a)) continue;
          const [r, q] = [visible(a), visible(b)];
          if (Math.min(r.right, q.right) - Math.max(r.left, q.left) > 0.5 && Math.min(r.bottom, q.bottom) - Math.max(r.top, q.top) > 0.5) out.push(`${label(a)} × ${label(b)}`);
        }
      return out;
    });
    expect(overlaps).toEqual([]);
    await shot(page, info, `${state}-zoomed-hud`);
  }
});

test("HUD open in every player state, with and without audio: the close target is fully visible without scrolling", async ({ page }, info) => {
  for (const audio of [false, true])
    for (const state of PLAYER) {
      if (state === "paused") continue; // no HUD control is offered over the paused cover
      await page.goto(`${DEV}/dev/viewer-ui?state=${state}${audio ? "&audio=1" : ""}`);
      await videoPlaying(page);
      if (!(await page.getByTestId("hud").isVisible())) await page.getByRole("button", { name: "Connection details", exact: true }).click();
      await expect(page.getByTestId("hud")).toBeVisible();
      const close = page.getByRole("button", { name: "Close connection details" });
      await expect(close).toBeVisible();
      await targetsVisible(page, 44);
      await buttonsUsable(page, 44);
      // The close button works from where it is, with no scrolling.
      if (audio && state === "reconnecting") await shot(page, info, "reconnecting-audio-hud");
      await close.click({ trial: true });
    }
});

test("player: fit and fill", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const video = page.getByTestId("video");
  await expect(video).toHaveCSS("object-fit", "contain");
  await page.getByTestId("fill-toggle").click();
  await expect(video).toHaveCSS("object-fit", "cover");
  await shot(page, info, "live-fill");
  await page.getByTestId("fill-toggle").click();
  await expect(video).toHaveCSS("object-fit", "contain");
});

test("player: controls hide on a live picture and a tap brings them back", async ({ page }) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=live&hide=800`);
  await videoPlaying(page);
  const bar = page.locator(".transition-opacity", { has: page.getByRole("button", { name: "Connection details", exact: true }) });
  await expect(bar).toHaveCSS("opacity", "0");
  const vp = page.viewportSize()!;
  await page.touchscreen.tap(vp.width / 2, vp.height / 2);
  await expect(bar).toHaveCSS("opacity", "1");
  await expect(bar).toHaveCSS("opacity", "0", { timeout: 3000 });
});

test("paused: nothing of the picture shows anywhere on the stage", async ({ page }) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=paused`);
  await videoPlaying(page);
  const cover = page.getByTestId("paused-cover");
  await expect(cover).toHaveCSS("background-color", "rgb(18, 16, 14)");
  await expect(cover).toHaveCSS("opacity", "1");
  // A 5×5 grid over the stage: the topmost element is never the video.
  const hits = await page.evaluate(() => {
    const out: string[] = [];
    for (let i = 0; i < 5; i++)
      for (let j = 0; j < 5; j++) {
        const el = document.elementFromPoint(((i + 0.5) / 5) * innerWidth, ((j + 0.5) / 5) * innerHeight);
        out.push(el?.tagName === "VIDEO" || el?.closest("[data-testid=zoom-layer]") ? "VIDEO" : "covered");
      }
    return out;
  });
  expect(hits.filter((h) => h === "VIDEO")).toEqual([]);
  await expect(page.getByTestId("live-badge")).toHaveCount(0);
});

test("rotation reflows the player without overflow", async ({ page }, info) => {
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const vp = page.viewportSize()!;
  await page.setViewportSize({ width: vp.height, height: vp.width });
  await expect.poll(async () => (await page.getByTestId("player").boundingBox())!.width).toBe(vp.height);
  await noOverflow(page);
  await buttonsUsable(page, 44);
  if (!landscape(info)) await shot(page, info, "live-rotated");
  await page.setViewportSize(vp);
  await expect.poll(async () => (await page.getByTestId("player").boundingBox())!.width).toBe(vp.width);
  await noOverflow(page);
});

test("fullscreen where supported", async ({ page }, info) => {
  test.skip(isWebKit(info), "headless WPE has no fullscreen window");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  await videoPlaying(page);
  const fs = page.getByRole("button", { name: "Fullscreen" });
  await expect(fs).toBeVisible();
  await fs.click();
  await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(true);
  await expect(page.getByRole("button", { name: "Exit fullscreen" })).toBeVisible();
  await page.getByRole("button", { name: "Exit fullscreen" }).click();
  await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(false);
});

test("reduced motion: no zoom animation, no spinner, no breathing badge", async ({ browser }, info) => {
  test.skip(width(info) !== 390 || landscape(info), "one run per engine");
  const ctx = await browser.newContext({ reducedMotion: "reduce", viewport: { width: 390, height: 844 }, hasTouch: true });
  const page = await ctx.newPage();
  await page.goto(`${DEV}/dev/viewer-ui?state=reconnecting`);
  const spin = await page.getByTestId("state-badge").locator("[aria-hidden=true]").first().evaluate((e) => getComputedStyle(e).animationName);
  expect(spin).toBe("none");
  await page.goto(`${DEV}/dev/viewer-ui?state=live`);
  const zoom = await page.getByTestId("zoom-layer").evaluate((e) => parseFloat(getComputedStyle(e).transitionDuration));
  expect(zoom).toBeLessThan(0.01);
  const breathe = await page.getByTestId("live-badge").locator("span").first().evaluate((e) => parseFloat(getComputedStyle(e).animationDuration));
  expect(breathe).toBeLessThan(0.01);
  await ctx.close();
});
