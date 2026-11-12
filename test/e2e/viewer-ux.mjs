// Viewer UX state transitions against the real stack: the real server, the
// real local daemon and CLI, the real sender page and real viewers in
// headless system Chromium emulating a phone (390×844, touch, DPR 3). Each
// test records every screen the viewer drew (window.passstreamViewer.screen,
// sampled every 50 ms in the page) and checks the DOM of each state.
// Screenshots: docs/screenshots/viewer/real-390/.
//
//   /tmp/passstream-briefs/media-run mise exec -- node test/e2e/viewer-ux.mjs   (needs build/passstream)
//   UX_FILTER=pause ...
import { spawn, spawnSync } from "node:child_process";
import { appendFileSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
const bin = resolve(process.env.E2E_SERVER_BIN ?? "build/passstream");
const run = process.env.UX_RUN ?? `viewer-ux-${new Date().toISOString().replaceAll(":", "-")}`;
const filter = process.env.UX_FILTER ? new RegExp(process.env.UX_FILTER) : null;
const out = resolve("test/torture/out", run);
const shots = resolve("docs/screenshots/viewer/real-390");
mkdirSync(out, { recursive: true });
mkdirSync(shots, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// Ports apart from test/e2e/e2ee.mjs.
const httpPort = 18380, daemonPort = 47933;
const origin = `http://127.0.0.1:${httpPort}`;

// ---------------------------------------------------------------- environment
const base = mkdtempSync(join(tmpdir(), "ps-ux-"));
const env = { ...process.env, XDG_RUNTIME_DIR: join(base, "run"), XDG_CONFIG_HOME: join(base, "config"), XDG_STATE_HOME: join(base, "state") };
for (const d of ["run", "config/passstream", "state", "launches", "data"]) mkdirSync(join(base, d), { recursive: true, mode: 0o700 });
chmodSync(join(base, "run"), 0o700);
const launches = join(base, "launches");
const wrapper = join(base, "browser.sh");
writeFileSync(wrapper, `#!/bin/sh\nprintf '%s\\n' "$@" > "${launches}/$$.args.tmp" && mv "${launches}/$$.args.tmp" "${launches}/$$.args"\nexec sleep 100000\n`, { mode: 0o755 });
writeFileSync(join(base, "config/passstream/config"), `server = ${origin}\nport = ${daemonPort}\nbrowser = ${wrapper}\n`, { mode: 0o600 });
const dataDir = join(base, "data");

let server, daemon;
async function startServer() {
  server = spawn(bin, ["serve", "--data", dataDir, "--listen", `127.0.0.1:${httpPort}`, "--base-url", origin, "--ice-udp", "127.0.0.1:18743", "--ice-tcp", "127.0.0.1:18743", "--turn", "127.0.0.1:13778"], { stdio: ["ignore", "pipe", "pipe"] });
  server.stdout.on("data", (b) => appendFileSync(join(out, "server.log"), b));
  server.stderr.on("data", (b) => appendFileSync(join(out, "server.log"), b));
  for (let i = 0; i < 200; i++) {
    try {
      if ((await fetch(origin + "/readyz")).ok) break;
    } catch {}
    await sleep(25);
  }
  const keyFile = join(base, "config/passstream/key");
  if (!existsSync(keyFile)) writeFileSync(keyFile, readFileSync(join(dataDir, "owner-key"), "utf8").trim() + "\n", { mode: 0o600 });
}
async function startDaemon() {
  daemon = spawn(bin, ["host", "--idle-exit", "-1s"], { env, stdio: ["ignore", "pipe", "pipe"] });
  daemon.stdout.on("data", (b) => appendFileSync(join(out, "daemon.log"), b));
  daemon.stderr.on("data", (b) => appendFileSync(join(out, "daemon.log"), b));
  for (let i = 0; i < 200; i++) {
    if (existsSync(join(env.XDG_RUNTIME_DIR, "passstream/host.sock")) && cli(["status", "--json"]).code === 0) return;
    await sleep(25);
  }
  throw Error("daemon did not start");
}
function cli(args) {
  const p = spawnSync(bin, args, { env, encoding: "utf8", timeout: 30000 });
  let json = null;
  try {
    json = JSON.parse(p.stdout);
  } catch {}
  return { code: p.status, stdout: p.stdout, stderr: p.stderr, json };
}
function cliAsync(args) {
  return new Promise((res) => {
    const p = spawn(bin, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    p.stdout.on("data", (b) => (stdout += b));
    p.stderr.on("data", (b) => (stderr += b));
    p.on("exit", (code) => {
      let json = null;
      try {
        json = JSON.parse(stdout);
      } catch {}
      res({ code, stdout, stderr, json });
    });
  });
}

// ---------------------------------------------------------------- browsers
let browser, senderCtx;
const PHONE = {
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 3,
  isMobile: true,
  hasTouch: true,
  userAgent: "Mozilla/5.0 (Linux; Android 15; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
};
function log(p, who) {
  p.on("pageerror", (e) => appendFileSync(join(out, "browser.log"), `[${who}] pageerror ${e}\n`));
  p.on("console", (m) => m.type() === "error" && appendFileSync(join(out, "browser.log"), `[${who}] ${m.text()}\n`));
}
/** A phone viewer that records every screen it draws. */
async function viewer(link, { pin = null, ctx = null } = {}) {
  const c = ctx ?? (await browser.newContext(PHONE));
  await c.addInitScript(() => {
    const seen = [];
    window.__screens = seen;
    setInterval(() => {
      const s = window.passstreamViewer?.screen;
      if (!s) return;
      const k = s.kind === "pin" ? `pin:${s.checking ? "checking" : s.lockout ? "lockout" : "entry"}` : s.kind === "reconnecting" ? `reconnecting:${s.cause}${s.long ? ":long" : ""}` : s.kind === "connecting" && s.slow ? "connecting:slow" : s.kind;
      if (seen.at(-1)?.k !== k) seen.push({ k, at: Date.now() });
    }, 50);
  });
  const p = await c.newPage();
  log(p, "viewer");
  await p.goto(link);
  if (pin !== null) await typePin(p, pin);
  return p;
}
async function typePin(p, pin) {
  await p.getByTestId("pin-screen").waitFor({ timeout: 20000 });
  const input = p.getByTestId("pin-input");
  await input.waitFor({ state: "visible" });
  await input.tap();
  await p.keyboard.type(pin, { delay: 30 });
}
const screens = (p) => p.evaluate(() => window.__screens.map((s) => s.k));
const screenNow = (p) => p.evaluate(() => window.__screens.at(-1)?.k ?? null);
async function until(p, k, timeout = 20000) {
  await p.waitForFunction((k) => window.__screens.at(-1)?.k === k, k, { timeout, polling: 50 });
}
async function shot(p, name) {
  await p.screenshot({ path: join(shots, `${name}.png`), animations: "disabled", scale: "css" });
}
/** Is `want` a subsequence of `got`? */
const inOrder = (got, want) => {
  let i = 0;
  for (const g of got) if (g === want[i]) i++;
  return i === want.length;
};

let currentSender = null;
async function share(args = []) {
  const before = new Set(readdirSync(launches));
  const pending = cliAsync(["share", "--no-wait", "--json", "--no-safe", ...args]);
  let file;
  for (let i = 0; i < 400 && !file; i++) {
    file = readdirSync(launches).find((f) => f.endsWith(".args") && !before.has(f));
    if (!file) await sleep(25);
  }
  if (!file) throw Error(`no browser launch: ${JSON.stringify(await pending)}`);
  const app = readFileSync(join(launches, file), "utf8").split("\n").find((a) => a.startsWith("--app=")).slice(6);
  const url = readFileSync(new URL(app), "utf8").match(/url=([^"'>\s]+)/i)?.[1]?.replaceAll("&amp;", "&");
  const p = await senderCtx.newPage();
  log(p, "sender");
  // No tone: the sender's test source is video only.
  await p.goto(url.replace("/sender#", "/sender?test=1#"));
  await p.getByTestId("test-source").click({ timeout: 15000 });
  const r = await pending;
  if (r.code !== 0) throw Error(`share failed: ${r.stdout} ${r.stderr}`);
  await p.waitForFunction(() => window.passstreamSender?.phase === "live", null, { timeout: 15000 });
  currentSender = p;
  return { page: p, link: r.json.link, pin: cli(["pin", "--reveal", "--json"]).json?.pin ?? null };
}
async function stopShare() {
  cli(["stop", "--json"]);
  for (let i = 0; i < 100; i++) {
    if (cli(["status", "--json"]).json?.stream == null) return;
    await sleep(50);
  }
}

const results = [];
async function test(name, fn) {
  if (filter && !filter.test(name)) return;
  const started = Date.now();
  let r;
  try {
    r = { pass: false, ...(await fn()) };
  } catch (e) {
    r = { pass: false, error: String(e).slice(0, 800) };
  }
  r = { name, ms: Date.now() - started, ...r };
  results.push(r);
  console.log(`${r.pass ? "PASS" : "FAIL"} ${name} ${JSON.stringify({ ...r, name: undefined })}`);
  if (server.exitCode !== null || server.signalCode) await startServer();
  try {
    await stopShare();
  } catch {}
  for (const c of browser.contexts()) if (c !== senderCtx) await c.close().catch(() => {});
  for (const p of senderCtx.pages()) await p.close().catch(() => {});
}

await startServer();
await startDaemon();
browser = await chromium.launch({
  executablePath: "/usr/bin/chromium",
  headless: true,
  args: ["--no-sandbox", "--allow-loopback-in-peer-connection", "--disable-dev-shm-usage", "--autoplay-policy=no-user-gesture-required", "--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows"],
});
senderCtx = await browser.newContext();

try {
  await test("pin_checking_admitted_live", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link);
    await until(v, "pin:entry");
    await shot(v, "pin");
    const input = v.getByTestId("pin-input");
    const attrs = await input.evaluate((e) => [e.inputMode, e.getAttribute("autocomplete"), e.getAttribute("pattern")]);
    await input.tap();
    await v.keyboard.type(s.pin.slice(0, 3), { delay: 30 });
    await v.keyboard.type(s.pin.slice(3));
    await until(v, "live");
    await v.getByTestId("live-badge").waitFor();
    await shot(v, "live");
    const seq = await screens(v);
    return { pass: inOrder(seq, ["pin:entry", "live"]) && attrs.join() === "numeric,one-time-code,[0-9]*" && !seq.some((k) => k.startsWith("reconnecting")), screens: seq, input: attrs };
  });

  await test("wrong_pin_tries_then_lockout_survives_reload", async () => {
    const s = await share(["--pin", "4"]);
    const wrong = s.pin === "0000" ? "1111" : "0000";
    const v = await viewer(s.link);
    const hints = [];
    for (let i = 0; i < 5; i++) {
      await typePin(v, wrong);
      await v.waitForFunction((n) => window.passstreamViewer.events.filter((e) => e.kind === "refused").length >= n, i + 1, { timeout: 10000 });
      hints.push(await v.getByTestId("pin-hint").textContent().catch(() => null));
    }
    await until(v, "pin:lockout");
    const disabled = await v.getByTestId("pin-input").isDisabled();
    const c1 = await v.getByTestId("pin-countdown").textContent();
    await shot(v, "pin-lockout");
    await v.reload();
    await until(v, "pin:lockout");
    const c2 = await v.getByTestId("pin-countdown").textContent();
    const seq = await screens(v);
    return { pass: disabled && /^[0-9]:[0-9]{2}$/.test(c1) && /^[0-9]:[0-9]{2}$/.test(c2) && hints.some((h) => /tries left/.test(h ?? "")), hints, countdown: [c1, c2], screens_after_reload: seq };
  });

  await test("knock_then_approved_live", async () => {
    const s = await share(["--pin", "4", "--knock"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "knock");
    const code = (await v.getByTestId("own-code").textContent()).trim();
    await shot(v, "knock");
    let knock;
    for (let i = 0; i < 40 && !knock; i++) {
      knock = cli(["viewers", "--json"]).json?.viewers?.find((r) => r.status === "knocking");
      if (!knock) await sleep(100);
    }
    cli(["knock", "approve", knock.knock_id, "--code", knock.row_code, "--json"]);
    await until(v, "live");
    const seq = await screens(v);
    return { pass: inOrder(seq, ["pin:entry", "knock", "live"]) && code.startsWith(knock.row_code), screens: seq };
  });

  await test("paused_cover_then_resume", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    cli(["pause", "--json"]);
    await until(v, "paused", 10000);
    await v.getByTestId("paused-cover").waitFor();
    // The cover is opaque and on top of the whole stage; no LIVE badge.
    const covered = await v.evaluate(() => {
      const hits = [];
      for (let i = 0; i < 4; i++) for (let j = 0; j < 4; j++) hits.push(document.elementFromPoint(((i + 0.5) / 4) * innerWidth, ((j + 0.5) / 4) * innerHeight)?.closest("[data-testid=zoom-layer]") ? "video" : "ok");
      return { video_hits: hits.filter((h) => h === "video").length, bg: getComputedStyle(document.querySelector("[data-testid=paused-cover]")).backgroundColor, live: !!document.querySelector("[data-testid=live-badge]") };
    });
    await shot(v, "paused");
    cli(["resume", "--json"]);
    await until(v, "live", 10000);
    const seq = await screens(v);
    return { pass: covered.video_hits === 0 && covered.bg === "rgb(18, 16, 14)" && !covered.live && inOrder(seq, ["live", "paused", "live"]), covered, screens: seq };
  });

  await test("short_outage_never_shows_reconnecting", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    await sleep(1500);
    // Freeze the server (media and signaling) for 1 s: the last frame holds.
    server.kill("SIGSTOP");
    await sleep(1000);
    server.kill("SIGCONT");
    await sleep(4000);
    const seq = await screens(v);
    const badge = await v.getByTestId("state-badge").count();
    const sessionStates = await v.evaluate(() => window.passstream.states.map((s) => s.state));
    return { pass: seq.at(-1) === "live" && !seq.some((k) => k.startsWith("reconnecting")) && badge === 0, screens: seq, session_states: sessionStates };
  });

  await test("long_outage_calm_reconnecting_then_live", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    await sleep(1500);
    // Kill the server and bring it back after 6 s: the viewer re-joins and re-admits by grant.
    server.kill("SIGKILL");
    await new Promise((r) => server.once("exit", r));
    const t0 = Date.now();
    await v.waitForFunction(() => window.__screens.at(-1)?.k.startsWith("reconnecting"), null, { timeout: 15000 });
    const shownAfter = Date.now() - t0;
    const text = await v.getByTestId("state-badge").textContent();
    await shot(v, "reconnecting");
    await sleep(Math.max(0, 6000 - (Date.now() - t0)));
    await startServer();
    await until(v, "live", 30000);
    const seq = await screens(v);
    const scary = /error|failed|lost|reload the page/i.test(text);
    // After the first live frame: only live and calm reconnecting screens, never a card.
    const after = seq.slice(seq.indexOf("live"));
    return { pass: shownAfter >= 1500 && !scary && seq.at(-1) === "live" && after.every((k) => k === "live" || k.startsWith("reconnecting")), shown_after_ms: shownAfter, badge: text, screens: seq };
  });

  await test("kicked_card_no_picture", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    await sleep(1000);
    const code = await v.evaluate(() => window.passstreamViewer.rowCode);
    const row = cli(["viewers", "--json"]).json.viewers.find((r) => r.row_code === code);
    cli(["kick", row.admission_ids[0], "--json"]);
    await until(v, "removed", 10000);
    await v.getByTestId("state-removed-kicked").waitFor();
    const videos = await v.locator("video").count();
    const focused = await v.evaluate(() => document.activeElement?.tagName);
    await shot(v, "kicked");
    return { pass: videos === 0 && focused === "H1", videos, focused, screens: await screens(v) };
  });

  await test("host_stops_ended_card", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    const st = cli(["stop", "--json"]);
    try {
      await until(v, "ended", 15000);
    } catch (e) {
      const d = await v.evaluate(() => ({ screens: window.__screens, view: window.passstreamViewer.view, marker: window.passstream?.marker, state: window.passstream?.state, events: window.passstreamViewer.events.slice(-8), sev: window.passstream?.events.slice(-8) }));
      return { error: String(e).slice(0, 200), stop: st, diag: d };
    }
    await v.getByTestId("state-ended").waitFor();
    await shot(v, "ended");
    const seq = await screens(v);
    return { pass: seq.at(-1) === "ended" && (await v.locator("video").count()) === 0, screens: seq };
  });

  // Review round 1, finding 1: a 1008 close is terminal at once, and nothing
  // queued can bring the page back to live.
  await test("ended_is_terminal_synchronously_and_final", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    cli(["stop", "--json"]);
    await until(v, "ended", 15000);
    const t = await v.evaluate(() => {
      const close = window.passstream.events.find((e) => e.kind === "ws-close" && e.detail === "1008");
      const terminal = window.passstreamViewer.events.find((e) => e.kind === "terminal");
      return { closeAt: close?.at ?? null, terminalAt: terminal?.at ?? null, code: terminal?.detail ?? null, disposed: window.passstreamViewer.disposed };
    });
    // A queued video-frame callback and a late admission answer land after the close.
    await v.evaluate(() => {
      window.passstream.stateTo("live");
      window.passstream.stateTo("reconnecting");
      window.passstreamViewer.setView({ kind: "media" });
      window.passstreamViewer.changed();
    });
    await sleep(1500);
    const after = await v.evaluate(() => ({ state: window.passstream.state, view: window.passstreamViewer.view.kind, videos: document.querySelectorAll("video").length }));
    const seq = await screens(v);
    const gap = t.closeAt !== null && t.terminalAt !== null ? t.terminalAt - t.closeAt : null;
    return {
      // Before the fix the terminal ran on the next 500 ms poll and a frame callback could flip the session back to live.
      pass: t.code === "stream_ended" && gap !== null && gap < 50 && t.disposed && after.state === "ended" && after.view === "ended" && after.videos === 0 && seq.at(-1) === "ended" && seq.indexOf("ended") === seq.length - 1,
      close_to_terminal_ms: gap,
      after,
      screens: seq,
    };
  });

  await test("paused_then_stopped_ends_and_stays_ended", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    cli(["pause", "--json"]);
    await until(v, "paused", 10000);
    cli(["stop", "--json"]);
    await until(v, "ended", 15000);
    await sleep(1500);
    const seq = await screens(v);
    const st = await v.evaluate(() => ({ disposed: window.passstreamViewer.disposed, state: window.passstream.state, videos: document.querySelectorAll("video").length }));
    return { pass: inOrder(seq, ["live", "paused", "ended"]) && seq.at(-1) === "ended" && st.disposed && st.state === "ended" && st.videos === 0, screens: seq, ...st };
  });

  await test("kicked_is_final_and_disposed", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await until(v, "live");
    await sleep(500);
    const code = await v.evaluate(() => window.passstreamViewer.rowCode);
    const row = cli(["viewers", "--json"]).json.viewers.find((r) => r.row_code === code);
    cli(["kick", row.admission_ids[0], "--json"]);
    await until(v, "removed", 10000);
    // Then the stream ends: the kicked card stays (no transition out of removed).
    cli(["stop", "--json"]);
    await sleep(2000);
    await v.evaluate(() => window.passstreamViewer.setView({ kind: "media" }));
    await sleep(300);
    const st = await v.evaluate(() => ({ view: window.passstreamViewer.view, disposed: window.passstreamViewer.disposed, state: window.passstream.state, videos: document.querySelectorAll("video").length, kicked: !!document.querySelector("[data-testid=state-removed-kicked]") }));
    const seq = await screens(v);
    return { pass: st.view.kind === "removed" && st.disposed && st.state === "ended" && st.videos === 0 && st.kicked && seq.at(-1) === "removed" && !seq.slice(seq.indexOf("removed")).some((k) => k !== "removed"), screens: seq, ...st };
  });

  // Finding 2: an admission answer that completes after a rejoin replaced its
  // admission sends nothing and leaves the typed PIN to the current attempt.
  await test("stale_admission_answer_has_no_effect", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link);
    await until(v, "pin:entry");
    await v.evaluate(() => {
      const c = window.passstreamViewer;
      const st = (window.__stale = { held: false, released: false, staleSends: 0, conn: null, release: null });
      const send = c.send.bind(c);
      c.send = (bytes) => {
        if (bytes.__stale) st.staleSends++;
        return send(bytes);
      };
      const receive = c.receive.bind(c);
      let armed = true;
      c.receive = (b) => {
        const adm = c.adm;
        if (armed && adm && adm.state === "hello_sent") {
          armed = false;
          const handle = adm.handle.bind(adm);
          // Hold this admission's answer to the signed CHALLENGE until the test releases it.
          adm.handle = async (bytes) => {
            const outs = await handle(bytes);
            st.held = true;
            st.conn = c.connectionId;
            await new Promise((r) => (st.release = r));
            for (const o of outs) o.__stale = true;
            return outs;
          };
        }
        return receive(b);
      };
    });
    await typePin(v, s.pin);
    await v.waitForFunction(() => window.__stale.held, null, { timeout: 15000 });
    // Signaling drops and rejoins while the answer is held: a new admission starts with the kept PIN.
    await v.evaluate(() => window.passstream.ws.close());
    await v.waitForFunction(() => window.passstreamViewer.connectionId && window.passstreamViewer.connectionId !== window.__stale.conn, null, { timeout: 15000 });
    await v.evaluate(() => {
      window.__stale.released = true;
      window.__stale.release();
    });
    await until(v, "live", 20000);
    await sleep(500);
    const st = await v.evaluate(() => ({ staleSends: window.__stale.staleSends, refused: window.passstreamViewer.events.filter((e) => e.kind === "refused").map((e) => e.detail), admissions: window.passstreamViewer.admissions, pinError: window.passstreamViewer.pinError }));
    const seq = await screens(v);
    return { pass: st.staleSends === 0 && st.refused.length === 0 && st.admissions === 1 && st.pinError === null && seq.at(-1) === "live", screens: seq, ...st };
  });

  // Finding 6: a name typed on the PIN screen reaches the host before the first knock.
  await test("name_reaches_host_before_first_knock", async () => {
    const s = await share(["--pin", "4", "--knock"]);
    const v = await viewer(s.link);
    await until(v, "pin:entry");
    await v.getByTestId("name-input").fill("Review name");
    await typePin(v, s.pin);
    await until(v, "knock");
    // The first knocking row the host sees already carries the name.
    // (The knock row's connections are attached once the server reports them; wait for that.)
    let knock = null;
    for (let i = 0; i < 100 && !knock?.connections?.length; i++) {
      knock = cli(["viewers", "--json"]).json?.viewers?.find((r) => r.status === "knocking") ?? null;
      if (!knock?.connections?.length) await sleep(50);
    }
    const labels = (knock?.connections ?? []).map((c) => c.reported?.label ?? null);
    const ev = await v.evaluate(() => window.passstreamViewer.events.filter((e) => ["rejoin", "joined", "hello"].includes(e.kind)).map((e) => e.kind));
    cli(["knock", "approve", knock.knock_id, "--code", knock.row_code, "--json"]);
    await until(v, "live");
    return { pass: labels.includes("Review name") && ev.indexOf("rejoin") >= 0 && ev.indexOf("rejoin") < ev.lastIndexOf("hello") && ev.filter((k) => k === "rejoin").length === 1, labels, events: ev, knock_row: knock && { status: knock.status, row_code: knock.row_code } };
  });
} finally {
  await browser.close().catch(() => {});
  daemon?.kill("SIGKILL");
  server?.kill("SIGCONT");
  server?.kill("SIGKILL");
}

const record = { run, created: new Date().toISOString(), commit: spawnSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).stdout.trim(), chromium: spawnSync("/usr/bin/chromium", ["--version"], { encoding: "utf8" }).stdout.trim(), results };
writeFileSync(join(out, "results.json"), JSON.stringify(record, null, 2));
console.log("\n| Test | Result |\n|---|---|\n" + results.map((r) => `| ${r.name} | ${r.pass ? "PASS" : "**FAIL**"} |`).join("\n"));
process.exit(results.every((r) => r.pass) ? 0 : 1);
