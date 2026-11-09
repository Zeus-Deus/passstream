// Track D2 features against the real stack (FEATURES.md "Track D2"): the real
// server, the real local daemon and CLI, the real sender page and real phone
// viewers in headless system Chromium.
//
//   curtain: command → last real frame, measured with the test source's
//            capture-time clock row decoded on the viewer; no real frame
//            captured after the command reaches a viewer; paused cover drawn.
//   annotations: round trip between two viewers through the host, host laser,
//            a kicked viewer's pre-sealed messages refused, and the relay sees
//            only ciphertext (a secret needle in the stroke never appears in
//            any signaling frame or server/daemon log).
//   screenshots: HUD and annotations at 320 and 390 px (docs/screenshots/features/).
//
//   /tmp/passstream-briefs/media-run mise exec -- node test/e2e/features-ui.mjs   (needs build/passstream)
//   FEAT_FILTER=curtain ...   CURTAIN_TRIALS=10 ...
import { spawn, spawnSync } from "node:child_process";
import dgram from "node:dgram";
import { appendFileSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
const bin = resolve(process.env.E2E_SERVER_BIN ?? "build/passstream");
const run = process.env.FEAT_RUN ?? `features-ui-${new Date().toISOString().replaceAll(":", "-")}`;
const filter = process.env.FEAT_FILTER ? new RegExp(process.env.FEAT_FILTER) : null;
const TRIALS = Number(process.env.CURTAIN_TRIALS ?? 5);
const out = resolve("test/torture/out", run);
const shots = resolve(process.env.FEAT_SCREENSHOT_DIR ?? "docs/screenshots/features");
mkdirSync(out, { recursive: true });
mkdirSync(shots, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// Ports apart from e2ee.mjs and viewer-ux.mjs.
const httpPort = 18480, daemonPort = 48033;
const origin = `http://127.0.0.1:${httpPort}`;

// ---------------------------------------------------------------- environment
const base = mkdtempSync(join(tmpdir(), "ps-feat-"));
const env = { ...process.env, XDG_RUNTIME_DIR: join(base, "run"), XDG_CONFIG_HOME: join(base, "config"), XDG_STATE_HOME: join(base, "state") };
for (const d of ["run", "config/passstream", "state", "launches", "data"]) mkdirSync(join(base, d), { recursive: true, mode: 0o700 });
chmodSync(join(base, "run"), 0o700);
const launches = join(base, "launches");
const wrapper = join(base, "browser.sh");
writeFileSync(wrapper, `#!/bin/sh\nprintf '%s\\n' "$@" > "${launches}/$$.args.tmp" && mv "${launches}/$$.args.tmp" "${launches}/$$.args"\nexec sleep 100000\n`, { mode: 0o755 });
writeFileSync(join(base, "config/passstream/config"), `server = ${origin}\nport = ${daemonPort}\nbrowser = ${wrapper}\n`, { mode: 0o600 });
const dataDir = join(base, "data");

let server, daemon;
// Whatever ends this process (a crash included), its server and daemon go with it.
process.on("exit", () => {
  daemon?.kill("SIGKILL");
  server?.kill("SIGCONT");
  server?.kill("SIGKILL");
});
async function startServer() {
  server = spawn(bin, ["serve", "--data", dataDir, "--listen", `127.0.0.1:${httpPort}`, "--base-url", origin, "--ice-udp", "127.0.0.1:18843", "--ice-tcp", "127.0.0.1:18843", "--turn", "127.0.0.1:13878"], { stdio: ["ignore", "pipe", "pipe"] });
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
/**
 * A UDP proxy that delays every datagram by `delayMs` in each direction: a
 * real delayed media path for one viewer, whose page is rewritten to reach
 * the SFU only through it (see `delayedPath`).
 */
function udpDelayProxy(listenPort, targetPort, delayMs) {
  const front = dgram.createSocket("udp4");
  const ups = new Map();
  let closed = false;
  const later = (f) => setTimeout(() => !closed && f(), delayMs);
  front.on("message", (msg, from) => {
    const key = `${from.address}:${from.port}`;
    let up = ups.get(key);
    if (!up) {
      up = dgram.createSocket("udp4");
      up.on("message", (m) => later(() => front.send(m, from.port, from.address)));
      up.bind(0, "127.0.0.1");
      ups.set(key, up);
    }
    later(() => up.send(msg, targetPort, "127.0.0.1"));
  });
  front.bind(listenPort, "127.0.0.1");
  return { close: () => ((closed = true), front.close(), [...ups.values()].forEach((u) => u.close())) };
}
/** Page init script: no TURN, no ICE-TCP, and the SFU's UDP host candidate rewritten to the proxy. */
function delayedPath([sfuPort, proxyPort]) {
  const fix = (line) => (/ tcp |typ relay|typ srflx/i.test(line) ? null : line.replace(new RegExp(` (udp \\d+ 127\\.0\\.0\\.1) ${sfuPort} typ host`, "i"), ` $1 ${proxyPort} typ host`));
  const Orig = window.RTCPeerConnection;
  window.RTCPeerConnection = function (config) {
    const pc = new Orig({ ...(config ?? {}), iceServers: [] });
    const srd = pc.setRemoteDescription.bind(pc);
    pc.setRemoteDescription = (d) => srd(d?.sdp ? { type: d.type, sdp: d.sdp.split(/\r\n/).map((l) => (l.startsWith("a=candidate:") ? fix(l) : l)).filter((l) => l !== null).join("\r\n") } : d);
    const aic = pc.addIceCandidate.bind(pc);
    pc.addIceCandidate = (c) => {
      if (!c?.candidate) return aic(c);
      const line = fix(c.candidate);
      return line === null ? Promise.resolve() : aic({ ...c, candidate: line });
    };
    return pc;
  };
  window.RTCPeerConnection.prototype = Orig.prototype;
}

/** The server's remote command API, as the dashboard or an agent would call it. */
async function remote(streamId, op, args = {}) {
  const key = readFileSync(join(dataDir, "owner-key"), "utf8").trim();
  const r = await fetch(`${origin}/api/v1/streams/${streamId}/commands`, { method: "POST", headers: { authorization: `Bearer ${key}`, "content-type": "application/json" }, body: JSON.stringify({ op, args }) });
  return { status: r.status, body: await r.json().catch(() => null) };
}

// ---------------------------------------------------------------- browsers
let browser, senderCtx;
const phone = (w, h) => ({
  viewport: { width: w, height: h },
  deviceScaleFactor: 3,
  isMobile: true,
  hasTouch: true,
  userAgent: "Mozilla/5.0 (Linux; Android 15; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
});
function log(p, who) {
  p.on("pageerror", (e) => appendFileSync(join(out, "browser.log"), `[${who}] pageerror ${e}\n`));
  p.on("console", (m) => m.type() === "error" && appendFileSync(join(out, "browser.log"), `[${who}] ${m.text()}\n`));
}
/** Every signaling WebSocket frame a page sends or receives: exactly what the server sees of it. */
const wsFrames = [];
function tapSignaling(p, who) {
  p.on("websocket", (ws) => {
    ws.on("framesent", (f) => wsFrames.push({ who, dir: "sent", payload: typeof f.payload === "string" ? f.payload : Buffer.from(f.payload).toString("latin1") }));
    ws.on("framereceived", (f) => wsFrames.push({ who, dir: "recv", payload: typeof f.payload === "string" ? f.payload : Buffer.from(f.payload).toString("latin1") }));
  });
}

/**
 * In the viewer page: every rendered video frame with its size and, for the
 * test source (960×540), the capture time from its 48-bit clock row. The
 * sender's "Paused" picture is 1280×720, so frame size alone tells real
 * frames from curtain frames.
 */
function frameRecorder() {
  window.__frames = [];
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  let attached = null;
  const attach = () => {
    const v = document.querySelector("[data-testid=video]");
    if (!v || v === attached) return;
    attached = v;
    const cb = (_now, md) => {
      const w = md.width, h = md.height;
      const rec = { at: Date.now(), w, h, real: false, cap: null };
      if (canvas.width !== w || canvas.height !== h) (canvas.width = w), (canvas.height = h);
      ctx.drawImage(v, 0, 0, w, h);
      // The test source is bright (#eaf1f5); the curtain's "Paused" picture is dark (#101418). Any rendition.
      const mid = ctx.getImageData(Math.floor(w * 0.6), Math.floor(h * 0.75), Math.max(1, Math.floor(w * 0.3)), Math.max(1, Math.floor(h * 0.15))).data;
      let sum = 0;
      for (let i = 0; i < mid.length; i += 4) sum += (mid[i] + mid[i + 1] + mid[i + 2]) / 3;
      rec.luma = Math.round(sum / (mid.length / 4));
      rec.real = rec.luma > 128;
      if (rec.real) {
        // The 48-bit capture clock: 12 px cells in the top 36 px of a 960 px frame, scaled. Trust it only when every cell is clean.
        const k = w / 960;
        const strip = ctx.getImageData(0, 0, w, Math.max(1, Math.ceil(36 * k))).data;
        let ts = 0, clean = true;
        for (let bit = 0; bit < 48; bit++) {
          const x = Math.min(w - 1, Math.floor((bit * 12 + 6) * k)), y = Math.floor(18 * k);
          const i = (y * w + x) * 4;
          const l = (strip[i] + strip[i + 1] + strip[i + 2]) / 3;
          if (l > 70 && l < 185) clean = false;
          if (l >= 128) ts += 2 ** bit;
        }
        if (clean && Math.abs(ts - Date.now()) < 60_000) rec.cap = ts;
      }
      window.__frames.push(rec);
      if (window.__frames.length > 20000) window.__frames.shift();
      if (attached === v) v.requestVideoFrameCallback(cb);
    };
    v.requestVideoFrameCallback(cb);
  };
  setInterval(attach, 100);
  const seen = [];
  window.__screens = seen;
  setInterval(() => {
    const s = window.passstreamViewer?.screen;
    if (s && seen.at(-1)?.k !== s.kind) seen.push({ k: s.kind, at: Date.now() });
  }, 20);
}
/** In a viewer page: sample the decrypted audio every 20 ms for a 440 Hz tone (like e2ee.mjs listen()). */
async function startTone(p) {
  return p.evaluate(() => {
    if (window.__tone) return true;
    const ctx = new AudioContext();
    const an = ctx.createAnalyser();
    an.fftSize = 4096;
    an.smoothingTimeConstant = 0;
    const el = new Audio();
    el.muted = true;
    let bound = null, src = null;
    // Follow the current audio receiver: a replaced peer connection brings a new track.
    const bind = () => {
      const t = window.passstream?.pc?.getReceivers().find((r) => r.track?.kind === "audio")?.track;
      if (!t || t === bound) return;
      bound = t;
      const stream = new MediaStream([t]);
      el.srcObject = stream;
      void el.play().catch(() => {});
      src?.disconnect();
      src = ctx.createMediaStreamSource(stream);
      src.connect(an);
      window.__toneRebinds = (window.__toneRebinds ?? -1) + 1;
    };
    bind();
    if (!bound) return false;
    void ctx.resume();
    const bins = new Float32Array(an.frequencyBinCount);
    window.__tone = [];
    setInterval(() => {
      bind();
      an.getFloatFrequencyData(bins);
      const k = Math.round((440 * an.fftSize) / ctx.sampleRate);
      let peak = -Infinity;
      for (let i = k - 2; i <= k + 2; i++) peak = Math.max(peak, bins[i]);
      window.__tone.push({ at: Date.now(), tone: peak > -60 });
      if (window.__tone.length > 20000) window.__tone.shift();
    }, 20);
    return true;
  });
}
async function viewer(link, pin, size = [390, 844], { via = browser, low = false, proxy = null } = {}) {
  const c = await via.newContext(phone(...size));
  await c.addInitScript(frameRecorder);
  if (proxy) await c.addInitScript(delayedPath, proxy);
  if (low) {
    // Withhold TWCC (no capacity proof): the SFU sends this viewer the low rendition, as in e2ee.mjs.
    await c.addInitScript(() => {
      const create = RTCPeerConnection.prototype.createOffer;
      RTCPeerConnection.prototype.createOffer = async function (...args) {
        const offer = await create.apply(this, args);
        offer.sdp = offer.sdp.replace(/^a=extmap:[^\r\n]*transport-wide-cc[^\r\n]*\r?\n/gm, "").replace(/^a=rtcp-fb:[^\r\n]*transport-cc[^\r\n]*\r?\n/gm, "");
        return offer;
      };
    });
  }
  const p = await c.newPage();
  log(p, "viewer");
  tapSignaling(p, `viewer${size[0]}`);
  await p.goto(link);
  await p.getByTestId("pin-screen").waitFor({ timeout: 20000 });
  const input = p.getByTestId("pin-input");
  await input.tap();
  await p.keyboard.type(pin, { delay: 20 });
  await p.waitForFunction(() => window.__screens.at(-1)?.k === "live", null, { timeout: 20000, polling: 50 });
  return p;
}

let currentSender = null;
async function share(args = [], { tone = 0, source = null } = {}) {
  const before = new Set(readdirSync(launches));
  const pending = cliAsync(["share", "--no-wait", "--json", ...args]);
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
  tapSignaling(p, "sender");
  await p.goto(url.replace("/sender#", `/sender?test=1${tone ? `&tone=${tone}` : ""}${source ? `&source=${source}` : ""}#`));
  await p.getByTestId("test-source").click({ timeout: 15000 });
  const r = await pending;
  if (r.code !== 0) throw Error(`share failed: ${r.stdout} ${r.stderr}`);
  await p.waitForFunction(() => window.passstreamSender?.phase === "live", null, { timeout: 15000 });
  currentSender = p;
  // Protocol errors the host library raises (e.g. a refused annotation).
  await p.evaluate(() => {
    window.__perr = [];
    window.passstreamSender.host.on("protocolError", (e) => window.__perr.push(e.error.code));
  });
  return { page: p, link: r.json.link, pin: cli(["pin", "--reveal", "--json"]).json?.pin ?? null, streamId: cli(["status", "--json"]).json?.stream?.id };
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
    r = { pass: false, error: String(e?.stack ?? e).slice(0, 1200) };
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

const pct = (xs, p) => {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))] : null;
};

/** One curtain trial: lower it by `how`, measure, lift it locally, wait for live. */
async function curtainTrial(s, v, how) {
  await v.evaluate(() => (window.__frames.length = 0));
  await s.page.evaluate(async () => {
    window.passstreamSender.trace(true);
    await window.passstreamSender.dumpTrace(); // start empty
  });
  await sleep(1500); // steady real frames first
  const t0 = Date.now();
  if (how === "cli") {
    const r = cli(["curtain", "on", "--json"]);
    if (r.code !== 0) throw Error(`curtain on: ${r.stderr}`);
  } else if (how === "hotkey") {
    await s.page.keyboard.press("Escape");
  } else if (how === "remote") {
    const r = await remote(s.streamId, "pause");
    if (r.status !== 200) throw Error(`remote pause ${r.status} ${JSON.stringify(r.body)}`);
  }
  await v.waitForFunction(() => window.__screens.at(-1)?.k === "paused", null, { timeout: 10000, polling: 20 });
  await v.getByTestId("paused-cover").waitFor();
  await sleep(1500); // anything still in flight arrives
  const { commandAt, appliedAt, fenceAt, fenceWorkerAt, fenceOpenAt, rows } = await s.page.evaluate(async () => ({ ...window.passstreamSender.curtain, rows: await window.passstreamSender.dumpTrace() }));
  // Sender side, from the encrypt worker's trace (same monotonic base as fenceAt).
  const video = rows.filter((r) => r[1] === 0 && r[2] !== 1 && r[8] === true);
  // The fence holds from when the worker armed it (postMessage takes a moment after the command;
  // frames reaching the worker in between were captured before the command, or are black).
  const firstAfter = video.find((r) => r[0] >= fenceWorkerAt);
  const { frames, screens } = await v.evaluate(() => ({ frames: window.__frames, screens: window.__screens }));
  const pausedAt = screens.filter((x) => x.k === "paused" && x.at >= t0).at(0)?.at ?? null;
  const real = frames.filter((f) => f.real && f.cap);
  const lastReal = real.at(-1);
  const firstCurtain = frames.find((f) => !f.real && f.at >= t0);
  // The cover must be on top everywhere: no hit lands on the video.
  const covered = await v.evaluate(() => {
    let hits = 0;
    for (let i = 0; i < 5; i++) for (let j = 0; j < 5; j++) if (document.elementFromPoint(((i + 0.5) / 5) * innerWidth, ((j + 0.5) / 5) * innerHeight)?.closest("[data-testid=zoom-layer]")) hits++;
    return hits === 0 && getComputedStyle(document.querySelector("[data-testid=paused-cover]")).backgroundColor !== "rgba(0, 0, 0, 0)";
  });
  const trial = {
    how,
    command_path_ms: commandAt - t0, // test start → command reached the page (CLI → daemon fsync → page)
    applied_after_command_ms: appliedAt - commandAt, // capture track disabled
    last_real_capture_minus_command_ms: lastReal ? lastReal.cap - commandAt : null,
    real_frames_captured_after_command: real.filter((f) => f.cap > commandAt).length,
    real_frames_shown_after_command_plus_500ms: frames.filter((f) => f.real && f.at > commandAt + 500).length,
    last_real_shown_minus_command_ms: lastReal ? lastReal.at - commandAt : null,
    first_curtain_frame_minus_command_ms: firstCurtain ? firstCurtain.at - commandAt : null,
    viewer_paused_minus_command_ms: pausedAt ? pausedAt - commandAt : null,
    cover_opaque: covered,
    sender_fence_open_after_command_ms: Math.round(fenceOpenAt - fenceAt),
    sender_fence_armed_after_post_ms: Math.round((fenceWorkerAt - fenceAt) * 10) / 10,
    sender_frames_encrypted_in_fence: rows.filter((r) => r[1] === 0 && r[8] === true && r[0] >= fenceWorkerAt && r[0] < fenceOpenAt).length,
    sender_first_video_after_fence_ms: firstAfter ? Math.round(firstAfter[0] - fenceAt) : null,
    sender_first_video_after_fence_is_key: firstAfter ? firstAfter[7] : null,
    frames_recorded: frames.length,
  };
  // Lift it (local only), then wait for real frames again.
  const off = cli(["curtain", "off", "--json"]);
  if (off.code !== 0) throw Error(`curtain off: ${off.stderr}`);
  await v.waitForFunction(() => window.__screens.at(-1)?.k === "live", null, { timeout: 10000, polling: 50 });
  return trial;
}

await startServer();
await startDaemon();
const LAUNCH_ARGS = ["--no-sandbox", "--allow-loopback-in-peer-connection", "--disable-dev-shm-usage", "--autoplay-policy=no-user-gesture-required", "--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows"];
browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true, args: LAUNCH_ARGS });
senderCtx = await browser.newContext({ viewport: { width: 900, height: 1000 } });

