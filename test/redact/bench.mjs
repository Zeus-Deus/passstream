// Safe mode benchmark: before/after screenshots and timings.
// Usage: node test/redact/bench.mjs [--gpu] [--hold=MS] [--out=DIR] [--headed]
// Needs the web dev server: (cd web && npx vite --port 5199 --strictPort)
import { chromium } from "../../web/node_modules/playwright-core/index.mjs";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";

const arg = (k, d) => process.argv.find((a) => a.startsWith(`--${k}=`))?.split("=")[1] ?? d;
const gpu = process.argv.includes("--gpu");
const headed = process.argv.includes("--headed") || gpu;
const hold = Number(arg("hold", "0"));
const bypass = process.argv.includes("--bypass");
const threads = arg("threads") ? Number(arg("threads")) : undefined;
const only = arg("only");
const workers = arg("workers") ? Number(arg("workers")) : undefined;
const run = (name) => !only || only.split(",").includes(name);
const out = arg("out", "/tmp/redact/out");
const url = arg("url", "http://localhost:5199/bench/redact.html");
mkdirSync(out, { recursive: true });

const args = ["--auto-accept-this-tab-capture", "--ignore-gpu-blocklist", "--enable-gpu"];
if (gpu) args.push("--enable-unsafe-webgpu", "--enable-features=Vulkan", "--ozone-platform=wayland");
else args.push("--use-angle=vulkan");
const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: !headed, args });
// Headed (GPU) runs use the real window, so capture pixels match the page's CSS pixels.
const page = await browser.newPage(headed ? { viewport: null } : { viewport: { width: 1920, height: 1080 } });
page.on("console", (m) => { if (m.type() === "error") console.error("page:", m.text().slice(0, 300)); });
page.on("pageerror", (e) => console.error("pageerror:", String(e).slice(0, 300)));
await page.goto(url);
await page.waitForFunction(() => window.bench);

const png = (name, dataUrl) => writeFileSync(`${out}/${name}.png`, Buffer.from(dataUrl.split(",")[1], "base64"));
const report = { gpu, hold, runs: {} };
const needles = {};
const image = process.argv.includes("--image");
const start = image
  ? await page.evaluate(([o, u]) => window.bench.startImage(o, u), [{ holdMs: hold, gpu, threads, workers }, "/static-before.png"])
  : await page.evaluate((o) => window.bench.start(o), { holdMs: hold, gpu, bypass, threads, workers });
report.start = { settings: start.settings, backend: start.stats?.backend, readyMs: Math.round(start.readyMs) };
console.log("ready", report.start);

