// The Omarchy plugin's Service.qml (its own headless Quickshell harness)
// against the integrated daemon on 127.0.0.1:47823, the real server, the
// real sender page (launched by the daemon, driven by Playwright with the
// test source) and a real E2EE viewer.
//
//   mise exec -- node test/plugin/real.mjs [PLUGIN_DIR]
// Needs build/passstream, /usr/bin/qs and the Omarchy shell kit, and a free
// port 47823 (the plugin's port check is fixed to it).
import { spawn, spawnSync } from "node:child_process";
import { appendFileSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const plugin = resolve(process.argv[2] ?? "../passstream-omarchy-plugin");
if (!existsSync(plugin)) throw Error("Plugin checkout not found; pass PLUGIN_DIR.");
const pluginDir = plugin;
const H = createRequire(join(pluginDir, "tests/"))("./lib/harness.js");
const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
const bin = resolve(process.env.E2E_SERVER_BIN ?? "build/passstream");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const run = process.env.E2E_RUN ?? `plugin-real-${new Date().toISOString().replaceAll(":", "-")}`;
const out = resolve("test/torture/out", run);
mkdirSync(out, { recursive: true });
const httpPort = 18380;
const origin = `http://127.0.0.1:${httpPort}`;

const base = mkdtempSync(join(tmpdir(), "ps-plugin-"));
const env = { ...process.env, XDG_RUNTIME_DIR: join(base, "run"), XDG_CONFIG_HOME: join(base, "config"), XDG_STATE_HOME: join(base, "state") };
for (const d of ["run", "config/passstream", "state", "launches", "data"]) mkdirSync(join(base, d), { recursive: true, mode: 0o700 });
chmodSync(join(base, "run"), 0o700);
const launches = join(base, "launches");
const wrapper = join(base, "browser.sh");
writeFileSync(wrapper, `#!/bin/sh\nprintf '%s\\n' "$@" > "${launches}/$$.args.tmp" && mv "${launches}/$$.args.tmp" "${launches}/$$.args"\nexec sleep 100000\n`, { mode: 0o755 });
writeFileSync(join(base, "config/passstream/config"), `server = ${origin}\nbrowser = ${wrapper}\n`, { mode: 0o600 });
const dataDir = join(base, "data");

const server = spawn(bin, ["serve", "--data", dataDir, "--listen", `127.0.0.1:${httpPort}`, "--base-url", origin, "--ice-udp", "127.0.0.1:18743", "--ice-tcp", "127.0.0.1:18743", "--turn", "127.0.0.1:13778"], { stdio: ["ignore", "pipe", "pipe"] });
server.stdout.on("data", (b) => appendFileSync(join(out, "server.log"), b));
server.stderr.on("data", (b) => appendFileSync(join(out, "server.log"), b));
for (let i = 0; i < 200; i++) {
  try {
    if ((await fetch(origin + "/readyz")).ok) break;
  } catch {}
  await sleep(25);
}
writeFileSync(join(base, "config/passstream/key"), readFileSync(join(dataDir, "owner-key"), "utf8").trim() + "\n", { mode: 0o600 });
const daemon = spawn(bin, ["host", "--idle-exit", "-1s"], { env, stdio: ["ignore", "pipe", "pipe"] });
daemon.stdout.on("data", (b) => appendFileSync(join(out, "daemon.log"), b));
daemon.stderr.on("data", (b) => appendFileSync(join(out, "daemon.log"), b));
const cli = (args) => {
  const p = spawnSync(bin, args, { env, encoding: "utf8", timeout: 20000 });
  try {
    return JSON.parse(p.stdout);
  } catch {
    return null;
  }
};
for (let i = 0; i < 200 && !cli(["status", "--json"]); i++) await sleep(25);

const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true, args: ["--no-sandbox", "--allow-loopback-in-peer-connection", "--autoplay-policy=no-user-gesture-required", "--disable-background-timer-throttling", "--disable-renderer-backgrounding"] });
let viewerState = null;
// Play the sender window the daemon launches, then join as a viewer with the PIN.
const driver = (async () => {
  let file;
  for (let i = 0; i < 1200 && !file; i++) {
    file = readdirSync(launches).find((f) => f.endsWith(".args"));
    if (!file) await sleep(50);
  }
  if (!file) return;
  const app = readFileSync(join(launches, file), "utf8").split("\n").find((a) => a.startsWith("--app=")).slice(6);
  const url = readFileSync(new URL(app), "utf8").match(/url=([^"'>\s]+)/i)[1];
  const sender = await (await browser.newContext()).newPage();
  await sender.goto(url.replace("/sender#", "/sender?test=1#"));
  await sender.getByTestId("test-source").click({ timeout: 15000 });
  await sender.waitForFunction(() => window.passstreamSender?.phase === "live", null, { timeout: 20000 });
  const link = await sender.evaluate(() => window.passstreamSender.link);
  const pin = cli(["pin", "--reveal", "--json"])?.pin;
  const v = await (await browser.newContext()).newPage();
  await v.goto(link);
  await v.getByTestId("pin-screen").waitFor({ timeout: 15000 });
  await v.locator("input").first().fill(pin);
  await v.waitForFunction(() => window.passstream?.firstFrame > 0, null, { timeout: 20000 });
  viewerState = await v.evaluate(() => ({ ttff: window.passstream.firstFrame, events: window.passstreamViewer.events.map((e) => e.kind) }));
})().catch((e) => (viewerState = { error: String(e) }));

const steps = `[
  { name: "ready", until: function() { return svc.ready && svc.statusKnown && svc.stream === null }, then: function() { h.log("start", svc.goLive()) } },
  { name: "live", timeout: 40000, until: function() { return svc.stream && svc.stream.phase === "live" } },
  { name: "watching", timeout: 30000, until: function() { return svc.stream.watching === 1 }, then: function() { h.log("viewers", svc.stream.viewers.map(function(v) { return { status: v.status, code: v.row_code } })); svc.pause() } },
  { name: "paused", until: function() { return svc.stream.phase === "paused" && svc.busyOp === "" }, then: function() { svc.togglePause() } },
  { name: "resumed", until: function() { return svc.stream.phase === "live" && svc.busyOp === "" }, then: function() { svc.toggleLock() } },
  { name: "locked", until: function() { return svc.stream.access.latches.locked && svc.busyOp === "" }, then: function() { svc.revealPin() } },
  { name: "revealed", until: function() { return /^\\d{4}$/.test(svc.revealedPin) }, then: function() { h.log("e2ee", svc.stream.e2ee); svc.stop() } },
  { name: "stopped", timeout: 15000, until: function() { return svc.stream === null } }
]`;
const dir = H.configDir(H.serviceShell(steps));
const stubs = H.stubBin(dir);
const result = await H.runQs(dir, env.XDG_RUNTIME_DIR, 150000, stubs.env);
await driver;
const records = result.records;
const passed = records.some((r) => r.kind === "done") && !records.some((r) => r.kind === "timeout" || r.kind === "error");
const summary = {
  run,
  pass: passed && !viewerState?.error && viewerState?.events?.includes("admitted"),
  steps: records.filter((r) => r.kind === "step").map((r) => r.data),
  failures: records.filter((r) => r.kind === "timeout" || r.kind === "error"),
  viewers_seen_by_plugin: records.find((r) => r.kind === "viewers")?.data,
  e2ee_seen_by_plugin: records.find((r) => r.kind === "e2ee")?.data,
  ended: records.find((r) => r.kind === "ended")?.data,
  live_notice: records.find((r) => r.kind === "liveNotice")?.data,
  viewer: viewerState,
  desktop_calls: stubs.calls().map((c) => c.argv?.[0] ?? c.name ?? c).slice(0, 30),
};
writeFileSync(join(out, "result.json"), JSON.stringify(summary, null, 2));
writeFileSync(`test/torture/results/${run}.json`, JSON.stringify({ ...summary, created: new Date().toISOString(), commit: spawnSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).stdout.trim() }, null, 2));
console.log(JSON.stringify(summary, null, 2));
if (!passed) console.log(result.output.slice(-3000));
await browser.close();
cli(["shutdown", "--force", "--json"]);
daemon.kill("SIGKILL");
for (const f of readdirSync(launches)) if (f.endsWith(".args")) try { process.kill(Number(f.split(".")[0]), "SIGKILL"); } catch {}
server.kill("SIGKILL");
process.exit(summary.pass ? 0 : 1);