try {
  await test("curtain_latency_and_no_real_frames", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, s.pin);
    const trials = [];
    for (let i = 0; i < TRIALS; i++) trials.push(await curtainTrial(s, v, "cli"));
    for (let i = 0; i < TRIALS; i++) trials.push(await curtainTrial(s, v, "hotkey"));
    trials.push(await curtainTrial(s, v, "remote"));
    // Lifting it from the server is refused, and nothing is applied.
    const lowered = await remote(s.streamId, "pause");
    await v.waitForFunction(() => window.__screens.at(-1)?.k === "paused", null, { timeout: 10000 });
    const lift = await remote(s.streamId, "resume");
    await sleep(1000);
    const stillPaused = await v.evaluate(() => window.__screens.at(-1)?.k === "paused");
    cli(["curtain", "off", "--json"]);
    const sum = (k, how) => {
      const xs = trials.filter((t) => !how || t.how === how).map((t) => t[k]).filter((x) => x !== null);
      return { p50: pct(xs, 50), p95: pct(xs, 95), max: Math.max(...xs) };
    };
    const frameMs = 1000 / 15;
    const ok = trials.every((t) => t.real_frames_captured_after_command === 0 && t.real_frames_shown_after_command_plus_500ms === 0 && t.cover_opaque && t.last_real_capture_minus_command_ms !== null && t.last_real_capture_minus_command_ms <= frameMs && t.sender_frames_encrypted_in_fence === 0 && t.sender_first_video_after_fence_is_key === true);
    return {
      pass: ok && lowered.status === 200 && lift.status === 403 && lift.body?.error?.code === "local_owner_required" && stillPaused,
      remote_resume: { status: lift.status, code: lift.body?.error?.code, still_paused: stillPaused },
      summary: {
        last_real_capture_minus_command_ms: sum("last_real_capture_minus_command_ms"),
        applied_after_command_ms: sum("applied_after_command_ms"),
        viewer_paused_minus_command_ms: sum("viewer_paused_minus_command_ms"),
        first_curtain_frame_minus_command_ms: sum("first_curtain_frame_minus_command_ms"),
        sender_fence_open_after_command_ms: sum("sender_fence_open_after_command_ms"),
        last_real_shown_minus_command_ms: sum("last_real_shown_minus_command_ms"),
        command_path_ms_cli: sum("command_path_ms", "cli"),
        command_path_ms_hotkey: sum("command_path_ms", "hotkey"),
        command_path_ms_remote: sum("command_path_ms", "remote"),
      },
      trials,
    };
  });

  await test("curtain_delayed_path_low_rendition_and_audio", async () => {
    // Viewer B reaches the SFU only through a UDP proxy adding DELAY ms each way (no TURN, no ICE-TCP).
    const DELAY = Number(process.env.CURTAIN_DELAY_MS ?? 150);
    const proxy = udpDelayProxy(18901, 18843, DELAY);
    const loadStart = readFileSync("/proc/loadavg", "utf8").trim();
    try {
      const s = await share(["--pin", "4"], { tone: 440 });
      const a = await viewer(s.link, s.pin); // loopback, native rendition
      const b = await viewer(s.link, s.pin, [390, 844], { proxy: [18843, 18901] }); // delayed path
      const c = await viewer(s.link, s.pin, [390, 844], { low: true }); // low rendition (no TWCC proof)
      const lowOk = await c.waitForFunction(() => document.querySelector("[data-testid=video]")?.videoHeight === 180 && window.passstreamViewer.decrypted.some((d) => d.track === 2), null, { timeout: 30000 }).then(() => true, () => false);
      const tones = [await startTone(a), await startTone(b), await startTone(c)];
      await sleep(2500);
      const g2g = async (p) => p.evaluate(() => {
        const ds = window.__frames.filter((f) => f.cap && Date.now() - f.at < 2000).map((f) => f.at - f.cap).sort((x, y) => x - y);
        return ds.length ? ds[Math.floor(ds.length / 2)] : null;
      });
      const g2gBefore = { a: await g2g(a), b: await g2g(b), c: await g2g(c) };
      await s.page.evaluate(() => window.passstreamSender.trace(true));
      const trials = [];
      for (let i = 0; i < 3; i++) {
        for (const p of [a, b, c]) await p.evaluate(() => ((window.__frames.length = 0), (window.__tone.length = 0)));
        await s.page.evaluate(() => (window.passstreamSender.drops.fence = 0));
        await sleep(1500);
        const r = cli(["curtain", "on", "--json"]);
        if (r.code !== 0) throw Error(`curtain on: ${r.stderr}`);
        for (const p of [a, b, c]) await p.waitForFunction(() => window.__screens.at(-1)?.k === "paused", null, { timeout: 10000, polling: 20 });
        await sleep(2000); // hold: anything in flight on the delayed path has arrived
        const liftAt = Date.now();
        const sender = await s.page.evaluate(async () => {
          const c = window.passstreamSender;
          const rows = await c.dumpTrace();
          return { commandAt: c.curtain.commandAt, fenceAt: c.curtain.fenceWorkerAt, fenceOpenAt: c.curtain.fenceOpenAt, fenceDrops: c.drops.fence ?? 0, rows };
        });
        const inFence = sender.rows.filter((row) => row[1] === 0 && row[8] === true && row[0] >= sender.fenceAt && row[0] < sender.fenceOpenAt);
        const firstVideoAfter = sender.rows.find((row) => row[1] === 0 && row[2] !== 1 && row[8] === true && row[0] >= sender.fenceAt);
        const view = async (p) => {
          const { frames, tone } = await p.evaluate(() => ({ frames: window.__frames, tone: window.__tone }));
          const cmd = sender.commandAt;
          const real = frames.filter((f) => f.real);
          const clocked = real.filter((f) => f.cap);
          const toneOn = tone.filter((x) => x.tone);
          return {
            frames: frames.length,
            clocked_real_frames: clocked.length,
            real_captured_after_command: clocked.filter((f) => f.cap > cmd).length,
            last_real_capture_minus_command_ms: clocked.length ? clocked.at(-1).cap - cmd : null,
            last_real_shown_minus_command_ms: real.length ? real.at(-1).at - cmd : null,
            first_curtain_frame_minus_command_ms: (frames.find((f) => !f.real && f.at >= cmd)?.at ?? NaN) - cmd,
            tone_samples: tone.length,
            last_tone_minus_command_ms: toneOn.filter((x) => x.at <= liftAt).length ? toneOn.filter((x) => x.at <= liftAt).at(-1).at - cmd : null,
            tone_during_hold: tone.filter((x) => x.tone && x.at > cmd + 800 && x.at < liftAt).length,
          };
        };
        const trial = { a: await view(a), b: await view(b), c: await view(c), sender: { fence_open_after_command_ms: Math.round(sender.fenceOpenAt - sender.fenceAt), video_frames_encrypted_in_fence: inFence.filter((row) => row[2] !== 1).length, audio_frames_encrypted_in_fence: inFence.filter((row) => row[2] === 1).length, first_video_after_fence_is_key: firstVideoAfter?.[7] ?? null, fence_drops: sender.fenceDrops } };
        cli(["curtain", "off", "--json"]);
        for (const p of [a, b, c]) await p.waitForFunction(() => window.__screens.at(-1)?.k === "live", null, { timeout: 15000 });
        await sleep(1200);
        trial.tone_back = { a: await a.evaluate(() => window.__tone.slice(-30).some((x) => x.tone)), b: await b.evaluate(() => window.__tone.slice(-30).some((x) => x.tone)), c: await c.evaluate(() => window.__tone.slice(-30).some((x) => x.tone)) };
        trial.c_low = await c.evaluate(() => document.querySelector("[data-testid=video]")?.videoHeight);
        trials.push(trial);
      }
      // Baseline: the lower of the two loopback viewers (one may be mid-replacement while sampled).
      const base = Math.min(...[g2gBefore.a, g2gBefore.c].filter((x) => x !== null));
      const delayed = Number.isFinite(base) && g2gBefore.b !== null && g2gBefore.b - base >= DELAY / 2;
      const rebinds = { a: await a.evaluate(() => window.__toneRebinds ?? 0), b: await b.evaluate(() => window.__toneRebinds ?? 0), c: await c.evaluate(() => window.__toneRebinds ?? 0) };
      const ok = trials.every((t) =>
        t.sender.video_frames_encrypted_in_fence === 0 && t.sender.audio_frames_encrypted_in_fence === 0 && t.sender.first_video_after_fence_is_key === true &&
        [t.a, t.b, t.c].every((v) => v.real_captured_after_command === 0 && v.tone_during_hold === 0 && v.tone_samples > 0) &&
        t.a.clocked_real_frames > 0 && t.b.clocked_real_frames > 0 && t.tone_back.a && t.tone_back.b && t.tone_back.c);
      // C's rendition is reported, not required here: whether the SFU drops a TWCC-less viewer to
      // the low rendition in time varies; curtain_exact_720p_capture_native_and_low requires it.
      return { pass: ok && delayed && tones.every(Boolean), delay_ms_each_way: DELAY, load_start: loadStart, load_end: readFileSync("/proc/loadavg", "utf8").trim(), low_rendition_on_c: lowOk, g2g_median_before_ms: g2gBefore, impairment_effective: delayed, tone_probe_rebinds: rebinds, replacements: { a: await a.evaluate(() => window.passstream.replacements), b: await b.evaluate(() => window.passstream.replacements), c: await c.evaluate(() => window.passstream.replacements) }, trials };
    } finally {
      proxy.close();
    }
  });

  await test("curtain_exact_720p_capture_native_and_low", async () => {
    // Review D2 r2 #3: a capture exactly the size of the old 1280×720 curtain picture
    // resumed with a delta (193–580 ms to paused). The picture now never matches it.
    const s = await share(["--pin", "4"], { source: "720p" });
    const a = await viewer(s.link, s.pin); // native
    const isLow = (p) => p.waitForFunction(() => document.querySelector("[data-testid=video]")?.videoHeight === 180 && window.passstreamViewer.decrypted.some((d) => d.track === 2), null, { timeout: 30000 }).then(() => true, () => false);
    // Whether the SFU moves a TWCC-less viewer to the low rendition in time varies: one fresh try.
    let c = await viewer(s.link, s.pin, [390, 844], { low: true });
    let lowOk = await isLow(c);
    let lowAttempts = 1;
    if (!lowOk) {
      await c.context().close();
      c = await viewer(s.link, s.pin, [390, 844], { low: true });
      lowOk = await isLow(c);
      lowAttempts = 2;
    }
    const capture = await s.page.evaluate(() => window.passstreamSender.capture.getVideoTracks()[0].getSettings());
    await s.page.evaluate(() => window.passstreamSender.trace(true));
    const trials = [];
    for (let i = 0; i < 5; i++) {
      for (const p of [a, c]) await p.evaluate(() => (window.__frames.length = 0));
      await s.page.evaluate(async () => void (await window.passstreamSender.dumpTrace()));
      await sleep(1500);
      const r = cli(["curtain", "on", "--json"]);
      if (r.code !== 0) throw Error(`curtain on: ${r.stderr}`);
      for (const p of [a, c]) await p.waitForFunction(() => window.__screens.at(-1)?.k === "paused", null, { timeout: 10000, polling: 20 });
      await sleep(800);
      const sender = await s.page.evaluate(async () => {
        const core = window.passstreamSender;
        return { ...core.curtain, rows: await core.dumpTrace() };
      });
      const first = sender.rows.find((row) => row[1] === 0 && row[2] === 0 && row[8] === true && row[0] >= sender.fenceWorkerAt);
      const firstLow = sender.rows.find((row) => row[1] === 0 && row[2] === 2 && row[8] === true && row[0] >= sender.fenceWorkerAt);
      const view = async (p) => {
        const { frames, screens } = await p.evaluate(() => ({ frames: window.__frames, screens: window.__screens }));
        const cmd = sender.commandAt;
        const curtain = frames.find((f) => !f.real && f.at >= cmd);
        return {
          paused_minus_command_ms: (screens.filter((x) => x.k === "paused").at(-1)?.at ?? NaN) - cmd,
          real_captured_after_command: frames.filter((f) => f.real && f.cap && f.cap > cmd).length,
          real_frames: frames.filter((f) => f.real).length,
          curtain_size: curtain ? `${curtain.w}x${curtain.h}` : null,
          real_size: frames.find((f) => f.real) ? `${frames.find((f) => f.real).w}x${frames.find((f) => f.real).h}` : null,
        };
      };
      trials.push({ native: await view(a), low: await view(c), sender: { first_native_after_fence_is_key: first?.[7] ?? null, first_low_after_fence_is_key: firstLow?.[7] ?? null, fence_open_after_command_ms: Math.round(sender.fenceOpenAt - sender.fenceAt) } });
      cli(["curtain", "off", "--json"]);
      for (const p of [a, c]) await p.waitForFunction(() => window.__screens.at(-1)?.k === "live", null, { timeout: 15000 });
    }
    const ok = trials.every((t) =>
      t.sender.first_native_after_fence_is_key === true && t.sender.first_low_after_fence_is_key === true &&
      [t.native, t.low].every((v) => v.real_captured_after_command === 0 && v.paused_minus_command_ms < 500 && v.curtain_size !== v.real_size));
    return { pass: ok && lowOk && capture.width === 1280 && capture.height === 720, capture: `${capture.width}x${capture.height}`, low_rendition: lowOk, low_attempts: lowAttempts, trials };
  });

  await test("annotations_round_trip_between_viewers_and_host", async () => {
    const s = await share(["--pin", "4"]);
    const a = await viewer(s.link, s.pin, [390, 844]);
    const b = await viewer(s.link, s.pin, [320, 568]);
    const codeA = await a.evaluate(() => window.passstreamViewer.rowCode);
    // Host-only by default: no draw control until the host allows it.
    const drawBefore = await a.getByTestId("draw-toggle").count();
    const remoteOn = await remote(s.streamId, "annotations", { viewers: true });
    await sleep(500);
    const drawAfterRemote = await a.getByTestId("draw-toggle").count();
    const on = cli(["annotations", "on", "--json"]);
    await a.getByTestId("draw-toggle").waitFor({ timeout: 5000 });
    await b.getByTestId("draw-toggle").waitFor({ timeout: 5000 });
    // Viewer A draws a pen stroke with the real UI (draw mode, one pointer).
    await a.getByTestId("draw-toggle").click();
    await a.getByTestId("tool-pen").click();
    const box = await a.getByTestId("stage").boundingBox();
    const y0 = box.y + box.height / 2;
    await a.mouse.move(box.x + 60, y0 - 30);
    await a.mouse.down();
    for (let i = 1; i <= 20; i++) await a.mouse.move(box.x + 60 + i * 12, y0 - 30 + Math.sin(i / 3) * 25, { steps: 2 });
    await a.mouse.up();
    // Then the laser.
    await a.getByTestId("tool-laser").click();
    await a.mouse.move(box.x + 100, y0 + 40);
    await a.mouse.down();
    for (let i = 1; i <= 10; i++) await a.mouse.move(box.x + 100 + i * 15, y0 + 40, { steps: 2 });
    await b.waitForFunction((code) => (window.passstreamViewer.annotations.store.origins.get(code)?.trail.length ?? 0) > 0, codeA, { timeout: 5000, polling: 10 });
    await a.mouse.up();
    await b.waitForFunction((code) => [...(window.passstreamViewer.annotations.store.origins.get(code)?.strokes.values() ?? [])].some((s) => s.done && s.points.length > 10), codeA, { timeout: 5000 });
    const strokeB = await b.evaluate((code) => [...window.passstreamViewer.annotations.store.origins.get(code).strokes.values()].map((s) => s.points.length), codeA);
    const strokeA = await a.evaluate((code) => [...window.passstreamViewer.annotations.store.origins.get(code).strokes.values()].map((s) => s.points.length), codeA);
    const strokeHost = await s.page.evaluate((code) => [...(window.passstreamSender.annotations.store.origins.get(code)?.strokes.values() ?? [])].map((x) => x.points.length), codeA);
    // The host points from its preview: viewers see it as "Host".
    const pv = await s.page.getByTestId("sender-preview").boundingBox();
    await s.page.mouse.move(pv.x + pv.width * 0.7, pv.y + pv.height * 0.3);
    await s.page.mouse.down();
    for (let i = 1; i <= 8; i++) await s.page.mouse.move(pv.x + pv.width * 0.7 - i * 6, pv.y + pv.height * 0.3 + i * 4, { steps: 2 });
    await Promise.all([a, b].map((p) => p.waitForFunction(() => (window.passstreamViewer.annotations.store.origins.get("host")?.trail.length ?? 0) > 0, null, { timeout: 5000, polling: 10 })));
    // Pixels: A's row colour is on B's overlay canvas.
    const colourOnB = await b.evaluate(async (code) => {
      const alphabet = "ACDEFHJKMNPRTUVWXY3479";
      const idx = 484 * alphabet.indexOf(code[0]) + 22 * alphabet.indexOf(code[1]) + alphabet.indexOf(code[2]);
      const hex = ["#e5484d", "#f76b15", "#ffc53d", "#30a46c", "#12a594", "#0090ff", "#8e4ec6", "#d6409f"][idx % 8];
      const want = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
      const c = document.querySelector("[data-testid=ink-overlay]");
      const d = c.getContext("2d").getImageData(0, 0, c.width, c.height).data;
      let n = 0;
      for (let i = 0; i < d.length; i += 4) if (d[i + 3] > 200 && Math.abs(d[i] - want[0]) < 12 && Math.abs(d[i + 1] - want[1]) < 12 && Math.abs(d[i + 2] - want[2]) < 12) n++;
      return n;
    }, codeA);
    await s.page.mouse.up();
    await sleep(150);
    // Screenshots: annotations on both phones, A's draw toolbar, and the sender window.
    await a.screenshot({ path: join(shots, "390-annotations.png"), animations: "disabled", scale: "css" });
    await b.screenshot({ path: join(shots, "320-annotations.png"), animations: "disabled", scale: "css" });
    await s.page.screenshot({ path: join(shots, "sender-preview.png"), animations: "disabled", scale: "css", fullPage: true });
    // Host clear reaches everyone; host turning drawing off removes the control.
    cli(["annotations", "clear", "--json"]);
    const cleared = await b.waitForFunction(() => window.passstreamViewer.annotations.store.origins.size === 0, null, { timeout: 5000 }).then(() => true, () => false);
    const remoteOff = await remote(s.streamId, "annotations", { viewers: false });
    const offReached = await a.waitForFunction(() => !window.passstreamViewer.annotations.policy, null, { timeout: 5000 }).then(() => true, () => false);
    const counters = await s.page.evaluate(() => window.passstreamSender.annotations.counters);
    const diag = {
      host_viewers_enabled: await s.page.evaluate(() => window.passstreamSender.annotations.viewersEnabled),
      host_pending: await s.page.evaluate(() => window.passstreamSender.annotations.pending),
      a_policy: await a.evaluate(() => window.passstreamViewer.annotations.policy),
      a_counters: await a.evaluate(() => window.passstreamViewer.annotations.counters),
      a_rejects: await a.evaluate(() => window.passstreamViewer.annotationRejects),
      remote_off_body: remoteOff.body,
    };
    return {
      pass:
        drawBefore === 0 && remoteOn.status === 403 && remoteOn.body?.error?.code === "local_owner_required" && drawAfterRemote === 0 && on.code === 0 &&
        strokeB.length === 1 && strokeB[0] === strokeA[0] && strokeHost[0] === strokeA[0] && colourOnB > 50 && remoteOff.status === 200 && counters.dropped_policy === 0 && cleared && offReached,
      cleared_on_b: cleared,
      off_reached_a: offReached,
      diag,
      row_a: codeA,
      stroke_points: { a: strokeA, b: strokeB, host: strokeHost },
      colour_pixels_on_b: colourOnB,
      remote: { on: [remoteOn.status, remoteOn.body?.error?.code], off: remoteOff.status },
      host_counters: counters,
    };
  });

  await test("annotation_clear_ordering_preserves_immediate_local_input", async () => {
    const s = await share(["--pin", "4"]);
    const a = await viewer(s.link, s.pin, [390, 844]);
    const b = await viewer(s.link, s.pin, [320, 568]);
    cli(["annotations", "on", "--json"]);
    await a.getByTestId("draw-toggle").waitFor({ timeout: 5000 });
    await a.getByTestId("draw-toggle").click();
    await a.getByTestId("tool-pen").click();
    const code = await a.evaluate(() => window.passstreamViewer.rowCode);
    const box = await a.getByTestId("stage").boundingBox();
    const draw = async (offset) => {
      await a.mouse.move(box.x + 50, box.y + box.height / 2 + offset);
      await a.mouse.down();
      await a.mouse.move(box.x + 170, box.y + box.height / 2 + offset + 15, { steps: 5 });
      await a.mouse.up();
    };
    await draw(-50);
    await b.waitForFunction(c => (window.passstreamViewer.annotations.store.origins.get(c)?.strokes.size ?? 0) === 1, code, { timeout: 5000 });
    // Hold only fan-out, not upstream acceptance, to force the review's exact
    // lossless ordering: host clear -> host accepts new ink -> confirmation.
    await s.page.waitForFunction(() => !window.passstreamSender.annotations.flushing);
    await s.page.evaluate(() => {
      const an = window.passstreamSender.annotations;
      window.__d2Flush = an.flush;
      an.flush = async () => {};
    });
    let fresh;
    try {
      await a.getByTestId("tool-clear").click();
      await s.page.waitForFunction(c => !window.passstreamSender.annotations.store.origins.has(c), code, { timeout: 5000 });
      await draw(35);
      fresh = await a.evaluate(c => [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
      await s.page.waitForFunction(({ c, id }) => window.passstreamSender.annotations.store.origins.get(c)?.strokes.get(id)?.done, { c: code, id: fresh[0] }, { timeout: 5000 });
    } finally {
      await s.page.evaluate(() => {
        const an = window.passstreamSender.annotations;
        an.flush = window.__d2Flush;
        delete window.__d2Flush;
        void an.flush();
      });
    }
    await b.waitForFunction(({ c, id }) => {
      const ids = [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])];
      return ids.length === 1 && ids[0] === id;
    }, { c: code, id: fresh[0] }, { timeout: 5000 });
    await sleep(1500); // include a subsequent repair tick
    const drawer = await a.evaluate(c => [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
    const observer = await b.evaluate(c => [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
    const host = await s.page.evaluate(c => [...(window.passstreamSender.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
    return { pass: fresh.length === 1 && [drawer, observer, host].every(ids => ids.length === 1 && ids[0] === fresh[0]), fresh, drawer, observer, host };
  });

  for (const prior of ["global", "origin"]) for (const lifecycle of ["sender_reload", "same_host_reconnect"]) {
    await test(`annotation_${lifecycle}_after_${prior}_clear`, async () => {
      const s = await share(["--pin", "4"]);
      const a = await viewer(s.link, s.pin, [390, 844]);
      const b = await viewer(s.link, s.pin, [320, 568]);
      cli(["annotations", "on", "--json"]);
      const code = await a.evaluate(() => window.passstreamViewer.rowCode);
      const draw = async offset => {
        await a.getByTestId("draw-toggle").waitFor({ timeout: 10000 });
        if (!(await a.getByTestId("tool-pen").count())) await a.getByTestId("draw-toggle").click();
        await a.getByTestId("tool-pen").click();
        const box = await a.getByTestId("stage").boundingBox();
        await a.mouse.move(box.x + 50, box.y + box.height / 2 + offset);
        await a.mouse.down();
        await a.mouse.move(box.x + 170, box.y + box.height / 2 + offset + 15, { steps: 5 });
        await a.mouse.up();
      };
      await draw(-40);
      await b.waitForFunction(c => (window.passstreamViewer.annotations.store.origins.get(c)?.strokes.size ?? 0) === 1, code);
      if (prior === "global") cli(["annotations", "clear", "--json"]);
      else await s.page.evaluate(c => window.passstreamSender.annotations.revoke({ admissionIds: [], rowCode: c }), code);
      await Promise.all([a, b].map(p => p.waitForFunction(c => !window.passstreamViewer.annotations.store.origins.has(c), code)));
      const observerAdmissions = await b.evaluate(() => window.passstreamViewer.admissions);
      const before = await a.evaluate(() => ({ admissions: window.passstreamViewer.admissions, namespace: String(window.passstreamViewer.annotations.namespace) }));
      if (lifecycle === "sender_reload") {
        await s.page.reload();
        await s.page.getByTestId("test-source").click({ timeout: 15000 });
        await s.page.waitForFunction(() => window.passstreamSender?.phase === "live", null, { timeout: 15000 });
        // Reload remains restrictive until the owner explicitly re-enables drawing.
        await a.waitForFunction(n => window.passstreamViewer.admissions > n && !window.passstreamViewer.annotations.policy, before.admissions, { timeout: 20000 });
        cli(["annotations", "on", "--json"]);
      } else {
        await a.evaluate(() => window.passstream.replace()); // Existing PC replacement path, without reloading the viewer.
      }
      await a.waitForFunction(n => window.passstreamViewer.admissions > n && window.passstreamViewer.annotations.policy, before.admissions, { timeout: 20000 });
      const observerBeforeWait = await b.evaluate(() => ({ admissions: window.passstreamViewer.admissions, namespace: String(window.passstreamViewer.annotations.namespace), policy: window.passstreamViewer.annotations.policy }));
      // Live-only ink can be asserted at the observer only after its own new admission.
      if (lifecycle === "sender_reload") await b.waitForFunction(n => window.passstreamViewer.admissions > n && window.passstreamViewer.annotations.policy, observerAdmissions, { timeout: 20000 });
      const staleBeforeFresh = await s.page.evaluate(() => window.passstreamSender.annotations.counters.dropped_stale);
      await draw(40);
      const fresh = await a.evaluate(c => [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
      const delivered = await Promise.all([b, s.page].map((p, i) => p.waitForFunction(({ c, id, sender }) => {
        const an = sender ? window.passstreamSender.annotations : window.passstreamViewer.annotations;
        return an.store.origins.get(c)?.strokes.get(id)?.done;
      }, { c: code, id: fresh[0], sender: i === 1 }, { timeout: 10000 }).then(() => true, () => false)));
      await sleep(1500);
      const after = await a.evaluate(c => ({ admissions: window.passstreamViewer.admissions, namespace: String(window.passstreamViewer.annotations.namespace), ids: [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], rejects: window.passstreamViewer.annotationRejects }), code);
      const host = await s.page.evaluate(c => ({ ids: [...(window.passstreamSender.annotations.store.origins.get(c)?.strokes.keys() ?? [])], stale: window.passstreamSender.annotations.counters.dropped_stale }), code);
      const observer = await b.evaluate(c => [...(window.passstreamViewer.annotations.store.origins.get(c)?.strokes.keys() ?? [])], code);
      return { pass: delivered.every(Boolean) && fresh.length === 1 && [after.ids, host.ids, observer].every(ids => ids.length === 1 && ids[0] === fresh[0]) && (before.namespace !== after.namespace) === (lifecycle === "sender_reload") && host.stale === staleBeforeFresh, before, after, host, observer, delivered, observer_before_wait: observerBeforeWait, stale_before_fresh: staleBeforeFresh };
    });
  }

  await test("kicked_viewer_annotations_rejected", async () => {
    const s = await share(["--pin", "4"]);
    const a = await viewer(s.link, s.pin);
    const b = await viewer(s.link, s.pin);
    cli(["annotations", "on", "--json"]);
    await a.waitForFunction(() => window.passstreamViewer.annotations.policy, null, { timeout: 5000 });
    const codeA = await a.evaluate(() => window.passstreamViewer.rowCode);
    // Positive control: A's laser reaches B.
    await a.evaluate(() => {
      const an = window.passstreamViewer.annotations;
      an.input.setTool("laser");
      an.input.begin({ x: 0.3, y: 0.3 });
      an.input.move({ x: 0.35, y: 0.32 });
      an.input.end();
    });
    await b.waitForFunction((code) => window.passstreamViewer.annotations.store.origins.has(code), codeA, { timeout: 5000 });
    // A kicked viewer's best try: messages sealed with keys it held before the kick.
    const sealed = await a.evaluate(async () => {
      const link = window.passstreamViewer.annotations.link;
      const out = [];
      for (let i = 0; i < 3; i++) {
        const body = new Uint8Array([1, 1, 0x40, 0x00, 0x40, 0x00]); // laser at (0.25, 0.25)
        const bytes = await link.seal(body);
        out.push(btoa(String.fromCharCode(...bytes)).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, ""));
      }
      return out;
    });
    const epochBefore = await b.evaluate(() => window.passstreamViewer.epoch);
    const acceptedBefore = await s.page.evaluate(() => window.passstreamSender.annotations.counters.accepted);
    const receivedBefore = await b.evaluate(() => window.passstreamViewer.annotations.counters.received);
    const row = cli(["viewers", "--json"]).json.viewers.find((r) => r.row_code === codeA);
    cli(["kick", row.admission_ids[0], "--json"]);
    await a.waitForFunction(() => window.__screens.at(-1)?.k === "removed", null, { timeout: 10000 });
    await b.waitForFunction((e) => window.passstreamViewer.epoch > e, epochBefore, { timeout: 5000 });
    // Inject them on a fresh signaling connection (the old one was closed by the kick).
    const cap = new URL(s.link).hash.match(/c=([^&]+)/)[1];
    const injected = await new Promise((res) => {
      const ws = new WebSocket(`ws://127.0.0.1:${httpPort}/signal`);
      let joined = false;
      ws.onmessage = (ev) => {
        const m = JSON.parse(ev.data);
        if (m.type === "joined" && !joined) {
          joined = true;
          for (const b64 of sealed) ws.send(JSON.stringify({ type: "e2ee", b: b64 }));
          setTimeout(() => (ws.close(), res({ joined: true })), 1500);
        }
      };
      ws.onopen = () => ws.send(JSON.stringify({ type: "join", role: "view", credential: cap, protocol: 1 }));
      ws.onerror = () => res({ joined: false });
      setTimeout(() => res({ joined }), 5000);
    });
    await sleep(500);
    const acceptedAfter = await s.page.evaluate(() => window.passstreamSender.annotations.counters.accepted);
    const receivedAfter = await b.evaluate(() => window.passstreamViewer.annotations.counters.received);
    // The kick clears A's ink everywhere (a host-stamped clear of A's origin), and nothing of A's comes back.
    const aInkOnB = await b.evaluate((code) => window.passstreamViewer.annotations.store.origins.has(code), codeA);
    const aInkOnHost = await s.page.evaluate((code) => window.passstreamSender.annotations.store.origins.has(code), codeA);
    const perr = await s.page.evaluate(() => window.__perr);
    // B still draws for others after the kick, on the new epoch.
    return {
      pass: injected.joined && acceptedAfter === acceptedBefore && !aInkOnB && !aInkOnHost && perr.filter((c) => c === "unexpected_message").length >= 3,
      a_ink_after_kick: { on_b: aInkOnB, on_host: aInkOnHost },
      injected,
      epoch: { before: epochBefore, after: await b.evaluate(() => window.passstreamViewer.epoch) },
      host_accepted: { before: acceptedBefore, after: acceptedAfter },
      b_received: { before: receivedBefore, after: receivedAfter },
      host_protocol_errors: perr,
    };
  });

  await test("relay_sees_only_ciphertext_needle", async () => {
    wsFrames.length = 0;
    const s = await share(["--pin", "4"]);
    const a = await viewer(s.link, s.pin);
    const b = await viewer(s.link, s.pin);
    cli(["annotations", "on", "--json"]);
    await a.waitForFunction(() => window.passstreamViewer.annotations.policy, null, { timeout: 5000 });
    const codeA = await a.evaluate(() => window.passstreamViewer.rowCode);
    // A stroke whose u16 coordinates spell a 16-byte needle.
    const NEEDLE = "PSNEEDLE-ANNOT-1";
    const words = [];
    for (let i = 0; i < NEEDLE.length; i += 2) words.push(NEEDLE.charCodeAt(i) * 256 + NEEDLE.charCodeAt(i + 1));
    await a.evaluate((w) => {
      const an = window.passstreamViewer.annotations;
      an.input.setTool("pen");
      const pts = [];
      for (let i = 0; i < w.length; i += 2) pts.push({ x: w[i] / 65535, y: w[i + 1] / 65535 });
      an.input.begin(pts[0]);
      for (const p of pts.slice(1)) an.input.move(p);
      an.input.end();
    }, words);
    await b.waitForFunction((code) => [...(window.passstreamViewer.annotations.store.origins.get(code)?.strokes.values() ?? [])].some((s) => s.done), codeA, { timeout: 5000 });
    const got = await b.evaluate((code) => [...window.passstreamViewer.annotations.store.origins.get(code).strokes.values()][0].points.flatMap((p) => [Math.round(p.x * 65535), Math.round(p.y * 65535)]), codeA);
    const decoded = got.map((w) => String.fromCharCode(w >> 8, w & 255)).join("");
    await sleep(500);
    // Every encoding of the needle a relay could see: raw, and base64/base64url at all three alignments.
    const raw = Buffer.from(NEEDLE, "latin1");
    const forms = new Set([NEEDLE]);
    for (let off = 0; off < 3; off++) {
      const padded = Buffer.concat([Buffer.alloc(off), raw]);
      for (const enc of ["base64", "base64url"]) {
        const s64 = padded.toString(enc).replace(/=+$/, "");
        const skip = Math.ceil((off * 4) / 3);
        forms.add(s64.slice(skip + 1, skip + 1 + 16)); // a 16-char core that doesn't depend on neighbours
      }
    }
    const hits = (text) => [...forms].filter((f) => f.length >= 12 && text.includes(f));
    // Positive control: the scanner finds the needle in a frame that carries the plaintext body.
    const body = Buffer.concat([Buffer.from([2, 0, 1, 1, 4]), raw]);
    const control = hits(JSON.stringify({ type: "e2ee", b: body.toString("base64url") })).length + hits(JSON.stringify({ type: "e2ee", b: Buffer.concat([Buffer.from([7]), body]).toString("base64url") })).length;
    const frameHits = wsFrames.flatMap((f) => hits(f.payload).map((h) => `${f.who}/${f.dir}:${h}`));
    const annotationFrames = wsFrames.filter((f) => f.payload.includes('"e2ee"')).length;
    const logs = ["server.log", "daemon.log", "browser.log"].map((f) => (existsSync(join(out, f)) ? readFileSync(join(out, f), "latin1") : "")).join("\n");
    const logHits = hits(logs);
    return {
      pass: decoded === NEEDLE && control >= 2 && frameHits.length === 0 && logHits.length === 0 && wsFrames.length > 0,
      decoded_on_b: decoded,
      scanner_control_hits: control,
      frames_scanned: wsFrames.length,
      e2ee_frames: annotationFrames,
      frame_hits: frameHits,
      log_hits: logHits,
      forms: [...forms],
    };
  });

  await test("hud_screenshots_320_390", async () => {
    const s = await share(["--pin", "4"]);
    const v390 = await viewer(s.link, s.pin, [390, 844]);
    const v320 = await viewer(s.link, s.pin, [320, 568]);
    cli(["annotations", "on", "--json"]);
    // Some ink from the 320 viewer and the host, so the HUD shots show the overlay too.
    await v320.waitForFunction(() => window.passstreamViewer.annotations.policy, null, { timeout: 5000 });
    await v320.evaluate(() => {
      const an = window.passstreamViewer.annotations;
      an.input.setTool("pen");
      an.input.begin({ x: 0.2, y: 0.6 });
      for (let i = 1; i < 30; i++) an.input.move({ x: 0.2 + i * 0.02, y: 0.6 + Math.sin(i / 4) * 0.08 });
      an.input.end();
    });
    await sleep(2500); // a few HUD samples
    const out = {};
    // The controls auto-hide 4 s after the last interaction on a live picture, and can hide
    // between a check and a click: reveal (tap the stage) and click with short retries.
    const reveal = async (p) => {
      if (await p.evaluate(() => !!document.querySelector("[aria-label='Connection details']")?.closest(".opacity-0"))) {
        await p.getByTestId("stage").tap({ position: { x: 20, y: 120 } });
        await sleep(400);
      }
    };
    const press = async (p, locator) => {
      for (let i = 0; i < 6; i++) {
        await reveal(p);
        if (await locator.click({ timeout: 1500 }).then(() => true, () => false)) return;
      }
      throw Error("control stayed hidden");
    };
    for (const [w, p] of [[390, v390], [320, v320]]) {
      await press(p, p.getByRole("button", { name: "Connection details", exact: true }));
      await p.getByTestId("hud").waitFor();
      await p.getByTestId("hud-more").click();
      await sleep(1100);
      const rows = await p.evaluate(() => Object.fromEntries([...document.querySelectorAll("[data-hud]")].map((e) => [e.dataset.hud, e.textContent])));
      await p.screenshot({ path: join(shots, `${w}-hud.png`), animations: "disabled", scale: "css" });
      // Close it; draw mode toolbar shot.
      await p.getByTestId("hud-close").click();
      await press(p, p.getByTestId("draw-toggle"));
      await p.getByTestId("draw-tools").waitFor();
      await p.screenshot({ path: join(shots, `${w}-draw-tools.png`), animations: "disabled", scale: "css" });
      const toolsFit = await p.evaluate(() => {
        const r = document.querySelector("[data-testid=draw-tools]").getBoundingClientRect();
        const small = [...document.querySelectorAll("[data-testid=draw-tools] button")].filter((b) => Math.min(b.getBoundingClientRect().width, b.getBoundingClientRect().height) < 43.5).length;
        return r.left >= 0 && r.right <= innerWidth && small === 0;
      });
      out[w] = { rows, toolsFit };
    }
    await s.page.locator("[data-testid=sender-hud] summary").click();
    await sleep(1200);
    const senderRows = await s.page.evaluate(() => Object.fromEntries([...document.querySelectorAll("[data-testid=sender-hud] [data-hud]")].map((e) => [e.dataset.hud, e.textContent])));
    await s.page.screenshot({ path: join(shots, "sender-hud.png"), animations: "disabled", scale: "css", fullPage: true });
    cli(["curtain", "on", "--json"]);
    await s.page.getByTestId("curtain-banner").waitFor();
    await sleep(300);
    await s.page.screenshot({ path: join(shots, "sender-curtain.png"), animations: "disabled", scale: "css", fullPage: true });
    await v390.waitForFunction(() => window.__screens.at(-1)?.k === "paused", null, { timeout: 5000 });
    await v390.screenshot({ path: join(shots, "390-curtain.png"), animations: "disabled", scale: "css" });
    cli(["curtain", "off", "--json"]);
    const filled = (r) => ["RTT", "Bitrate", "Frame rate", "Path", "Rendition", "Layer", "E2EE epoch"].every((k) => r[k] && r[k] !== "—");
    return { pass: filled(out[390].rows) && filled(out[320].rows) && out[390].toolsFit && out[320].toolsFit && senderRows.Uplink !== "—" && senderRows.Resolution === "960×540", viewer: out, sender: senderRows };
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