if (image) {
  // Replays the headless run's screenshots (needles saved by it) on a canvas source.
  const saved = JSON.parse(readFileSync(arg("needles", "/tmp/redact/out-cpu/needles.json"), "utf8"));
  for (const [name, url, key] of [["cold", "/static-before.png", "static"], ["popup", "/popup-before.png", "popup"]]) {
    await page.evaluate(() => window.bench.mark());
    if (name === "cold") await page.evaluate(() => window.bench.reset());
    else await page.evaluate((u) => window.bench.showImage(u), url);
    const reveal = await page.evaluate(() => window.bench.waitClear(20000));
    await page.waitForTimeout(300);
    const snap = await page.evaluate(() => window.bench.snapshot());
    png(`${name}-after`, snap.after);
    const recall = await page.evaluate((n) => window.bench.recallSaved(n), saved[key]);
    const m = await page.evaluate(() => window.bench.measure());
    report.runs[name] = { revealMs: Math.round(reveal), recall, ...m };
    const missed = recall.filter((x) => x.covered < 90);
    console.log(name, "reveal", Math.round(reveal), "ms", `${recall.length - missed.length}/${recall.length} covered`, missed.length ? JSON.stringify(missed) : "");
  }
  writeFileSync(`${out}/report.json`, JSON.stringify(report, null, 2));
  await browser.close();
  process.exit(0);
}
if (bypass) {
  // Baseline: the processor → worker → generator hop with no safe mode work.
  for (const [name, fn] of [["typing", () => window.bench.type("pay 4111 1111 1111 1111 then call +1 415 555 0100", 110)], ["scroll", () => window.bench.scroll(30, 24, 100)]]) {
    await page.evaluate(() => window.bench.mark());
    await page.evaluate(fn);
    await page.waitForTimeout(300);
    console.log(name, JSON.stringify((await page.evaluate(() => window.bench.measure())).added));
  }
  await browser.close();
  process.exit(0);
}
// 1. Cold start: whole screen unknown, then cleared.
if (run("cold")) {
await page.evaluate(() => window.bench.mark());
await page.evaluate(() => window.bench.reset());
const cold = await page.evaluate(() => window.bench.waitClear(20000));
await page.waitForTimeout(300);
const snapStatic = await page.evaluate(() => window.bench.snapshot());
png("static-before", snapStatic.before);
png("static-after", snapStatic.after);
report.recallStatic = await page.evaluate(() => window.bench.recall());
needles.static = await page.evaluate(() => window.bench.needles());
report.runs.cold = { revealMs: Math.round(cold), ...(await page.evaluate(() => window.bench.measure())) };

}
// 2. Static: nothing changes for 3 s (cost of an idle protected screen).
if (run("static")) {
await page.evaluate(() => window.bench.mark());
await page.waitForTimeout(3000);
report.runs.static = await page.evaluate(() => window.bench.measure());

}
// 3. Typing a card number and a phone number into the terminal.
if (run("typing")) {
await page.evaluate(() => window.bench.mark());
await page.evaluate(() => window.bench.type("pay 4111 1111 1111 1111 then call +1 415 555 0100", 110));
await page.evaluate(() => window.bench.waitClear(5000));
report.runs.typing = await page.evaluate(() => window.bench.measure());
const snapType = await page.evaluate(() => window.bench.snapshot());
report.recallTyping = await page.evaluate(() => window.bench.recall());
png("typing-after", snapType.after);

}
// 4. Scrolling the inbox list.
if (run("scroll")) {
await page.evaluate(() => window.bench.mark());
await page.evaluate(() => window.bench.scroll(30, 24, 100));
await page.evaluate(() => window.bench.waitClear(5000));
report.runs.scroll = await page.evaluate(() => window.bench.measure());

}
// 5. A new window with lots of sensitive text appears.
if (run("popup")) {
await page.evaluate(() => window.bench.mark());
await page.evaluate(() => window.bench.popup(true));
await page.waitForTimeout(60);
const snapPending = await page.evaluate(() => window.bench.snapshot());
png("popup-pending", snapPending.after);
const popupReveal = await page.evaluate(() => window.bench.waitClear(20000));
await page.waitForTimeout(300);
const snapPopup = await page.evaluate(() => window.bench.snapshot());
png("popup-before", snapPopup.before);
png("popup-after", snapPopup.after);
report.recallPopup = await page.evaluate(() => window.bench.recall());
if (needles.static) (needles.popup = await page.evaluate(() => window.bench.needles())), writeFileSync(`${out}/needles.json`, JSON.stringify(needles));
report.runs.popup = { revealMs: Math.round(popupReveal), ...(await page.evaluate(() => window.bench.measure())) };

}
report.texts = await page.evaluate(() => window.bench.texts());
report.log = await page.evaluate(() => window.bench.log());
writeFileSync(`${out}/report.json`, JSON.stringify(report, null, 2));
for (const k of ["recallStatic", "recallTyping", "recallPopup"]) {
  const r = report[k] ?? [];
  const missed = r.filter((x) => x.covered < 90);
  console.log(k, `${r.length - missed.length}/${r.length} fully covered`, missed.length ? "MISSED: " + JSON.stringify(missed) : "");
}
const brief = Object.fromEntries(Object.entries(report.runs).map(([k, v]) => [k, { reveal: v.revealMs, added: v.added, frames: v.frames, pendingFrames: v.framesWithPendingCovers, proc: v.stats.proc, copy: v.stats.copy, diff: v.stats.diff, detect: v.stats.detect, compose: v.stats.compose, hold: v.stats.hold, ocr: v.stats.ocr, backend: v.stats.backend, pass: v.stats.passThrough }]));
console.log(JSON.stringify(brief, null, 1));
await browser.close();
