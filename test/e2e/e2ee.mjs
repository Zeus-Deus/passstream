// End-to-end tests with PIN and E2EE on: the real server, the real local
// daemon and CLI, the real sender page (launched by the daemon through a
// browser wrapper so Playwright drives it) and real viewers in headless
// system Chromium. Writes test/torture/results/<run>.json and prints a table.
//
//   Build instrumentation explicitly with npm --prefix web run build -- --mode e2e,
//   then embed it in the test binary. Normal production builds have no sender hooks.
//   mise exec -- node test/e2e/e2ee.mjs            (needs the instrumented binary)
//   E2E_FILTER=kick E2E_RUN=name ...               (subset, run name)
import { artifactNeedles } from "./artifact-needles.mjs";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import { appendFileSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const root = process.cwd();
const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
// The real SFrame code (Node runs the TypeScript sources), for parsing what the SFU forwards.
const { SFrameKey, decodeHeader } = await import(pathToFileURL(resolve("e2ee/src/sframe.ts")).href);
const { deriveBaseKey, splitKid } = await import(pathToFileURL(resolve("e2ee/src/keyring.ts")).href);
const TONE_HZ = 440;
const bin = resolve(process.env.E2E_SERVER_BIN ?? "build/passstream");
const run = process.env.E2E_RUN ?? `e2ee-${new Date().toISOString().replaceAll(":", "-")}`;
const filter = process.env.E2E_FILTER ? new RegExp(process.env.E2E_FILTER) : null;
const out = resolve("test/torture/out", run);
mkdirSync(out, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const httpPort = 18280, daemonPort = 47923;
const origin = `http://127.0.0.1:${httpPort}`;

// ---------------------------------------------------------------- environment
const base = mkdtempSync(join(tmpdir(), "ps-e2e-"));
const env = {
  ...process.env,
  XDG_RUNTIME_DIR: join(base, "run"),
  XDG_CONFIG_HOME: join(base, "config"),
  XDG_STATE_HOME: join(base, "state"),
};
for (const d of ["run", "config/passstream", "state", "launches", "data"]) mkdirSync(join(base, d), { recursive: true, mode: 0o700 });
chmodSync(join(base, "run"), 0o700);
const launches = join(base, "launches");
const wrapper = join(base, "browser.sh");
// The daemon supervises this "browser": it records its arguments for the
// harness and stays alive until the daemon kills its process group.
writeFileSync(wrapper, `#!/bin/sh\nprintf '%s\\n' "$@" > "${launches}/$$.args.tmp" && mv "${launches}/$$.args.tmp" "${launches}/$$.args"\nexec sleep 100000\n`, { mode: 0o755 });
// Publishing tests are physically pointed at a local fake, never real services.
const uploadRequests = [];
const fakePassKey = "FAKE_PASS_KEY_FOR_E2E_ONLY_NEVER_LOG_482196";
const fakePass = createServer(async (req, res) => {
  const chunks = []; for await (const chunk of req) chunks.push(chunk);
  uploadRequests.push({ method: req.method, url: req.url, auth: req.headers.authorization, contentType: req.headers["content-type"], body: Buffer.concat(chunks) });
  res.writeHead(201, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ url: `http://127.0.0.1:${fakePass.address().port}/published/${uploadRequests.length}` }));
});
await new Promise((resolve) => fakePass.listen(0, "127.0.0.1", resolve));
const fakePassURL = `http://127.0.0.1:${fakePass.address().port}`;
for (const service of ["passfile", "passpage"]) {
  mkdirSync(join(base, "config", service), { mode: 0o700 });
  writeFileSync(join(base, "config", service, "key"), fakePassKey + "\n", { mode: 0o600 });
}
writeFileSync(join(base, "config/passstream/config"), `server = ${origin}\nport = ${daemonPort}\nbrowser = ${wrapper}\npassfile_server = ${fakePassURL}\npasspage_server = ${fakePassURL}\n`, { mode: 0o600 });
const dataDir = join(base, "data");

let server, daemon;
const logs = { server: join(out, "server.log"), daemon: join(out, "daemon.log") };
async function startServer() {
  server = spawn(bin, ["serve", "--data", dataDir, "--listen", `127.0.0.1:${httpPort}`, "--base-url", origin, "--ice-udp", "127.0.0.1:18643", "--ice-tcp", "127.0.0.1:18643", "--turn", "127.0.0.1:13678"], { stdio: ["ignore", "pipe", "pipe"] });
  server.stdout.on("data", (b) => appendFileSync(logs.server, b));
  server.stderr.on("data", (b) => appendFileSync(logs.server, b));
  for (let i = 0; i < 200; i++) {
    try {
      if ((await fetch(origin + "/readyz")).ok) break;
    } catch {}
    await sleep(25);
  }
  const keyFile = join(base, "config/passstream/key");
  if (!existsSync(keyFile)) writeFileSync(keyFile, readFileSync(join(dataDir, "owner-key"), "utf8").trim() + "\n", { mode: 0o600 });
}
async function killServer() {
  server.kill("SIGKILL");
  await new Promise((r) => server.once("exit", r));
}
async function startDaemon() {
  daemon = spawn(bin, ["host", "--idle-exit", "-1s"], { env, stdio: ["ignore", "pipe", "pipe"] });
  daemon.stdout.on("data", (b) => appendFileSync(logs.daemon, b));
  daemon.stderr.on("data", (b) => appendFileSync(logs.daemon, b));
  for (let i = 0; i < 200; i++) {
    if (existsSync(join(env.XDG_RUNTIME_DIR, "passstream/host.sock")) && cli(["status", "--json"]).code === 0) return;
    await sleep(25);
  }
  throw Error("daemon did not start");
}
async function killDaemon() {
  daemon.kill("SIGKILL");
  await new Promise((r) => daemon.once("exit", r));
}
function cli(args, input) {
  const p = spawnSync(bin, args, { env, input, encoding: "utf8", timeout: 30000 });
  let json = null;
  try {
    json = JSON.parse(p.stdout);
  } catch {}
  return { code: p.status, stdout: p.stdout, stderr: p.stderr, json };
}
function cliAsync(args) {
  return new Promise((resolveP) => {
    const p = spawn(bin, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    p.stdout.on("data", (b) => (stdout += b));
    p.stderr.on("data", (b) => (stderr += b));
    p.on("exit", (code) => {
      let json = null;
      try {
        json = JSON.parse(stdout);
      } catch {}
      resolveP({ code, stdout, stderr, json });
    });
  });
}

// ---------------------------------------------------------------- browsers
let browser;
const sent = []; // everything any page sent: for the "server never receives k/s" check
function watch(p, who) {
  p.on("request", (r) => sent.push({ who, kind: "http", url: r.url(), headers: JSON.stringify(r.headers()), body: r.postData() ?? "" }));
  p.on("websocket", (ws) => {
    sent.push({ who, kind: "ws-open", url: ws.url(), headers: "", body: "" });
    ws.on("framesent", (f) => sent.push({ who, kind: "ws", url: ws.url(), headers: "", body: String(f.payload) }));
  });
  p.on("pageerror", (e) => appendFileSync(join(out, "browser.log"), `[${who}] pageerror ${e}\n`));
  p.on("console", (m) => {
    if (m.type() === "error") appendFileSync(join(out, "browser.log"), `[${who}] ${m.text()}\n`);
  });
  p.on("response", (r) => {
    if (r.status() >= 400) appendFileSync(join(out, "browser.log"), `[${who}] ${r.status()} ${r.url().replace(/#.*/, "")}\n`);
  });
}
let senderCtx;
async function newSenderPage() {
  const p = await senderCtx.newPage();
  watch(p, "sender");
  return p;
}
async function viewer(link, { pin = null, ctx = null, expectPin = true, timeout = 20000 } = {}) {
  const c = ctx ?? (await browser.newContext());
  const p = await c.newPage();
  watch(p, "viewer");
  await p.goto(link);
  if (pin !== null) {
    await p.getByTestId("pin-screen").waitFor({ timeout });
    await p.locator("input").first().fill(pin);
  }
  return p;
}
let currentSender = null;
async function firstFrame(p, timeout = 20000) {
  try {
    await p.waitForFunction(() => window.passstream?.firstFrame > 0, null, { timeout });
  } catch (e) {
    const d = await diag(p, currentSender);
    writeFileSync(join(out, `diag-${Date.now()}.json`), JSON.stringify(d, null, 2));
    throw Error(`no first frame: ${JSON.stringify(d).slice(0, 1500)}`);
  }
}
const frames = (p) => p.evaluate(() => window.passstream?.frameTimeline.length ?? 0);
const view = (p) => p.evaluate(() => window.passstreamViewer?.view ?? null);
async function freshFrames(p, ms = 1500) {
  const a = await frames(p);
  await sleep(ms);
  return (await frames(p)) - a;
}

// Positive playback checks prove progress, without assuming a selected temporal
// layer's frame rate. Fixed privacy/cutoff and outage observations stay separate.
// PLAN: a gap over 500 ms is a freeze. Viewers who are not the target of a kick or lock must not freeze.
const FREEZE_MS = 500;
async function playbackProgress(p, minimum = 4) {
  const after = Date.now();
  await p.waitForFunction(({ after, minimum }) => (window.passstream?.frameTimeline ?? []).filter((f) => f.at > after).length >= minimum, { after, minimum }, { timeout: 10000 }).catch(() => {});
  return { frames: await p.evaluate((t) => (window.passstream?.frameTimeline ?? []).filter((f) => f.at > t).length, after), observation_ms: Date.now() - after };
}

async function diag(v, sp) {
  const vd = await v.evaluate(() => ({ view: window.passstreamViewer?.view, events: window.passstreamViewer?.events, drops: window.passstreamViewer?.drops, state: window.passstream?.state, quality: window.passstream?.quality, sev: window.passstream?.events.slice(0, 40) })).catch((e) => String(e));
  const sd = await sp?.evaluate(() => ({ phase: window.passstreamSender?.phase, drops: window.passstreamSender?.drops, sev: window.passstream?.events.slice(-20), q: window.passstream?.quality })).catch((e) => String(e));
  return { viewer: vd, sender: sd };
}
/** Start a share through the CLI; Playwright plays the daemon-launched sender window. */
async function share(args = [], { tone = TONE_HZ, source = "clock", usePicker = false, safe = false } = {}) {
  const before = new Set(readdirSync(launches));
  // Safe mode would cover the timing sources' changing text: only its own tests turn it on.
  const pending = cliAsync(["share", "--no-wait", "--json", ...(safe ? [] : ["--no-safe"]), ...args]);
  let file;
  for (let i = 0; i < 400 && !file; i++) {
    file = readdirSync(launches).find((f) => f.endsWith(".args") && !before.has(f));
    if (!file) await sleep(25);
  }
  if (!file) throw Error(`no browser launch: ${JSON.stringify(await pending)}`);
  const argv = readFileSync(join(launches, file), "utf8").split("\n");
  const app = argv.find((a) => a.startsWith("--app=")).slice(6);
  const html = readFileSync(new URL(app), "utf8");
  const url = html.match(/url=([^"'>\s]+)/i)?.[1]?.replaceAll("&amp;", "&");
  if (!url) throw Error("no URL in the launch file");
  const p = await newSenderPage();
  if (usePicker) await p.addInitScript(() => {
    // Exercise the normal production UI without a picker or sender test hook.
    navigator.mediaDevices.getDisplayMedia = async () => {
      const canvas = document.createElement("canvas"); canvas.width = 320; canvas.height = 180;
      const ctx = canvas.getContext("2d"); let n = 0;
      const draw = () => { ctx.fillStyle = "#f0f2e8"; ctx.fillRect(0, 0, 320, 180); ctx.fillStyle = "#253443"; ctx.font = "20px monospace"; ctx.fillText(`Approval regression ${n++}`, 10, 80); };
      draw(); setInterval(draw, 100);
      return canvas.captureStream(10);
    };
  });
  await p.goto(url.replace("/sender#", `/sender?test=1&source=${source}${tone ? `&tone=${tone}` : ""}#`));
  await p.getByTestId(usePicker ? "share" : "test-source").click({ timeout: 15000 });
  const r = await pending;
  if (r.code !== 0) throw Error(`share failed: ${r.stdout} ${r.stderr}`);
  await p.waitForFunction(() => document.querySelector('[data-testid="sender"]')?.getAttribute("data-phase") === "live", null, { timeout: 15000 });
  const pin = cli(["pin", "--reveal", "--json"]).json?.pin ?? null;
  currentSender = p;
  return { page: p, link: r.json.link, id: r.json.stream_id, pin, pid: Number(file.split(".")[0]) };
}
async function stopShare() {
  cli(["stop", "--json"]);
  for (let i = 0; i < 100; i++) {
    if (cli(["status", "--json"]).json?.stream == null) return;
    await sleep(50);
  }
}
const status = () => cli(["status", "--json"]).json;
const viewersList = () => cli(["viewers", "--json"]).json;

// ---------------------------------------------------------------- SFU's-eye viewer
// A viewer that joins with the capability but never admits, and records the
// encoded frames exactly as the SFU forwards them (the SFU doesn't touch
// payloads), video and audio. They are parsed as SFrame in Node (sframeCheck).
async function sfuEye(link, { twcc = true, waitMs = 0 } = {}) {
  const c = await browser.newContext({ bypassCSP: true });
  const p = await c.newPage();
  await p.goto(origin + "/privacy");
  const cap = new URL(link).hash.match(/c=([^&]+)/)[1];
  return { page: p, result: p.evaluate(async ({ cap, twcc, waitMs }) => {
    const worker = new Worker(URL.createObjectURL(new Blob([`
      const n = { video: 0, audio: 0 };
      onrtctransform = (e) => {
        const t = e.transformer, kind = t.options.kind;
        t.readable.pipeThrough(new TransformStream({ transform(frame) {
          if (n[kind]++ < 600) postMessage({ kind, type: frame.type, data: new Uint8Array(frame.data) });
        } })).pipeTo(t.writable);
      };`], { type: "text/javascript" })));
    const withoutTWCC = sdp => sdp.replace(/^a=extmap:[^\r\n]*transport-wide-cc[^\r\n]*\r?\n/gm, "").replace(/^a=rtcp-fb:[^\r\n]*transport-cc[^\r\n]*\r?\n/gm, "");
    const got = [];
    worker.onmessage = (e) => got.push(e.data);
    const ws = new WebSocket(`ws://${location.host}/signal`);
    const pc = new RTCPeerConnection();
    const heartbeat = pc.createDataChannel("heartbeat", { ordered: false, maxRetransmits: 0 });
    // This capture probe discards ciphertext before decoding. Simulate decoder
    // diagnostics so the controller can exercise native as well as low delivery;
    // the forced-low probe still withholds TWCC capacity proof.
    const reports = setInterval(() => {
      if (heartbeat.readyState === "open") heartbeat.send(JSON.stringify({ type: "heartbeat", at: Date.now(), freshFrames: got.filter(g => g.kind === "video").length }));
    }, 500);
    const t = pc.addTransceiver("video", { direction: "recvonly" });
    t.setCodecPreferences(RTCRtpReceiver.getCapabilities("video").codecs.filter((x) => x.mimeType === "video/VP9"));
    t.receiver.transform = new RTCRtpScriptTransform(worker, { kind: "video" });
    const a = pc.addTransceiver("audio", { direction: "recvonly" });
    a.receiver.transform = new RTCRtpScriptTransform(worker, { kind: "audio" });
    pc.onicecandidate = (e) => e.candidate && ws.send(JSON.stringify({ type: "candidate", candidate: e.candidate.toJSON() }));
    await new Promise((r) => (ws.onopen = r));
    ws.onmessage = async (e) => {
      const m = JSON.parse(e.data);
      if (m.type === "joined") {
        const o = await pc.createOffer();
        if (!twcc) o.sdp = withoutTWCC(o.sdp);
        await pc.setLocalDescription(o);
        ws.send(JSON.stringify({ type: "offer", sdp: o }));
      } else if (m.type === "answer") await pc.setRemoteDescription(m.sdp);
      else if (m.type === "candidate") await pc.addIceCandidate(m.candidate).catch(() => {});
    };
    ws.send(JSON.stringify({ type: "join", role: "view", id: "", credential: cap, protocol: 1 }));
    const video = () => got.filter((g) => g.kind === "video");
    for (let i = 0; i < Math.max(100, waitMs / 100) && (i * 100 < waitMs || video().length < 30 || video().filter((g) => g.type === "key").length < 1 || got.length - video().length < 30); i++) await new Promise((r) => setTimeout(r, 100));
    clearInterval(reports);
    ws.close();
    pc.close();
    const entropy = (b) => {
      const c = new Array(256).fill(0);
      for (const x of b) c[x]++;
      let h = 0;
      for (const n of c) if (n) h -= (n / b.length) * Math.log2(n / b.length);
      return h;
    };
    const vf = video();
    const keys = vf.filter((g) => g.type === "key");
    const all = vf.flatMap((g) => [...g.data]);
    // VP9 keyframe: frame marker + profile, then sync code 0x49 0x83 0x42.
    const hasSync = (d) => { for (let i = 0; i + 2 < Math.min(d.length, 24); i++) if (d[i] === 0x49 && d[i + 1] === 0x83 && d[i + 2] === 0x42) return true; return false; };
    const sync = vf.filter((g) => hasSync(g.data)).length;
    const b64 = (d) => { let s = ""; for (let i = 0; i < d.length; i += 0x8000) s += String.fromCharCode(...d.subarray(i, i + 0x8000)); return btoa(s); };
    // Positive control for the detectors: a plaintext VP9 loopback in this page.
    const control = await (async () => {
      const w2 = new Worker(URL.createObjectURL(new Blob([`
        let n = 0;
        onrtctransform = (e) => { const t = e.transformer; t.readable.pipeThrough(new TransformStream({ transform(f, c) { if (n++ < 60) postMessage({ type: f.type, data: new Uint8Array(f.data) }); c.enqueue(f); } })).pipeTo(t.writable); };`], { type: "text/javascript" })));
      const g2 = [];
      w2.onmessage = (e) => g2.push(e.data);
      const cv = document.createElement("canvas");
      cv.width = 640; cv.height = 360;
      const cx = cv.getContext("2d");
      let i = 0;
      const tm = setInterval(() => { cx.fillStyle = "#eee"; cx.fillRect(0, 0, 640, 360); cx.fillStyle = "#123"; cx.font = "30px monospace"; cx.fillText("plaintext control " + i++, 20, 100); }, 66);
      const a = new RTCPeerConnection(), b = new RTCPeerConnection();
      a.onicecandidate = (e) => e.candidate && b.addIceCandidate(e.candidate);
      b.onicecandidate = (e) => e.candidate && a.addIceCandidate(e.candidate);
      const tr = a.addTransceiver(cv.captureStream(15).getVideoTracks()[0], { direction: "sendonly" });
      tr.setCodecPreferences(RTCRtpSender.getCapabilities("video").codecs.filter((x) => x.mimeType === "video/VP9"));
      b.ontrack = (e) => { e.receiver.transform = new RTCRtpScriptTransform(w2, {}); };
      await a.setLocalDescription(await a.createOffer());
      await b.setRemoteDescription(a.localDescription);
      await b.setLocalDescription(await b.createAnswer());
      await a.setRemoteDescription(b.localDescription);
      for (let k = 0; k < 50 && g2.length < 30; k++) await new Promise((r) => setTimeout(r, 100));
      clearInterval(tm); a.close(); b.close();
      const ck = g2.filter((g) => g.type === "key");
      const cs = ck.filter((g) => hasSync(g.data)).length;
      return { frames: g2.length, keyframes: ck.length, vp9_sync_codes: cs, entropy_bits_per_byte: g2.length ? entropy(g2.flatMap((g) => [...g.data])) : null };
    })();
    return {
      frames: vf.length, keyframes: keys.length, vp9_sync_codes: sync, entropy_bits_per_byte: all.length ? entropy(all) : null, keyframe_entropy: keys.length ? entropy(keys.flatMap((g) => [...g.data])) : null, bytes: all.length, plaintext_control: control,
      raw: { video: vf.map((g) => b64(g.data)), audio: got.filter((g) => g.kind === "audio").map((g) => b64(g.data)) },
    };
  }, { cap, twcc, waitMs }) };
}

/**
 * Parse what the SFU forwarded as SFrame (RFC 9605 §4.3) with the library's
 * own parser: every frame has a minimal header whose KID is epoch×16+track,
 * CTRs increase per KID, the whole frame (no clear prefix) authenticates
 * with that epoch's real key, and fails with a wrong key.
 */
async function sframeCheck(frames, track, epochSecrets) {
  const r = { frames: frames.length, parsed: 0, wrong_track: 0, ctr_not_increasing: 0, unknown_epoch: 0, right_key_ok: 0, right_key_fail: 0, wrong_key_ok: 0, plaintext_needle_hits: 0, epochs: [] };
  const last = new Map(), keys = new Map(), wrong = new Map();
  for (const b64 of frames) {
    const data = new Uint8Array(Buffer.from(b64, "base64"));
    let h;
    try {
      h = decodeHeader(data);
    } catch {
      continue;
    }
    r.parsed++;
    const { epoch, track: t } = splitKid(h.kid);
    if (!(Array.isArray(track) ? track : [track]).includes(t)) r.wrong_track++;
    if (!r.epochs.includes(epoch)) r.epochs.push(epoch);
    const prev = last.get(h.kid);
    if (prev !== undefined && h.ctr <= prev) r.ctr_not_increasing++;
    last.set(h.kid, h.ctr);
    const secret = epochSecrets.get(epoch);
    if (!secret) {
      r.unknown_epoch++;
      continue;
    }
    if (!keys.has(h.kid)) {
      keys.set(h.kid, await SFrameKey.create({ kid: h.kid, baseKey: deriveBaseKey(secret, h.kid), direction: "decrypt" }));
      wrong.set(h.kid, await SFrameKey.create({ kid: h.kid, baseKey: deriveBaseKey(new Uint8Array(32).fill(7), h.kid), direction: "decrypt" }));
    }
    try {
      const plaintext = await keys.get(h.kid).decrypt(data);
      if (plaintext.length >= 32 && Buffer.from(data).includes(plaintext.subarray(0, 32))) r.plaintext_needle_hits++;
      r.right_key_ok++;
    } catch {
      r.right_key_fail++;
    }
    try {
      await wrong.get(h.kid).decrypt(data);
      r.wrong_key_ok++;
    } catch {}
  }
  r.pass = r.frames > 0 && r.parsed === r.frames && r.wrong_track === 0 && r.ctr_not_increasing === 0 && r.unknown_epoch === 0 && r.right_key_fail === 0 && r.right_key_ok === r.frames && r.wrong_key_ok === 0 && r.plaintext_needle_hits === 0;
  return r;
}
async function epochSecretsOf(senderPage) {
  const list = await senderPage.evaluate(() => window.passstreamSender?.testSecrets?.epochs ?? []);
  return new Map(list.map((e) => [e.epoch, new Uint8Array(Buffer.from(e.secret, "base64url"))]));
}

// ---------------------------------------------------------------- audio probes
/** Peak frequency and its level over a remote audio track, through an AnalyserNode. */
async function listen(p, ms = 1500) {
  return p.evaluate(async (ms) => {
    const pc = window.passstream?.pc;
    const rx = pc?.getReceivers().find((r) => r.track?.kind === "audio");
    if (!rx) return { error: "no audio receiver" };
    // A recovered PC has a fresh receiver. Pulling the retired track can
    // falsely report silence, and can also miss a confidentiality regression.
    if (window.__probe && window.__probe.track !== rx.track) {
      window.__probe.source.disconnect();
      window.__probe.el.pause();
      window.__probe.el.srcObject = null;
      void window.__probe.ctx.close().catch(() => {});
      window.__probe = null;
    }
    window.__probe ??= (() => {
      const ctx = new AudioContext();
      const stream = new MediaStream([rx.track]);
      // Chrome only pulls remote WebRTC audio into WebAudio while a media element plays it.
      const el = new Audio();
      el.srcObject = stream;
      el.muted = true;
      void el.play().catch(() => {});
      const an = ctx.createAnalyser();
      an.fftSize = 8192;
      an.smoothingTimeConstant = 0;
      const source = ctx.createMediaStreamSource(stream);
      source.connect(an);
      void ctx.resume();
      return { ctx, an, el, source, track: rx.track };
    })();
    const { ctx, an } = window.__probe;
    const bins = new Float32Array(an.frequencyBinCount);
    let best = { hz: 0, db: -Infinity }, samples = 0, hits = 0;
    const until = performance.now() + ms;
    while (performance.now() < until) {
      await new Promise((r) => setTimeout(r, 100));
      an.getFloatFrequencyData(bins);
      let i = 0;
      for (let k = 1; k < bins.length; k++) if (bins[k] > bins[i]) i = k;
      const hz = (i * ctx.sampleRate) / an.fftSize;
      samples++;
      if (bins[i] > -60 && Math.abs(hz - 440) < 15) hits++;
      if (bins[i] > best.db) best = { hz, db: bins[i] };
    }
    return { peak_hz: Math.round(best.hz), peak_db: Math.round(best.db), tone_samples: hits, samples, ctx_state: ctx.state };
  }, ms);
}
/** Negotiated m-lines on the sender's PC, and whether its audio sender is encrypted. */
const senderMlines = (p) =>
  p.evaluate(() => {
    const pc = window.passstream.pc;
    const sdp = pc.localDescription?.sdp ?? "";
    const a = pc.getTransceivers().find((t) => t.receiver.track?.kind === "audio");
    return { audio: (sdp.match(/^m=audio/gm) ?? []).length, video: (sdp.match(/^m=video/gm) ?? []).length, audio_transform: !!a?.sender.transform };
  });
/** Share a fresh capture from the sender page: a canvas, and a tone if hz > 0. */
const recapture = (p, hz) =>
  p.evaluate(async (hz) => {
    const c = document.createElement("canvas");
    c.width = 640;
    c.height = 360;
    const g = c.getContext("2d");
    let n = 0;
    setInterval(() => {
      g.fillStyle = `hsl(${n++ % 360} 60% 50%)`;
      g.fillRect(0, 0, 640, 360);
    }, 50);
    const stream = c.captureStream(15);
    if (hz) {
      const ctx = new AudioContext();
      const o = ctx.createOscillator();
      o.frequency.value = hz;
      const d = ctx.createMediaStreamDestination();
      o.connect(d);
      o.start();
      await ctx.resume().catch(() => {});
      stream.addTrack(d.stream.getAudioTracks()[0]);
    }
    await window.passstreamSender.share(stream);
  }, hz);
const audioReports = (p, since) => p.evaluate((t) => window.passstreamViewer.decrypted.filter((d) => d.track === 1 && d.at > t), since);

// ---------------------------------------------------------------- tests
const results = [];
async function test(name, fn) {
  if (filter && !filter.test(name)) return;
  const started = Date.now();
  const loadStart = readFileSync("/proc/loadavg", "utf8").trim();
  let r;
  try {
    r = { pass: false, ...(await fn()) };
  } catch (e) {
    r = { pass: false, error: String(e).slice(0, 800), stack: e.stack?.slice(0, 2000) };
  }
  r = { name, ms: Date.now() - started, load_start: loadStart, load_end: readFileSync("/proc/loadavg", "utf8").trim(), ...r };
  results.push(r);
  console.log(`${r.pass ? "PASS" : "FAIL"} ${name} ${JSON.stringify({ ...r, name: undefined })}`);
  await harvest().catch(() => {});
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
const secrets = []; // every PIN, link secret and key we saw, for the leak scan
const snapshotNeedles = [];
const recordingNeedles = [];

/**
 * Collect the real key material of the pages still open, before the stream
 * ends (which deletes the sender's record): epoch secrets (sender test hook),
 * identity seeds (the sender's IndexedDB record) and viewer grant secrets.
 */
async function harvest() {
  for (const p of senderCtx.pages()) {
    const h = await p
      .evaluate(async () => {
        const c = window.passstreamSender;
        if (!c) return null;
        const db = await new Promise((res, rej) => {
          const r = indexedDB.open("passstream-sender");
          r.onsuccess = () => res(r.result);
          r.onerror = () => rej(r.error);
        });
        const all = await new Promise((res) => {
          const r = db.transaction("kv").objectStore("kv").getAll();
          r.onsuccess = () => res(r.result);
        });
        db.close();
        const seeds = [];
        for (const v of all) {
          try {
            const j = JSON.parse(v);
            if (j?.identity_seed) seeds.push(j.identity_seed);
          } catch {}
        }
        return { epochs: (c.testSecrets?.epochs ?? []).map((e) => e.secret), seeds };
      })
      .catch(() => null);
    if (!h) continue;
    for (const e of h.epochs) secrets.push({ epoch_secret: e });
    for (const seed of h.seeds) secrets.push({ identity_seed: seed });
  }
  for (const c of browser.contexts()) {
    if (c === senderCtx) continue;
    for (const p of c.pages()) {
      const grants = await p
        .evaluate(() => {
          const out = [];
          for (let i = 0; i < sessionStorage.length; i++) {
            const k = sessionStorage.key(i);
            if (!k?.startsWith("ps:viewer:")) continue;
            try {
              const g = JSON.parse(sessionStorage.getItem(k))?.grant;
              if (g?.secret) out.push(g.secret);
            } catch {}
          }
          return out;
        })
        .catch(() => []);
      for (const g of grants) secrets.push({ grant_secret: g });
    }
  }
}

try {
  await test("agent_publish_requires_trusted_gesture", async () => {
    const s = await share(["--pin", "4"], { usePicker: true, tone: 0 });
    secrets.push({ pin: s.pin, link: s.link });
    const hooks = await s.page.evaluate(() => ({
      sender_global: typeof window.passstreamSender !== "undefined",
      test_source: !!document.querySelector('[data-testid="test-source"]'),
    }));
    const before = uploadRequests.length;
    const request = await cliAsync(["publish", "snapshot", "passfile", "--json"]);
    if (request.json?.status !== "pending") throw Error(`publish not pending: ${request.stdout} ${request.stderr}`);
    const dialog = s.page.getByRole("dialog", { name: "Confirm publishing" });
    const confirm = dialog.getByRole("button", { name: "Confirm upload", exact: true });
    await confirm.waitFor();
    await confirm.evaluate((button) => button.click());
    await sleep(150);
    const domUploads = uploadRequests.length - before;
    const probe = await s.page.evaluate(() => {
      try {
        // Keep the reviewer's exact bypass as a regression.
        const c = window.passstreamSender.cfg.channel;
        c.decidePublish(c.getPublishPrompt().request_id, true);
        return { rejected: false };
      } catch { return { rejected: true }; }
    });
    const alternate = await s.page.evaluate(async (id) => {
      const c = window.passstreamSender?.cfg.channel;
      if (!c) return { attempted: false };
      const decision = { type: "publish_decision", request_id: id, approved: true };
      c.sendCapture(decision);
      await c.sendArtifact(decision);
      c.sendResult("not-a-command", { ...decision, ok: true, data: null });
      return { attempted: true };
    }, request.json.request_id);
    await sleep(150);
    const scriptUploads = uploadRequests.length - before - domUploads;
    const pending = await cliAsync(["publish", "status", request.json.request_id, "--json"]);
    await confirm.click(); // Playwright input produces the trusted native click.
    let status;
    for (let i = 0; i < 100; i++) {
      status = await cliAsync(["publish", "status", request.json.request_id, "--json"]);
      if (status.json?.status !== "pending" && !status.json?.uploading) break;
      await sleep(50);
    }
    await sleep(150);
    const trustedUploads = uploadRequests.length - before - domUploads - scriptUploads;
    const production = process.env.E2E_REQUIRE_PRODUCTION === "1";
    return { pass: probe.rejected && domUploads === 0 && scriptUploads === 0 && pending.json?.status === "pending" && trustedUploads === 1 && status.json?.status === "confirmed" && status.json?.url?.startsWith(fakePassURL) && (!production || (!hooks.sender_global && !hooks.test_source)),
      production_required: production, ...hooks, exact_probe_rejected: probe.rejected,
      alternate_envelope_probe: alternate.attempted,
      dom_click_uploads: domUploads, channel_script_uploads: scriptUploads, trusted_click_uploads: trustedUploads,
      status_before_click: pending.json?.status, status_after_click: status.json?.status };
  });
  if(process.env.E2E_CAPTURE_BENCH==="1") for(const [width,height] of [[1920,1080],[3840,2160]]) await test(`agent_capture_cost_${height}`,async()=>{
    const s=await share(["--pin","4"],{tone:0});secrets.push({pin:s.pin,link:s.link});
    const v=await viewer(s.link,{pin:s.pin});await firstFrame(v);
    await s.page.evaluate(async({width,height})=>{
      const c=document.createElement("canvas");c.width=width;c.height=height;document.body.append(c);const x=c.getContext("2d");let n=0;
      const draw=()=>{x.fillStyle="#f8f8f0";x.fillRect(0,0,width,height);x.fillStyle="#253443";x.font="30px monospace";for(let y=60;y<height;y+=60)x.fillText(`PassStream capture measurement ${width}x${height} · line ${y} · frame ${n}`,30,y);n++;};
      draw();window.captureBenchTimer=setInterval(draw,1000/15);await window.passstreamSender.share(c.captureStream(15));
    },{width,height});
    const cd=await senderCtx.newCDPSession(s.page);await cd.send("Performance.enable");
    await sleep(4000);
    async function point(){
      const metrics=(await cd.send("Performance.getMetrics")).metrics;
      const cpu=metrics.find(m=>m.name==="ProcessTime")?.value;if(typeof cpu!=="number")throw Error("publisher renderer CPU time is unavailable");
      const video=await s.page.evaluate(async()=>{const pc=window.passstreamSender.session.pc;const mid=pc.getTransceivers().find(t=>t.receiver.track.kind==="video").mid;const stats=await pc.getStats();const row=[...stats.values()].find(r=>r.type==="outbound-rtp"&&r.kind==="video"&&r.mid===mid);return {frames:row.framesEncoded,dimensions:[row.frameWidth,row.frameHeight],...window.passstreamSender.features.metrics};});
      return {cpu,at:performance.now(),...video};
    }
    async function measure(){const a=await point();await sleep(15000);const b=await point();const seconds=(b.at-a.at)/1000;return {seconds,publisher_renderer_cpu_percent:100*(b.cpu-a.cpu)/seconds,publisher_fps:(b.frames-a.frames)/seconds,readbacks:b.readbacks-a.readbacks,png_encodes:b.encodes-a.encodes,dimensions:b.dimensions};}
    const idle=await measure();
    await cliAsync(["frame","--json"]);await sleep(1500);
    const active=await measure();await s.page.evaluate(()=>clearInterval(window.captureBenchTimer));
    return {pass:idle.readbacks===0&&idle.png_encodes===0&&active.readbacks>0&&active.png_encodes>0&&idle.dimensions[0]===width&&active.dimensions[0]===width,width,height,source_fps:15,idle,active};
  });
  await test("agent_frames_local_change_privacy_record_and_publish", async () => {
    const uploadStart = uploadRequests.length;
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    await s.page.evaluate(async () => {
      const canvas = document.createElement("canvas"); canvas.id = "agent-canvas"; canvas.width = 320; canvas.height = 180; document.body.append(canvas);
      const ctx = canvas.getContext("2d"), pixels = ctx.createImageData(320, 180);
      let seed = 0x71d39a;
      for (let i = 0; i < pixels.data.length; i++) { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; pixels.data[i] = i % 4 === 3 ? 255 : seed >>> 24; }
      ctx.putImageData(pixels, 0, 0);
      await window.passstreamSender.share(canvas.captureStream(10));
    });
    async function getFrame(after = "") {
      for (let i = 0; i < 60; i++) {
        const r = cli(["frame", "--json", ...(after ? ["--after", after] : [])]);
        if (r.code !== 0) throw Error(`frame failed ${r.stderr}`);
        if (r.json.status === "frame" && r.json.width === 320) return r.json;
        await sleep(100);
      }
      throw Error("no changed native frame");
    }
    const first = await getFrame();
    const rawPNG = Buffer.from(first.data, "base64");
    // Actual PNG content, excluding its common header, is a secret needle.
    snapshotNeedles.push(...artifactNeedles(rawPNG));
    await sleep(1100);
    const unchanged = cli(["frame", "--after", first.id, "--json"]).json;
    const pending = cliAsync(["frame", "--after", first.id, "--wait", "5", "--json"]);
    await s.page.evaluate(() => { const c = document.getElementById("agent-canvas"); const ctx = c.getContext("2d"); ctx.fillStyle = "#fa4177"; ctx.fillRect(15, 15, 50, 50); });
    const changed = await pending;
    snapshotNeedles.push(...artifactNeedles(Buffer.from(changed.json.data,"base64"),rawPNG));
    const scaled = cli(["frame", "--max-width", "160", "--json"]).json;
    const auth = cli(["capture-auth", "--json"]).json;
    const unauthorized = await fetch(`http://127.0.0.1:${daemonPort}/local/v1/frame`);
    const remoteDenied = await fetch(`http://127.0.0.1:${daemonPort}/local/v1/frame`, { headers: { Authorization: `Bearer ${auth.token}`, Origin: origin } });
    const local = await fetch(`http://127.0.0.1:${daemonPort}/local/v1/frame`, { headers: { Authorization: `Bearer ${auth.token}` } });
    const localFrame = await local.json();
    const recordingSentAt=sent.length;
    const recordStart = await cliAsync(["record", "start", "--duration", "1", "--json"]);
    await sleep(1800);
    const recordStop = await cliAsync(["record", "stop", "--json"]);
    const recordingChunks=sent.slice(recordingSentAt).flatMap((r)=>{try{const m=JSON.parse(r.body);return m.type==="recording"?[Buffer.from(m.data,"base64")]:[];}catch{return [];}});
    for(const chunk of recordingChunks) recordingNeedles.push(...artifactNeedles(chunk));
    const published=[];let unconfirmedUploads=0;
    for(const args of [["snapshot","passfile"],["snapshot","passpage"],["recording","passfile"]]) {
      const before=uploadRequests.length;
      const request=await cliAsync(["publish",...args,"--json"]);
      if(request.json?.status!=="pending")throw Error(`not pending: ${request.stderr} ${request.stdout}`);
      unconfirmedUploads+=uploadRequests.length-before;
      const dialog=s.page.getByRole("dialog",{name:"Confirm publishing"});
      await dialog.waitFor();
      if(!(await dialog.innerText()).includes(new URL(fakePassURL).host))throw Error("destination missing from prompt");
      if(args[0]==="recording" && !/seconds/.test(await dialog.innerText()))throw Error("recording duration missing");
      await dialog.getByRole("button",{name:"Confirm upload",exact:true}).click();
      let result;
      for(let i=0;i<100;i++){result=await cliAsync(["publish","status",request.json.request_id,"--json"]);if(result.json?.status!=="pending"&&!result.json?.uploading)break;await sleep(100);}
      published.push(result);
    }
    // A static canvas only captures on draw. Animate it while the SFU probe
    // joins so its negative needle scan has real ciphertext to inspect.
    await s.page.evaluate(() => {
      const c = document.getElementById("agent-canvas"), ctx = c.getContext("2d");
      let i = 0;
      window.agentCanvasTimer = setInterval(() => { ctx.fillStyle = `rgb(${i++ % 255},40,100)`; ctx.fillRect(0, 0, 12, 12); }, 100);
    });
    const eye = await sfuEye(s.link, { waitMs: 1500 });
    const observed = await eye.result;
    await s.page.evaluate(() => clearInterval(window.agentCanvasTimer));
    const sfuHits = [...observed.raw.video, ...observed.raw.audio].filter((f) => [...snapshotNeedles,...recordingNeedles].some((n) => Buffer.from(f, "base64").includes(n))).length;
    const pause = await cliAsync(["pause", "--json"]);
    const paused = cli(["frame", "--json"]).json;
    const refusedUpload = await cliAsync(["publish", "snapshot", "passfile", "--json"]);
    const refusedRecording = await cliAsync(["publish", "recording", "passfile", "--json"]);
    const remote = await fetch(`${origin}/api/v1/streams/${s.id}/frame`, { headers: { Authorization: `Bearer ${readFileSync(join(base, "config/passstream/key"), "utf8").trim()}` } });
    const requests = uploadRequests.slice(uploadStart);
    const requestShapes = requests.length === 3 && requests[0].method === "PUT" && requests[0].url === "/api/v1/files?filename=snapshot.png" && requests[1].method === "POST" && requests[1].url === "/api/shares" && requests[2].url === "/api/v1/files?filename=recording.webm" && requests.every((r) => r.auth === `Bearer ${fakePassKey}`);
    const pngUploaded = requests[0]?.body.equals(Buffer.from(changed.json.data, "base64"));
    const webm = requests[2]?.body;
    const webmHeader = webm && webm.subarray(0, 4).equals(Buffer.from([0x1a, 0x45, 0xdf, 0xa3]));
    return { pass: recordingChunks.length>0 && unconfirmedUploads===0 && unchanged.status === "unchanged" && changed.code === 0 && changed.json.id !== first.id && scaled.width === 160 && scaled.height === 90 && unauthorized.status === 401 && remoteDenied.status === 403 && localFrame.status === "frame" && recordStart.code === 0 && recordStop.json?.completed === true && published.every((r) => r.code === 0 && r.json.url.startsWith(fakePassURL)) && requestShapes && pngUploaded && webmHeader && pause.code === 0 && paused.status === "paused" && !paused.data && refusedUpload.code === 1 && refusedRecording.code === 1 && remote.status === 404 && sfuHits === 0 && observed.raw.video.length > 0,
      recording_chunks_scanned:recordingChunks.length, recording_needles:recordingNeedles.length, snapshot_needles:snapshotNeedles.length, unconfirmed_uploads:unconfirmedUploads, unchanged: unchanged.status, changed: changed.json?.id !== first.id, native: [first.width, first.height], scaled: [scaled.width, scaled.height], local_auth: [unauthorized.status, remoteDenied.status, local.status], record_start: recordStart.code, record_stop: recordStop.json, publishes: published.map((r) => ({ code: r.code, error: r.stderr })), upload_requests: requests.length, request_shapes: requestShapes, png_uploaded: pngUploaded, webm_header: webmHeader, recording_bytes: webm?.length, paused: paused.status, paused_uploads_refused: [refusedUpload.code, refusedRecording.code], remote_frame_status: remote.status, sfu_snapshot_hits: sfuHits, sfu_frames_scanned: observed.raw.video.length };
  });

  await test("agent_owner_controls_local_portal_and_sender", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    for (let i = 0; i < 60 && cli(["frame", "--json"]).json?.status !== "frame"; i++) await sleep(100);
    const before = uploadRequests.length;
    const controls = s.page.getByRole("region", { name: "Owner captures" });
    await controls.getByRole("button", { name: "Snapshot → PassFile", exact: true }).click();
    await s.page.getByRole("dialog",{name:"Confirm publishing"}).getByRole("button",{name:"Confirm upload",exact:true}).click();
    await controls.getByRole("link", { name: "Open published capture" }).waitFor();
    const portal = await browser.newPage(); watch(portal, "local-owner");
    await portal.goto(`http://127.0.0.1:${daemonPort}/owner?stream_id=${s.id}`);
    const disabled = await portal.getByRole("button", { name: "Snapshot → PassPage", exact: true }).isDisabled();
    const auth = cli(["capture-auth", "--json"]).json;
    await portal.getByLabel("Local owner token").fill(auth.token);
    await portal.getByRole("button", { name: "Snapshot → PassPage", exact: true }).click();
    await s.page.getByRole("dialog",{name:"Confirm publishing"}).getByRole("button",{name:"Confirm upload",exact:true}).click();
    await portal.getByRole("link", { name: "Open published capture" }).waitFor();
    const link = await portal.getByRole("link", { name: "Open published capture" }).getAttribute("href");
    return { pass: disabled && link.startsWith(fakePassURL) && uploadRequests.length === before + 2, credential_required: disabled, explicit_ui_uploads: uploadRequests.length - before };
  });

  await test("audio_probe_rebinds_after_peer_replacement", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const before = await listen(v, 1000);
    await v.evaluate(() => {
      window.__previousAudioReceiver = window.passstream.pc.getReceivers().find((r) => r.track.kind === "audio");
      window.passstream.replace();
    });
    await v.waitForFunction(() => window.passstream.pc?.connectionState === "connected" && window.passstream.pc.getReceivers().some((r) => r.track.kind === "audio" && r !== window.__previousAudioReceiver), null, { timeout: 15000 });
    await playbackProgress(v);
    const after = await listen(v, 1500);
    const current = await v.evaluate(() => window.__probe.el.srcObject.getAudioTracks()[0] === window.passstream.pc.getReceivers().find((r) => r.track.kind === "audio").track);
    return { pass: current && before.tone_samples > 0 && after.tone_samples > 0 && Math.abs(after.peak_hz - TONE_HZ) < 15, before, after, probe_follows_current_receiver: current };
  });

  await test("audio_tone_decodes_e2ee", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await v.waitForFunction(() => window.passstreamViewer.audioLive, null, { timeout: 10000 });
    const unmute = await v.getByTestId("unmute").count();
    const heard = await listen(v, 2000);
    const sfu = await v.evaluate(() => window.passstream.quality.repair?.audio ?? null);
    const tx = await s.page.evaluate(() => window.passstream.pc.getTransceivers().map((t) => ({ kind: t.receiver.track.kind, dir: t.currentDirection, codecs: t.sender.getParameters().codecs?.map((c) => c.mimeType), transform: !!t.sender.transform })));
    return {
      pass: Math.abs(heard.peak_hz - TONE_HZ) < 15 && heard.tone_samples >= heard.samples / 2 && unmute === 1 && sfu?.forwarded > 0 && tx.every((t) => t.kind === "video" || (t.transform && t.codecs?.every((c) => c === "audio/opus"))),
      heard,
      unmute_button: unmute,
      sfu_audio: sfu,
      sender_transceivers: tx,
    };
  });

  await test("no_audio_no_unmute_button", async () => {
    const s = await share(["--pin", "4"], { tone: 0 });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await sleep(2000);
    const live = await v.evaluate(() => window.passstreamViewer.audioLive);
    const unmute = await v.getByTestId("unmute").count();
    // A capture without audio negotiates no audio m-line on the publisher PC.
    const mlines = await senderMlines(s.page);
    return { pass: !live && unmute === 0 && mlines.audio === 0 && mlines.video === 2, audio_live: live, unmute_button: unmute, sender_mlines: mlines };
  });

  // Audio that appears on a recapture is added by renegotiation, encrypted.
  await test("late_audio_renegotiates_e2ee", async () => {
    const s = await share(["--pin", "4"], { tone: 0 });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const before = await senderMlines(s.page);
    await recapture(s.page, TONE_HZ);
    await s.page.waitForFunction(() => /^m=audio/m.test(window.passstream.pc.currentRemoteDescription?.sdp ?? ""), null, { timeout: 10000 });
    await v.waitForFunction(() => window.passstreamViewer.audioLive, null, { timeout: 10000 });
    const heard = await listen(v, 2000);
    const after = await senderMlines(s.page);
    const fresh = await playbackProgress(v);
    return {
      pass: before.audio === 0 && after.audio === 1 && after.audio_transform && Math.abs(heard.peak_hz - TONE_HZ) < 15 && heard.tone_samples >= heard.samples / 2 && fresh.frames > 3,
      before,
      after,
      heard,
      fresh_frames: fresh.frames, playback_observation_ms: fresh.observation_ms,
    };
  });

  // A recapture while paused attaches its audio already disabled: both on a
  // PC that had no audio (renegotiated) and on one that had (replaceTrack).
  await test("paused_recapture_attaches_muted_audio", async () => {
    const out = {};
    for (const initial of [0, TONE_HZ]) {
      const s = await share(["--pin", "4"], { tone: initial });
      const v = await viewer(s.link, { pin: s.pin });
      await firstFrame(v);
      cli(["pause", "--json"]);
      await v.waitForFunction(() => window.passstream.marker === "paused", null, { timeout: 10000 });
      await s.page.evaluate(() => {
        window.__audioAttach = [];
        const orig = RTCRtpSender.prototype.replaceTrack;
        RTCRtpSender.prototype.replaceTrack = function (t) {
          if (t?.kind === "audio") window.__audioAttach.push({ enabled: t.enabled, paused: window.passstreamSender.latches.paused });
          return orig.call(this, t);
        };
      });
      await recapture(s.page, TONE_HZ);
      await s.page.waitForFunction(() => window.__audioAttach.length > 0, null, { timeout: 10000 }).catch(() => {});
      await sleep(1500);
      const attaches = await s.page.evaluate(() => window.__audioAttach);
      const tone = await listen(v, 1200);
      cli(["resume", "--json"]);
      await v.waitForFunction(() => window.passstream.marker === null, null, { timeout: 10000 });
      await v.waitForFunction(() => window.passstreamViewer.audioLive, null, { timeout: 10000 }).catch(() => {});
      const toneAfter = await listen(v, 1500);
      out[initial ? "had_audio" : "no_audio_before"] = { attaches, tone_while_paused: tone, tone_after_resume: toneAfter, ok: attaches.length > 0 && attaches.every((a) => a.paused && !a.enabled) && tone.tone_samples === 0 && Math.abs(toneAfter.peak_hz - TONE_HZ) < 15 };
      await stopShare();
    }
    return { pass: Object.values(out).every((r) => r.ok), ...out };
  });

  // The cryptographic kick cutoff: the server's disconnect is masked (a hostile
  // SFU ignores e2ee.close), so the kicked viewer keeps its connection and
  // keeps receiving frames, and must fail to decrypt every frame captured more
  // than 300 ms + one frame interval after the kick. With `stall`, the sender's
  // UI thread is blocked for 850 ms right after the kick (no tick, no acks):
  // the encrypt worker must enforce the cutoff by itself.
  for (const stall of [0, 850]) {
    await test(stall ? "kick_cutoff_cryptographic_ui_stall" : "kick_cutoff_cryptographic", async () => {
      const s = await share(["--pin", "4"]);
      const a = await viewer(s.link, { pin: s.pin });
      const b = await viewer(s.link, { pin: s.pin });
      await firstFrame(a);
      await firstFrame(b);
      // A capture that keeps producing while the UI thread stalls (screen
      // capture runs off the main thread): a worker-drawn pixel clock, and a tone.
      await s.page.evaluate(async () => {
        const track = new MediaStreamTrackGenerator({ kind: "video" });
        const w = new Worker(URL.createObjectURL(new Blob([`
          onmessage = async (e) => {
            const writer = e.data.getWriter();
            const cv = new OffscreenCanvas(960, 540), x = cv.getContext("2d");
            let i = 0;
            setInterval(async () => {
              x.fillStyle = i++ % 2 ? "#4678ce" : "#ae5a36"; x.fillRect(0, 0, cv.width, cv.height);
              const now = Date.now();
              for (let bit = 0; bit < 64; bit++) {
                const value = bit < 48 ? Math.floor(now / 2 ** bit) % 2 : Math.floor(i / 2 ** (bit - 48)) % 2;
                x.fillStyle = value ? "#fff" : "#000"; x.fillRect(bit * 12, 0, 12, 36);
              }
              const f = new VideoFrame(cv, { timestamp: i * 66666 });
              await writer.write(f); f.close();
            }, 66);
          };`], { type: "text/javascript" })));
        w.postMessage(track.writable, [track.writable]);
        window.__source = w;
        const ctx = new AudioContext();
        const osc = ctx.createOscillator();
        osc.frequency.value = 440;
        const dest = ctx.createMediaStreamDestination();
        osc.connect(dest);
        osc.start();
        await window.passstreamSender.share(new MediaStream([track, dest.stream.getAudioTracks()[0]]));
        // Mask the server-side disconnect: confidentiality must come from the keys.
        const sess = window.passstreamSender.session;
        const send = sess.sendSignal.bind(sess);
        sess.sendSignal = (m) => (m.type === "e2ee.close" ? true : send(m));
      });
      // The remaining viewer gets the new epoch but never acks: the 300 ms timeout path.
      await b.evaluate(() => {
        const send = window.passstreamViewer.send.bind(window.passstreamViewer);
        // Drop REKEY_ACK only (wire type 0x07). A PC replacement still needs
        // its grant admission; suppressing HELLO/RESPONSE would strand it.
        window.passstreamViewer.send = bytes => { if (bytes[1] !== 0x07) send(bytes); };
      });
      await a.waitForFunction(() => window.passstreamViewer.audioLive, null, { timeout: 10000 });
      await sleep(1500);
      const code = await a.evaluate(() => window.passstreamViewer.rowCode);
      const id = await s.page.evaluate((code) => window.passstreamSender.viewers().find((v) => v.rowCode === code).admissionId, code);
      await s.page.evaluate(() => window.passstreamSender.trace(true));
      const dropsBefore = await a.evaluate(() => {
        // Sample the kicked viewer's transport: it must stay connected through the cutoff window.
        window.__conn = [];
        setInterval(() => window.__conn.push([Date.now(), window.passstream.pc?.connectionState ?? null, window.passstream.signalingOpen]), 100);
        return { ...window.passstreamViewer.drops };
      });
      const kick = await s.page.evaluate(
        async ({ id, stall }) => {
          const c = window.passstreamSender;
          const t0 = Date.now();
          const pending = c.command("window", "", "kick", { admission_id: id });
          if (stall) {
            await new Promise((r) => setTimeout(r, 40)); // the kick's rekey is under way
            const until = performance.now() + stall;
            while (performance.now() < until) {}
          }
          const r = await pending;
          return { t0, ok: r.ok, unblocked: Date.now(), epoch: c.host.keys.activeEpoch };
        },
        { id, stall },
      );
      await sleep(2000);
      const cutoff = kick.t0 + 300;
      const frameMs = 67;
      const a1 = await a.evaluate(
        ({ t0, cutoff, frameMs }) => ({
          // Decoded pictures captured after the cutoff + one frame interval: must be none.
          late_frames: window.passstream.frameTimeline.filter((f) => f.delay !== null && f.at - f.delay > cutoff + frameMs && f.at < t0 + 2000).map((f) => ({ capture_ms: f.at - f.delay - t0, at_ms: f.at - t0 })),
          // Audio decrypted after the cutoff + one 20 ms frame + 100 ms transit slack: must be none.
          late_audio: window.passstreamViewer.decrypted.filter((d) => d.track === 1 && d.at > cutoff + 120).map((d) => d.at - t0),
          drops: { ...window.passstreamViewer.drops },
          // Through 1.2 s after the kick: past the cutoff and the 850 ms stall, before the viewer's own stall recovery (≈1.4 s) or 2 s missing-key re-admission.
          // Signaling stays open (the server never removed it); its own recovery may replace a PC.
          connected: window.__conn.filter(([at]) => at >= t0 && at <= t0 + 1200).every(([, , ws]) => ws),
          pc_states: [...new Set(window.__conn.filter(([at]) => at >= t0 && at <= t0 + 1200).map(([, pc]) => pc))],
          samples: window.__conn.filter(([at]) => at >= t0 && at <= t0 + 1200).length,
          view: window.passstreamViewer.view.kind,
          events: window.passstreamViewer.events.filter((e) => e.at >= t0 - 200).map((e) => `${e.at - t0}:${e.kind}:${e.detail ?? ""}`),
          sevents: window.passstream.events.filter((e) => e.at >= t0 - 200).map((e) => `${e.at - t0}:${e.kind}:${e.detail ?? ""}`),
          conn: window.__conn.filter(([at]) => at >= t0 && at <= t0 + 1300).map(([at, pc, ws]) => `${at - t0}:${pc}:${ws}`),
        }),
        { t0: kick.t0, cutoff, frameMs },
      );
      // The fixed window above tests confidentiality through the UI stall.
      // Recovery is separate: a remaining viewer may replace its PC and
      // re-admit by grant before a new keyframe decodes. Do not sample it
      // halfway through that asynchronous recovery.
      const recoveryStarted = Date.now();
      await b.waitForFunction((t) => window.passstream.frameTimeline.filter((f) => f.at > t && f.delay !== null && f.delay < 1000).length > 3, kick.unblocked + 300, { timeout: 10000 }).catch(() => {});
      const bFresh = await b.evaluate((t) => window.passstream.frameTimeline.filter((f) => f.at > t && f.delay !== null && f.delay < 1000).length, kick.unblocked + 300);
      const cryptoTrace = await s.page.evaluate(() => window.passstreamSender.dumpTrace());
      await s.page.evaluate(() => window.__source.terminate());
      const undecryptable = (a1.drops.unknown_key ?? 0) - (dropsBefore.unknown_key ?? 0);
      return {
        pass: kick.ok && a1.late_frames.length === 0 && a1.late_audio.length === 0 && undecryptable > 0 && a1.connected && a1.samples >= 10 && bFresh > 3,
        stall_ms: stall,
        kick: { ok: kick.ok, unblocked_after_ms: kick.unblocked - kick.t0, epoch: kick.epoch },
        kicked: { late_frames: a1.late_frames, late_audio_ms: a1.late_audio, undecryptable_frames: undecryptable, stayed_connected: a1.connected, pc_states: a1.pc_states, view: a1.view, ...(a1.connected ? {} : { events: a1.events, session_events: a1.sevents, conn: a1.conn }) },
        remaining_viewer_fresh_frames: bFresh,
        recovery_observation_ms: Date.now() - recoveryStarted,
        ...(bFresh > 3 ? {} : { remaining_viewer_diagnostics: await diag(b, s.page), sender_keys_and_drops: cryptoTrace.filter(f => f[7] || f[8] !== true).map(f => ({ at_ms: f[0] - kick.t0, track: f[2], key: f[7], result: f[8] })) }),
      };
    });
  }

  // Phase 2c review (HIGH): a restarted daemon's command counter starts again;
  // the page answered "pause" = d1 with the cached result of "resume" = d1.
  await test("daemon_restart_command_ids_never_collide", async () => {
    const s = await share(["--pin", "4"]);
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const resume = cli(["resume", "--json"]);
    const idsBefore = await s.page.evaluate(() => [...window.passstreamSender.results.local.keys()]);
    await killDaemon();
    await startDaemon();
    for (let i = 0; i < 100; i++) {
      if (status()?.stream?.attachment === "attached") break;
      await sleep(100);
    }
    const pause = cli(["pause", "--json"]);
    await v.waitForFunction(() => window.passstream.marker === "paused", null, { timeout: 10000 }).catch(() => {});
    const observed = await s.page.evaluate(() => ({
      phase: window.passstreamSender.phase,
      paused: window.passstreamSender.latches.paused,
      captureEnabled: window.passstreamSender.capture.getVideoTracks()[0].enabled,
      ids: [...window.passstreamSender.results.local.keys()],
    }));
    const marker = await v.evaluate(() => window.passstream.marker);
    return {
      pass: resume.code === 0 && pause.code === 0 && observed.paused && !observed.captureEnabled && marker === "paused" && new Set(observed.ids).size === observed.ids.length && idsBefore.every((x) => observed.ids.includes(x)) && observed.ids.length > idsBefore.length,
      ids_before: idsBefore,
      observed,
      viewer_marker: marker,
    };
  });

  await test("correct_pin_decrypts", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    try {
      await firstFrame(v);
    } catch (e) {
      return { error: String(e), diag: await diag(v, s.page) };
    }
    const fresh = await playbackProgress(v, 6);
    const q = await v.evaluate(() => ({ ttff: window.passstream.firstFrame, g2g: window.passstream.quality.g2g, events: window.passstreamViewer.events.map((e) => e.kind + ":" + (e.detail ?? "")), drops: window.passstreamViewer.drops }));
    return { pass: fresh.frames > 5 && q.events.includes("admitted:pin"), ttff_ms: Math.round(q.ttff), g2g_ms: q.g2g, frames_after_admission: fresh.frames, playback_observation_ms: fresh.observation_ms, events: q.events };
  });

  await test("sender_health_owner_only", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const key = readFileSync(join(dataDir, "owner-key"), "utf8").trim();
    let remote, local;
    for (let i = 0; i < 100; i++) {
      remote = await (await fetch(`${origin}/api/v1/streams/${s.id}`, { headers: { Authorization: `Bearer ${key}` } })).json();
      local = status().stream;
      if (remote.effective?.health?.encode_cpu_pct != null && local?.health?.encode_cpu_pct != null) break;
      await sleep(100);
    }
    const health = remote.effective?.health;
    const localHealth = local?.health;
    const cap = new URL(s.link).hash.match(/c=([^&]+)/)[1];
    const denied = await fetch(`${origin}/api/v1/streams/${s.id}`, { headers: { Authorization: `Bearer ${cap}` } });
    const keys = Object.keys(health ?? {}).sort();
    const allowed = ["encode_cpu_pct", "encode_ms", "fps", "height", "quality_limitation", "uplink_kbps", "width"];
    return { pass: health?.encode_cpu_pct != null && localHealth?.encode_cpu_pct != null && health.width > 0 && health.height > 0 && health.fps > 0 && health.uplink_kbps > 0 && denied.status === 401 && keys.every((k) => allowed.includes(k)), health, local_health: localHealth, viewer_status: denied.status };
  });

  await test("wrong_pin_tries_lockout_freeze", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const wrong = s.pin === "0000" ? "1111" : "0000";
    const tries = [];
    let frozenView = null;
    // Four viewer sessions × five wrong PINs: each session backs off after 5,
    // and the stream freezes at 20. From the 5th failure on, the stream-wide
    // pacing (PROTOCOL §6) makes every attempt, from any session, wait
    // 1, 2, 4 … 32, then 60 s, so a fresh session id gains nothing. The test
    // waits out each signed wait in real time (about ten minutes).
    for (let session = 0; session < 4; session++) {
      const ctx = await browser.newContext();
      const v = await viewer(s.link, { pin: wrong, ctx });
      let wrongPins = 0;
      for (let refusals = 1; wrongPins < 5 && refusals <= 12; refusals++) {
        await v.waitForFunction((n) => window.passstreamViewer.events.filter((e) => e.kind === "refused").length >= n, refusals, { timeout: 15000 });
        // locked > 0 (ms left): this refusal set a new wait (lockedUntil isn't cleared when one ends).
        const st = await v.evaluate(() => ({ session: 0, tries: window.passstreamViewer.triesLeft, locked: Math.max(0, (window.passstreamViewer.lockedUntil ?? 0) - Date.now()), last: window.passstreamViewer.events.filter((e) => e.kind === "refused").at(-1)?.detail }));
        st.session = session;
        tries.push(st);
        if (st.last === "wrong_pin" || st.last === "frozen") wrongPins++;
        if (st.last === "frozen" || wrongPins >= 5) break;
        // Wait out the signed wait (the session's backoff or the stream's pacing), then retype.
        if (st.locked) await v.waitForFunction(() => Date.now() >= (window.passstreamViewer.lockedUntil ?? 0) + 50, null, { timeout: 70000, polling: 250 });
        await v.getByTestId("pin-screen").waitFor();
        await v.locator("input").first().fill(wrong);
      }
      await ctx.close();
      if (tries.at(-1)?.last === "frozen") break;
    }
    const v = await viewer(s.link, { pin: s.pin });
    await v.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 });
    frozenView = await view(v);
    let st;
    for (let i = 0; i < 40; i++) {
      st = status();
      if (st.stream.access.failures.stream >= 20) break;
      await sleep(100);
    }
    const failures = tries.filter((t) => t.last === "wrong_pin" || t.last === "frozen");
    const backoffs = new Set(tries.filter((t) => t.locked).map((t) => t.session)).size;
    // Every wrong PIN from the 5th on carries a wait, whichever session sent it.
    const pacedAfterFive = failures.slice(4, -1).every((t) => t.locked);
    // A fresh session id after 5 failures was told to wait too, or arrived after the wait.
    const freshSessionsPaced = tries.filter((t, i) => i > 0 && t.session !== tries[i - 1].session).every((t) => t.last === "backoff" || t.locked);
    // The 20th wrong PIN freezes the stream; the frozen latch then holds the
    // host locked, so even the right PIN is refused (signed "locked").
    return {
      pass: tries.at(-1).last === "frozen" && failures.length === 20 && ["frozen", "locked"].includes(frozenView?.reason) && st.stream.access.latches.frozen === true && st.stream.access.failures.stream === 20 && backoffs >= 3 && tries[0].tries === 4 && pacedAfterFive && freshSessionsPaced,
      tries_left: tries.map((t) => t.tries),
      refusals: tries.map((t) => `${t.session}:${t.last}`),
      backoff_sessions: backoffs,
      paced_after_five: pacedAfterFive,
      fresh_sessions_paced: freshSessionsPaced,
      final: frozenView,
      failures: st.stream.access.failures,
    };
  });

  await test("knock_approve_with_code_via_cli", async () => {
    const s = await share(["--pin", "4", "--knock"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await v.getByTestId("state-knock").waitFor({ timeout: 15000 });
    const code = (await view(v)).code;
    let knock;
    for (let i = 0; i < 40 && !knock; i++) {
      knock = viewersList()?.viewers?.find((r) => r.status === "knocking");
      if (!knock) await sleep(100);
    }
    const wrongCode = cli(["knock", "approve", knock.knock_id, "--code", code === "ACD" ? "ACE" : "ACD", "--json"]);
    const ok = cli(["knock", "approve", knock.knock_id, "--code", code.toLowerCase(), "--json"]);
    await firstFrame(v);
    const fresh = await playbackProgress(v);
    return { pass: ok.code === 0 && wrongCode.code !== 0 && /code_mismatch/.test(wrongCode.stderr + wrongCode.stdout) && fresh.frames > 3 && knock.row_code === code, viewer_code: code, row_code: knock.row_code, wrong_code_error: (wrongCode.stderr || wrongCode.stdout).trim().slice(0, 120), frames_after_approval: fresh.frames, playback_observation_ms: fresh.observation_ms };
  });

  await test("kick_blackens_one_viewer", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const a = await viewer(s.link, { pin: s.pin });
    const b = await viewer(s.link, { pin: s.pin });
    await firstFrame(a);
    await firstFrame(b);
    await sleep(1500);
    const rows = viewersList().viewers.filter((r) => r.status === "watching");
    const aCode = await a.evaluate(() => window.passstreamViewer.rowCode);
    const target = rows.find((r) => r.row_code === aCode);
    const epochBefore = status().stream.e2ee.epoch;
    const t0 = Date.now();
    const k = cli(["kick", target.admission_ids[0], "--json"]);
    await a.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 });
    await sleep(2500);
    const lastA = await a.evaluate(() => window.passstream.frameTimeline.at(-1)?.at ?? 0);
    const bTimeline = await b.evaluate((t0) => window.passstream.frameTimeline.filter((f) => f.at >= t0 - 500).map((f) => f.at), t0);
    const gaps = bTimeline.slice(1).map((x, i) => x - bTimeline[i]);
    const epochAfter = status().stream.e2ee.epoch;
    const freshB = await playbackProgress(b);
    // One disposal for every terminal state: worker gone, keys dropped, revoked grant forgotten.
    const disposed = await a.evaluate(() => {
      const c = window.passstreamViewer;
      const grants = Object.keys(sessionStorage).filter((k) => k.startsWith("ps:viewer:")).map((k) => JSON.parse(sessionStorage.getItem(k))?.grant ?? null);
      return { disposed: c.disposed, worker: !!c.worker, channel: !!c.channel, grants_left: grants.filter(Boolean).length };
    });
    return {
      disposed,
      pass: k.code === 0 && lastA - t0 < 1500 && freshB.frames > 3 && Math.max(0, ...gaps) <= FREEZE_MS && epochAfter > epochBefore && (await view(a)).reason === "kicked" && disposed.disposed && !disposed.worker && !disposed.channel && disposed.grants_left === 0,
      kicked_last_frame_after_ms: lastA - t0,
      other_max_gap_ms: Math.max(0, ...gaps),
      other_frames: freshB.frames, playback_observation_ms: freshB.observation_ms,
      epoch: [epochBefore, epochAfter],
    };
  });

  await test("kick_and_rotate_pin_keeps_old_pin_out", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const a = await viewer(s.link, { pin: s.pin });
    await firstFrame(a);
    await sleep(1000);
    const code = await a.evaluate(() => window.passstreamViewer.rowCode);
    const target = viewersList().viewers.find((r) => r.row_code === code);
    const k = cli(["kick", target.admission_ids[0], "--rotate-pin", "--json"]);
    if (k.code !== 0) throw Error(`kick --rotate-pin failed (exit ${k.code}, code ${k.json?.error?.code ?? "unknown"})`);
    const newPin = cli(["pin", "--reveal", "--json"]).json?.pin;
    secrets.push({ pin: newPin });
    await a.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 }).catch(async error => {
      throw Error(`rotate did not remove target: ${JSON.stringify(await diag(a, s.page))}`, { cause: error });
    });
    // The kicked person reloads and tries the old PIN.
    await a.reload();
    await a.getByTestId("pin-screen").waitFor({ timeout: 10000 });
    await a.locator("input").first().fill(s.pin);
    await a.waitForFunction(() => window.passstreamViewer.events.some((e) => e.kind === "refused"), null, { timeout: 10000 }).catch(async error => {
      throw Error(`old PIN did not reach refusal: ${JSON.stringify(await diag(a, s.page))}`, { cause: error });
    });
    const refused = await a.evaluate(() => window.passstreamViewer.events.filter((e) => e.kind === "refused").map((e) => e.detail));
    const c = await viewer(s.link, { pin: newPin });
    await firstFrame(c);
    return { pass: k.code === 0 && newPin && newPin !== s.pin && refused.includes("wrong_pin"), refusals: refused, pin_changed: newPin !== s.pin };
  });

  await test("pin_hello_waits_for_host_return", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const a = await viewer(s.link);
    await a.getByTestId("pin-screen").waitFor({ timeout: 10000 });
    // Model a HELLO lost at publisher departure, then deliver the normal
    // away/back events through the actual controller. Crypto after return
    // still travels through the real relay and sender.
    await a.evaluate(() => {
      const c = window.passstreamViewer, send = c.session.sendSignal.bind(c.session);
      let hellos = 0;
      window.__offlineHello = { hellos: () => hellos };
      c.session.sendSignal = message => {
        if (message.type === "e2ee") {
          const bytes = atob(message.b.replaceAll("-", "+").replaceAll("_", "/"));
          if (bytes.charCodeAt(1) === 1 && ++hellos === 1) return true;
        }
        return send(message);
      };
    });
    await a.getByTestId("pin-input").fill(s.pin);
    await a.waitForFunction(() => window.__offlineHello.hellos() === 1 && window.passstreamViewer.adm?.state === "hello_sent", null, { timeout: 10000 });
    await a.evaluate(() => window.passstreamViewer.onSignal({ type: "e2ee.sender_away" }));
    await sleep(6000);
    const waiting = await a.evaluate(() => ({
      view: window.passstreamViewer.view.kind,
      retained_pin: window.passstreamViewer.typedPin !== null,
      timeouts: window.passstreamViewer.events.filter(e => e.kind === "admission_timeout").length,
      hellos: window.__offlineHello.hellos(),
    }));
    await a.evaluate(() => {
      const c = window.passstreamViewer;
      c.onSignal({ type: "e2ee.sender_back", instance: c.senderInstance });
    });
    await firstFrame(a);
    const final = await a.evaluate(() => ({ hellos: window.__offlineHello.hellos(), busy: window.passstreamViewer.busy, admitted: window.passstreamViewer.admitted }));
    return { pass: waiting.view === "waiting" && waiting.retained_pin && waiting.timeouts === 0 && waiting.hellos === 1 && final.hellos === 2 && !final.busy && final.admitted, fixture: "dropped HELLO and controller away/back events", offline_observation_ms: 6000, waiting, final };
  });

  for (const approving of [false, true]) await test(`knock_${approving ? "approval" : "pending"}_supersession`, async () => {
    const s = await share(["--pin", "4", "--knock"]);
    secrets.push({ pin: s.pin, link: s.link });
    const a = await viewer(s.link, { pin: s.pin });
    await a.getByTestId("state-knock").waitFor({ timeout: 10000 });
    const code = (await view(a)).code;
    let previous;
    for (let i = 0; i < 40 && !previous; i++) {
      previous = viewersList()?.viewers?.find(r => r.status === "knocking" && r.row_code === code);
      if (!previous) await sleep(100);
    }
    if (!previous) throw Error("initial knock absent from owner state");
    let oldApproval;
    if (approving) {
      await s.page.evaluate(() => {
        const grants = window.passstreamSender.host.grants, issue = grants.issue.bind(grants);
        let release, entered = false;
        const gate = new Promise(resolve => { release = resolve; });
        window.__heldApproval = { entered: () => entered, release: () => release() };
        grants.issue = async (...args) => { grants.issue = issue; entered = true; await gate; return issue(...args); };
      });
      oldApproval = cliAsync(["knock", "approve", previous.knock_id, "--code", code, "--json"]);
      await s.page.waitForFunction(() => window.__heldApproval.entered(), null, { timeout: 10000 });
      await a.evaluate(() => {
        const c = window.passstreamViewer, send = c.session.sendSignal.bind(c.session), held = [];
        let holding = true;
        window.__heldKnockResponse = { count: () => held.length, release: () => { holding = false; for (const m of held) send(m); held.length = 0; } };
        c.session.sendSignal = m => {
          if (holding && m.type === "e2ee") {
            const b = atob(m.b.replaceAll("-", "+").replaceAll("_", "/"));
            if (b.charCodeAt(1) === 3) { held.push(m); return true; }
          }
          return send(m);
        };
      });
    }
    await a.evaluate(() => {
      const c = window.passstreamViewer;
      c.onSignal({ type: "e2ee.sender_away" });
      c.onSignal({ type: "e2ee.sender_back", instance: c.senderInstance });
    });
    await a.getByTestId("pin-input").fill(s.pin);
    let oldExit = null;
    if (approving) {
      await a.waitForFunction(() => window.__heldKnockResponse.count() === 1 && window.passstreamViewer.adm?.state === "response_sent", null, { timeout: 10000 });
      await s.page.evaluate(() => window.__heldApproval.release());
      oldExit = (await oldApproval).code;
      await a.evaluate(() => window.__heldKnockResponse.release());
    }
    await a.getByTestId("state-knock").waitFor({ timeout: 10000 });
    const nextCode = (await view(a)).code;
    let next;
    for (let i = 0; i < 40 && !next; i++) {
      next = viewersList()?.viewers?.find(r => r.status === "knocking" && r.row_code === nextCode && r.knock_id !== previous.knock_id);
      if (!next) await sleep(100);
    }
    if (!next) throw Error("replacement knock absent from owner state");
    const ok = cli(["knock", "approve", next.knock_id, "--code", nextCode, "--json"]);
    if (ok.code !== 0) throw Error(`replacement approval failed (exit ${ok.code})`);
    await firstFrame(a);
    const refusals = await a.evaluate(() => window.passstreamViewer.events.filter(e => e.kind === "refused").map(e => e.detail));
    return { pass: refusals.length === 0 && (!approving || oldExit === 1), held_approval: approving, old_approval_exit: oldExit, replacement_admitted: true, refusals };
  });

  // Hold a real grant RESPONSE across owner cancellation. Only the relay is
  // delayed; the app, CPace transcript, storage and revocation are unchanged.
  for (const action of ["kick_rotate", "rotate"]) for (const drop of [false, true]) {
    await test(`admission_cancel_${action}_${drop ? "lost" : "delivered"}`, async () => {
      const s = await share(["--pin", "4"]);
      secrets.push({ pin: s.pin, link: s.link });
      const a = await viewer(s.link, { pin: s.pin });
      await firstFrame(a);
      await sleep(1000);
      const code = await a.evaluate(() => window.passstreamViewer.rowCode);
      const target = viewersList().viewers.find(r => r.row_code === code).admission_ids[0];
      await a.addInitScript(() => {
        const Native = WebSocket, send = Native.prototype.send, held = [];
        let holding = true, dropNext = false, dropped = 0;
        const wireType = data => {
          try {
            const m = JSON.parse(data);
            if (m.type !== "e2ee" || typeof m.b !== "string") return null;
            const b = atob(m.b.replaceAll("-", "+").replaceAll("_", "/"));
            return b.charCodeAt(0) === 1 ? b.charCodeAt(1) : null;
          } catch { return null; }
        };
        window.__admissionProbe = {
          blocked: () => held.length, dropped: () => dropped,
          dropCancellation: value => { dropNext = value; },
          release: () => { holding = false; for (const [ws, data] of held) send.call(ws, data); held.length = 0; },
        };
        window.WebSocket = new Proxy(Native, { construct(Target, args) {
          const ws = new Target(...args);
          ws.addEventListener("message", ev => {
            if (dropNext && wireType(ev.data) === 5) { dropNext = false; dropped++; ev.stopImmediatePropagation(); }
          }, true);
          return ws;
        } });
        Native.prototype.send = function(data) {
          if (holding && wireType(data) === 3) { held.push([this, data]); return; }
          return send.call(this, data);
        };
      });
      await a.reload();
      await a.waitForFunction(() => window.__admissionProbe?.blocked() === 1 && window.passstreamViewer?.adm?.state === "response_sent", null, { timeout: 10000 });
      await s.page.waitForFunction(id => window.passstreamSender.viewers().some(v => v.admissionId === id && !v.connected), target, { timeout: 10000 });
      await a.evaluate(value => window.__admissionProbe.dropCancellation(value), drop);
      const started = Date.now();
      const result = action === "kick_rotate" ? cli(["kick", target, "--rotate-pin", "--json"]) : cli(["pin", "rotate", "--json"]);
      if (result.code !== 0) throw Error(`owner cancellation failed (exit ${result.code})`);
      const newPin = cli(["pin", "--reveal", "--json"]).json?.pin;
      secrets.push({ pin: newPin });
      if (!newPin || newPin === s.pin) throw Error("new PIN unavailable after successful rotation");
      await a.evaluate(() => window.__admissionProbe.release());
      await a.waitForFunction(() => window.passstreamViewer.view.kind === "pin" && !window.passstreamViewer.busy, null, { timeout: 10000 }).catch(async error => {
        throw Error(`cancelled grant did not return to PIN: ${JSON.stringify(await diag(a, s.page))}`, { cause: error });
      });
      const recovered = await a.evaluate(() => ({
        busy: window.passstreamViewer.busy, admitted: window.passstreamViewer.admitted,
        dropped: window.__admissionProbe.dropped(),
        refusals: window.passstreamViewer.events.filter(e => e.kind === "refused").map(e => e.detail),
        timeouts: window.passstreamViewer.events.filter(e => e.kind === "admission_timeout").length,
        grant_left: Object.keys(sessionStorage).some(k => k.startsWith("ps:viewer:") && JSON.parse(sessionStorage.getItem(k))?.grant),
      }));
      const recoveryMs = Date.now() - started;
      await a.getByTestId("pin-input").fill(s.pin);
      await a.waitForFunction(() => window.passstreamViewer.events.some(e => e.kind === "refused" && e.detail === "wrong_pin"), null, { timeout: 10000 });
      const fresh = await viewer(s.link, { pin: newPin });
      await firstFrame(fresh);
      return { pass: !recovered.busy && !recovered.admitted && !recovered.grant_left && recovered.refusals.includes("grant_invalid") && recovered.dropped === Number(drop) && recovered.timeouts === Number(drop), action, lost_cancellation: drop, recovery_ms: recoveryMs, old_pin_refused: true, new_pin_plays: true, ...recovered };
    });
  }

  await test("lock_refuses_new_viewers", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const a = await viewer(s.link, { pin: s.pin });
    await firstFrame(a);
    const tLock = Date.now();
    const l = cli(["lock", "--json"]);
    const b = await viewer(s.link, { pin: s.pin });
    await b.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 });
    const bView = await view(b);
    const freshA = await playbackProgress(a);
    // The admitted viewer must keep playing through lock and the refused join: no freeze.
    const aTimeline = await a.evaluate((t) => window.passstream.frameTimeline.filter((f) => f.at >= t - 500).map((f) => f.at), tLock);
    aTimeline.push(Date.now());
    const aMaxGap = Math.max(0, ...aTimeline.slice(1).map((x, i) => x - aTimeline[i]));
    const u = cli(["unlock", "--json"]);
    const c = await viewer(s.link, { pin: s.pin });
    await firstFrame(c);
    return { pass: l.code === 0 && u.code === 0 && bView.reason === "locked" && freshA.frames > 3 && aMaxGap <= FREEZE_MS, existing_max_gap_ms: aMaxGap, new_viewer: bView, existing_frames: freshA.frames, playback_observation_ms: freshA.observation_ms };
  });

  await test("pause_draws_paused_no_media_leak", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await sleep(1000);
    const t0 = Date.now();
    const pz = cli(["pause", "--json"]);
    await v.waitForFunction(() => window.passstream.marker === "paused", null, { timeout: 10000 });
    const tPaused = Date.now();
    await sleep(3000);
    // After the switch, every decoded frame is the paused picture: no clock-carrying screen frame.
    const leak = await v.evaluate((t) => window.passstream.frameTimeline.filter((f) => f.at > t + 300 && f.delay !== null).length, tPaused);
    const pausedFrames = await v.evaluate((t) => window.passstream.frameTimeline.filter((f) => f.at > t).length, tPaused);
    const sender = await s.page.evaluate(() => {
      const c = window.passstreamSender;
      const tr = window.passstream.pc.getSenders()[0].track;
      return { phase: c.phase, captureEnabled: c.capture.getVideoTracks()[0].enabled, onWireIsCapture: tr === c.capture.getVideoTracks()[0] };
    });
    const playerState = await v.locator("[data-testid=live-badge]").count();
    // Pause mutes audio: the capture's audio is disabled (tiny silence frames), no tone plays, no "Tap to unmute".
    const toneWhilePaused = await listen(v, 1200);
    const unmuteWhilePaused = await v.getByTestId("unmute").count();
    const audioSender = await s.page.evaluate(async () => {
      const bytes = async () => {
        let n = 0;
        (await window.passstream.pc.getStats()).forEach((r) => {
          if (r.type === "outbound-rtp" && r.kind === "audio") n = r.bytesSent;
        });
        return n;
      };
      const a = await bytes();
      await new Promise((r) => setTimeout(r, 1000));
      return { audioTrackEnabled: window.passstreamSender.audio?.enabled ?? null, audio_bytes_per_s_while_paused: (await bytes()) - a };
    });
    const rs = cli(["resume", "--json"]);
    await v.waitForFunction(() => window.passstream.marker === null, null, { timeout: 10000 });
    const tResumed = Date.now();
    await v.waitForFunction((t) => window.passstreamViewer.decrypted.some((d) => d.track === 1 && d.at > t), tResumed, { timeout: 10000 }).catch(() => {});
    const audioAfter = (await audioReports(v, tResumed)).length;
    const toneAfter = await listen(v, 1200);
    return {
      pass:
        pz.code === 0 && rs.code === 0 && leak === 0 && pausedFrames > 3 && !sender.captureEnabled && !sender.onWireIsCapture && sender.phase === "paused" &&
        toneWhilePaused.tone_samples === 0 && unmuteWhilePaused === 0 && audioSender.audioTrackEnabled === false && audioAfter > 0 && Math.abs(toneAfter.peak_hz - TONE_HZ) < 15,
      switch_ms: tPaused - t0,
      screen_frames_while_paused: leak,
      paused_frames: pausedFrames,
      sender,
      live_badge_while_paused: playerState,
      audio: { tone_while_paused: toneWhilePaused, tone_after_resume: toneAfter, unmute_while_paused: unmuteWhilePaused, sender: audioSender, decrypt_reports_after_resume: audioAfter },
    };
  });

  await test("viewer_reload_readmits_by_grant", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const t0 = Date.now();
    await v.reload();
    await firstFrame(v);
    const ttff = Date.now() - t0;
    const ev = await v.evaluate(() => window.passstreamViewer.events.map((e) => e.kind + ":" + (e.detail ?? "")));
    const pinShown = ev.some((e) => e.startsWith("hello:pin"));
    return { pass: !pinShown && ev.includes("admitted:grant"), reload_to_frame_ms: ttff, events: ev };
  });

  await test("sender_reload_recovers_with_latches", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    cli(["lock", "--json"]);
    const epochBefore = status().stream.e2ee.epoch;
    const t0 = Date.now();
    await s.page.reload();
    await s.page.getByTestId("test-source").click({ timeout: 15000 });
    await s.page.waitForFunction(() => window.passstreamSender?.phase === "live", null, { timeout: 15000 });
    await v.waitForFunction((t) => window.passstream.frameTimeline.some((f) => f.at > t + 500 && f.delay !== null), t0, { timeout: 20000 });
    const recovered = Date.now() - t0;
    const st = status();
    const pin = cli(["pin", "--reveal", "--json"]).json?.pin;
    const n = await viewer(s.link, { pin: s.pin });
    await n.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 });
    const ev = await v.evaluate(() => window.passstreamViewer.events.map((e) => e.kind + ":" + (e.detail ?? "")));
    return {
      pass: st.stream.access.latches.locked && pin === s.pin && (await view(n)).reason === "locked" && ev.filter((e) => e === "admitted:grant").length >= 1,
      viewer_recovered_ms: recovered,
      latches: st.stream.access.latches,
      epoch: [epochBefore, st.stream.e2ee.epoch],
      viewer_events: ev,
    };
  });

  await test("daemon_kill9_recovers", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await killDaemon();
    const during = await freshFrames(v, 2000);
    const t0 = Date.now();
    await startDaemon();
    let st;
    for (let i = 0; i < 100; i++) {
      st = status();
      if (st?.stream?.attachment === "attached" && st.stream.phase === "live" && st.stream.has_link) break;
      await sleep(100);
    }
    const reattached = Date.now() - t0;
    const pin = cli(["pin", "--reveal", "--json"]).json?.pin;
    const afterAt = Date.now();
    // Attachment restores local IPC; frame cadence is controlled independently
    // by the media path. Require four new decoded pictures after attachment,
    // instead of assuming a particular temporal layer within a 1.5 s sample.
    await v.waitForFunction((t) => window.passstream.frameTimeline.filter((f) => f.at > t).length > 3, afterAt, { timeout: 10000 }).catch(() => {});
    await sleep(Math.max(0, afterAt + 1500 - Date.now()));
    const observed = await v.evaluate((t) => ({
      after: window.passstream.frameTimeline.filter((f) => f.at > t).length,
      fixed_sample: window.passstream.frameTimeline.filter((f) => f.at > t && f.at <= t + 1500).length,
      events: window.passstream.events.filter((e) => e.at > t - 5000),
      quality: {fps: window.passstream.quality.fps, layer: window.passstream.quality.layer, g2g: window.passstream.quality.g2g},
    }), afterAt);
    const after = observed.after;
    const attached = st?.stream?.attachment === "attached";
    const pinMatch = typeof s.pin === "string" && pin === s.pin;
    return {
      pass: during > 5 && attached && st?.stream?.phase === "live" && st?.stream?.has_link === true && pinMatch && after > 3,
      frames_while_daemon_down_2s: during, reattach_ms: reattached,
      attachment: st?.stream?.attachment, phase: st?.stream?.phase, has_link: st?.stream?.has_link,
      pin_match: pinMatch, pin_reveal_ok: pin !== undefined,
      frames_after_restart: after, after_observation_ms: Date.now() - afterAt,
      old_1500ms_sample: observed.fixed_sample, session_events: observed.events, media_quality: observed.quality,
      ...(after > 3 ? {} : { diagnostics: await diag(v, s.page) }),
    };
  });

  await test("server_restart_recovers_by_grant", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await sleep(1000);
    await killServer();
    const t0 = Date.now();
    await startServer();
    await v.waitForFunction((t) => window.passstream.frameTimeline.some((f) => f.at > t + 200 && f.delay !== null && f.delay < 1000), t0, { timeout: 30000 });
    const recovered = Date.now() - t0;
    const ev = await v.evaluate(() => window.passstreamViewer.events.map((e) => e.kind + ":" + (e.detail ?? "")));
    return { pass: ev.filter((e) => e === "admitted:grant").length >= 1 && !ev.slice(2).some((e) => e === "hello:pin"), recovery_ms: recovered, events: ev };
  });

  await test("safe_mode_hides_card_from_viewers", async () => {
    // web/src/source.ts sensitiveSource(): the card's digits and a plain line (1280×720 px).
    const CARD = { x: 360, y: 206, w: 330, h: 30 }, PLAIN = { x: 64, y: 306, w: 400, h: 30 };
    const s = await share([], { source: "sensitive", safe: true, tone: 0 });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    // Luma standard deviation of a region of the decoded picture: text is high, a cover is flat.
    const spread = (r) => v.evaluate((r) => {
      const el = document.querySelector("video");
      if (!el?.videoWidth) return -1;
      const c = document.createElement("canvas");
      c.width = el.videoWidth;
      c.height = el.videoHeight;
      const g = c.getContext("2d");
      g.drawImage(el, 0, 0);
      const k = el.videoWidth / 1280;
      const d = g.getImageData(Math.round(r.x * k), Math.round(r.y * k), Math.round(r.w * k), Math.round(r.h * k)).data;
      let n = 0, sum = 0, sq = 0;
      for (let i = 0; i < d.length; i += 4) {
        const l = (d[i] * 77 + d[i + 1] * 150 + d[i + 2] * 29) >> 8;
        n++, (sum += l), (sq += l * l);
      }
      return Math.sqrt(sq / n - (sum / n) ** 2);
    }, r);
    const until = async (cond, ms = 20000) => {
      const end = Date.now() + ms;
      while (Date.now() < end) {
        const x = { card: await spread(CARD), plain: await spread(PLAIN) };
        if (cond(x)) return x;
        await sleep(100);
      }
      return { card: await spread(CARD), plain: await spread(PLAIN), timeout: true };
    };
    // Safe mode on: plain text is revealed once checked, the card stays covered.
    const on = await until((x) => x.plain > 30);
    await v.screenshot({ path: join(out, "safe-mode-viewer-on.png") });
    const reported = status()?.stream?.safe ?? status()?.safe;
    const offCmd = cli(["safe", "off", "--json"]);
    const off = await until((x) => x.card > 30);
    await v.screenshot({ path: join(out, "safe-mode-viewer-off.png") });
    // The cover's soft bar is not flat, but far flatter than readable digits.
    return { pass: !on.timeout && on.card < on.plain * 0.4 && on.plain > 30 && !off.timeout && off.card > 30 && offCmd.code === 0 && reported === true, on, off, reported };
  });

  await test("pin_none_no_pin_screen_still_e2ee", async () => {
    const s = await share(["--pin", "none"]);
    secrets.push({ link: s.link });
    const v = await viewer(s.link, { expectPin: false });
    await firstFrame(v);
    const ev = await v.evaluate(() => window.passstreamViewer.events.map((e) => e.kind + ":" + (e.detail ?? "")));
    const pinScreen = await v.getByTestId("pin-screen").count();
    const eye = await sfuEye(s.link);
    const { raw, ...r } = await eye.result;
    const epochs = await epochSecretsOf(s.page);
    const video = await sframeCheck(raw.video, [0, 2], epochs);
    const audio = await sframeCheck(raw.audio, 1, epochs);
    return {
      pass: pinScreen === 0 && ev.includes("admitted:none") && r.keyframes > 0 && r.vp9_sync_codes === 0 && r.entropy_bits_per_byte > 7.5 && video.pass && audio.pass,
      events: ev,
      sfu_eye: r,
      sframe: { video, audio },
    };
  });

  await test("unsupported_browser_fails_closed", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const ctx = await browser.newContext();
    await ctx.addInitScript(() => {
      delete window.RTCRtpScriptTransform;
    });
    const p = await ctx.newPage();
    let sockets = 0;
    p.on("websocket", () => sockets++);
    await p.goto(s.link);
    await p.getByTestId("state-unsupported").waitFor({ timeout: 10000 });
    await sleep(1500);
    const pcs = await p.evaluate(() => !!window.passstream);
    return { pass: sockets === 0 && !pcs, signaling_sockets: sockets, session_started: pcs };
  });

  await test("vp8_only_viewer_fails_closed", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const ctx = await browser.newContext();
    await ctx.addInitScript(() => {
      const capabilities = RTCRtpReceiver.getCapabilities;
      RTCRtpReceiver.getCapabilities = function(kind) {
        const c = capabilities.call(this, kind);
        return kind === "video" ? { ...c, codecs: c.codecs.filter(codec => codec.mimeType.toLowerCase() === "video/vp8") } : c;
      };
    });
    const v = await viewer(s.link, { pin: s.pin, ctx });
    await v.getByTestId("state-unsupported").waitFor({ timeout: 10000 });
    const detail = await v.getByTestId("state-unsupported").textContent();
    const offers = await v.evaluate(() => window.passstream.events.filter(e => e.kind === "offer").length);
    return { pass: detail.includes("VP9 profile 0") && offers === 0, offers, detail };
  });

  await test("remote_commands_restrictive_only", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await sleep(800);
    const key = readFileSync(join(dataDir, "owner-key"), "utf8").trim();
    const cmd = async (id, op, args = {}) => {
      const r = await fetch(`${origin}/api/v1/streams/${s.id}/commands`, { method: "POST", headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" }, body: JSON.stringify({ id, op, args }) });
      return { status: r.status, body: await r.json().catch(() => null) };
    };
    const pause = await cmd("p1", "pause");
    await v.waitForFunction(() => window.passstream.marker === "paused", null, { timeout: 10000 });
    const resume = await cmd("r1", "resume");
    const unlock = await cmd("u1", "lock", { locked: false });
    const rotate = await cmd("k0", "kick", { admission_id: "A".repeat(22), rotate_pin: true });
    const code = await v.evaluate(() => window.passstreamViewer.rowCode);
    const target = viewersList().viewers.find((r) => r.row_code === code);
    const kick = await cmd("k1", "kick", { admission_id: target.admission_ids[0], lock: true });
    await v.waitForFunction(() => window.passstreamViewer.view.kind === "removed", null, { timeout: 10000 });
    const st = status().stream;
    const local = cli(["resume", "--json"]);
    return {
      pass:
        pause.status === 200 && resume.status === 403 && resume.body?.error?.code === "local_owner_required" && unlock.status === 403 && rotate.status === 403 &&
        kick.status === 200 && kick.body?.result?.locked === true && st.access.latches.locked && st.access.latches.paused && local.code === 0,
      statuses: { pause: pause.status, resume: resume.status, unlock: unlock.status, kick_rotate: rotate.status, kick_lock: kick.status },
      latches: st.access.latches,
      bodies: { resume: resume.body?.error?.code, kick: kick.body },
      local_resume: local.code === 0 ? "ok" : (local.stderr || local.stdout).slice(0, 200),
    };
  });

  await test("long_pin_share_for_leak_scan", async () => {
    // A 12-digit PIN can't match by chance in the leak scan below.
    const s = await share(["--pin", "12"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    await v.reload();
    await firstFrame(v);
    return { pass: s.pin?.length === 12, pin_digits: s.pin?.length };
  });

  await test("low_rendition_decrypts_plays_and_rekeys", async () => {
    const s = await share(["--pin", "4"], { source: "screen" });
    secrets.push({ pin: s.pin, link: s.link });
    const ctx = await browser.newContext();
    await ctx.addInitScript(() => {
      const create = RTCPeerConnection.prototype.createOffer;
      RTCPeerConnection.prototype.createOffer = async function(...args) {
        const offer = await create.apply(this, args);
        offer.sdp = offer.sdp.replace(/^a=extmap:[^\r\n]*transport-wide-cc[^\r\n]*\r?\n/gm, "").replace(/^a=rtcp-fb:[^\r\n]*transport-cc[^\r\n]*\r?\n/gm, "");
        return offer;
      };
    });
    const v = await viewer(s.link, { pin: s.pin, ctx });
    await firstFrame(v);
    await v.waitForFunction(() => document.querySelector("video")?.videoHeight === 180 && window.passstreamViewer.decrypted.some(d => d.track === 2), null, { timeout: 20000 });
    const initial = await frames(v);
    await v.waitForFunction(n => window.passstream.frameTimeline.length >= n + 10, initial, { timeout: 15000 });
    const before = (await frames(v)) - initial;
    const epoch = await s.page.evaluate(() => window.passstreamSender.host.keys.activeEpoch);
    await s.page.evaluate(() => window.passstreamSender.host.keys.rekey("manual"));
    await s.page.waitForFunction(epoch => window.passstreamSender.host.keys.activeEpoch > epoch, epoch, { timeout: 10000 });
    const rekeyed = await frames(v);
    const playingAt = Date.now();
    await v.waitForFunction(n => window.passstream.frameTimeline.length >= n + 10, rekeyed, { timeout: 15000 });
    const after = (await frames(v)) - rekeyed;
    const playbackMs = Date.now() - playingAt;
    const size = await v.evaluate(() => ({ width: document.querySelector("video").videoWidth, height: document.querySelector("video").videoHeight }));
    cli(["pause", "--json"]);
    await v.waitForFunction(() => window.passstream.marker === "paused", null, { timeout: 10000 });
    cli(["resume", "--json"]);
    await v.waitForFunction(() => window.passstream.marker === null, null, { timeout: 10000 });
    return { pass: before >= 10 && after >= 10 && size.width === 320 && size.height === 180, before_rekey_frames: before, after_rekey_frames: after, after_rekey_observation_ms: playbackMs, size, epoch_before: epoch, epoch_after: await s.page.evaluate(() => window.passstreamSender.host.keys.activeEpoch) };
  });

  await test("sfu_sees_only_ciphertext", async () => {
    const s = await share(["--pin", "4"]);
    secrets.push({ pin: s.pin, link: s.link });
    const v = await viewer(s.link, { pin: s.pin });
    await firstFrame(v);
    const eye = await sfuEye(s.link, { waitMs: 15000 });
    const { raw, ...r } = await eye.result;
    const lowEye = await sfuEye(s.link, { twcc: false, waitMs: 10000 });
    const lowResult = await lowEye.result;
    const epochs = await epochSecretsOf(s.page);
    const trackOf = b64 => splitKid(decodeHeader(new Uint8Array(Buffer.from(b64, "base64"))).kid).track;
    const nativeFrames = raw.video.filter(f => trackOf(f) === 0);
    const lowFrames = [...raw.video.filter(f => trackOf(f) === 2), ...lowResult.raw.video.filter(f => trackOf(f) === 2)];
    // Real SFrame parsing: header, KID (epoch×16 + track), CTR order, right key decrypts, wrong key fails.
    const video = await sframeCheck(nativeFrames, 0, epochs);
    // Independent captures can overlap counters; check each peer's ordered frames separately.
    const low = await sframeCheck(lowResult.raw.video.filter(f => trackOf(f) === 2), 2, epochs);
    const otherNativeFrames = lowResult.raw.video.filter(f => trackOf(f) === 0);
    const otherNative = otherNativeFrames.length ? await sframeCheck(otherNativeFrames, 0, epochs) : null;
    const switchedLow = raw.video.filter(f => trackOf(f) === 2);
    const initialLow = switchedLow.length ? await sframeCheck(switchedLow, 2, epochs) : null;
    const knownTracks = [...raw.video, ...lowResult.raw.video].every(f => [0, 2].includes(trackOf(f)));
    const audio = await sframeCheck(raw.audio, 1, epochs);
    // Control: the admitted viewer decodes the same stream (real pictures with a valid clock).
    const decoded = await v.evaluate(() => window.passstream.frameTimeline.filter((f) => f.delay !== null).length);
    const c = r.plaintext_control;
    return {
      pass: r.frames > 20 && r.keyframes > 0 && r.vp9_sync_codes === 0 && r.entropy_bits_per_byte > 7.5 && knownTracks && video.frames >= 20 && low.frames >= 20 && low.pass && (!otherNative || otherNative.pass) && (!initialLow || initialLow.pass) && video.pass && audio.pass && audio.frames >= 20 && decoded > 10 && c.keyframes > 0 && c.vp9_sync_codes === c.keyframes && c.entropy_bits_per_byte < r.entropy_bits_per_byte,
      ...r,
      sframe: { video, low, initial_low: initialLow, other_native: otherNative, audio },
      low_frames_observed: lowFrames.length,
      admitted_viewer_decoded_frames: decoded,
    };
  });
} finally {
  await stopShare().catch(() => {});
  await browser?.close().catch(() => {});
  try {
    await killDaemon();
  } catch {}
  // Kill any wrapper left behind.
  for (const f of readdirSync(launches)) if (f.endsWith(".args")) try { process.kill(Number(f.split(".")[0]), "SIGKILL"); } catch {}
  await killServer().catch(() => {});
  await new Promise((resolve) => fakePass.close(resolve));
}

// ---------------------------------------------------------------- leak scans
function keyForms(what, b64url) {
  const raw = Buffer.from(b64url, "base64url");
  if (raw.length < 16) return [];
  return [
    { what: `${what}_raw`, raw },
    { what: `${what}_hex`, v: raw.toString("hex") },
    { what: `${what}_HEX`, v: raw.toString("hex").toUpperCase() },
    { what: `${what}_b64`, v: raw.toString("base64").replace(/=+$/, "") },
    { what: `${what}_b64url`, v: raw.toString("base64url") },
  ];
}
function scanLeaks() {
  const needles = [];
  for (const n of recordingNeedles) needles.push({ what:"recording_raw",raw:n },{what:"recording_b64",v:n.toString("base64")},{what:"recording_hex",v:n.toString("hex")});
  for (const n of snapshotNeedles) needles.push({ what: "snapshot_raw", raw: n }, { what: "snapshot_b64", v: n.toString("base64").replace(/=+$/, "") }, { what: "snapshot_hex", v: n.toString("hex") });
  needles.push({ what: "pass_service_key", v: fakePassKey });
  const counts = { epoch_secret: 0, identity_seed: 0, grant_secret: 0 };
  for (const s of secrets) {
    for (const what of ["epoch_secret", "identity_seed", "grant_secret"]) {
      if (s[what]) {
        counts[what]++;
        needles.push(...keyForms(what, s[what]));
      }
    }
    if (s.pin && s.pin.length >= 4) needles.push({ what: "pin", v: s.pin });
    if (s.link) {
      const h = new URL(s.link).hash;
      const k = h.match(/k=([^&]+)/)?.[1], sec = h.match(/s=([^&]+)/)?.[1], c = h.match(/c=([^&]+)/)?.[1];
      needles.push({ what: "link_secret", v: sec }, { what: "sender_key", v: k }, { what: "capability_plain", v: c, serverMaySee: true });
      needles.push({ what: "link_secret_hex", v: Buffer.from(sec, "base64url").toString("hex") }, { what: "link_secret_b64", v: Buffer.from(sec, "base64url").toString("base64").replace(/=+$/, "") });
    }
  }
  const files = [logs.server, logs.daemon, ...readdirSync(dataDir).map((f) => join(dataDir, f)), join(env.XDG_STATE_HOME, "passstream/host-state.json")].filter(existsSync);
  const fileHits = [];
  for (const f of files) {
    const b = readFileSync(f);
    const text = b.toString("latin1");
    for (const n of needles) {
      if (n.raw) {
        if (b.includes(n.raw)) fileHits.push({ file: f.replace(base, "$TMP").replace(root, "."), what: n.what });
        continue;
      }
      if (!n.v) continue;
      // Short digit strings occur by chance inside hex hashes, base64 ids and
      // timestamps, so a PIN counts only as a standalone token.
      if (n.what === "pin") {
        const re = new RegExp(`(?<![0-9A-Za-z_:.\\-])${n.v}(?![0-9A-Za-z_:.\\-])`, "g");
        const ctx = [...text.matchAll(re)].slice(0, 3).map((m) => text.slice(Math.max(0, m.index - 16), m.index + n.v.length + 16).replaceAll(n.v, "<PIN>").replace(/[^\x20-\x7e]/g, "."));
        if (ctx.length) fileHits.push({ file: f.replace(base, "$TMP").replace(root, "."), what: n.what, pin_digits: n.v.length, context: ctx });
      } else if (text.includes(n.v)) fileHits.push({ file: f.replace(base, "$TMP").replace(root, "."), what: n.what });
    }
  }
  // Grep the complete process logs with patterns over stdin: no secret in argv
  // or diagnostic output. Short PINs use token boundaries to avoid timestamps.
  const escapeRegex = (v) => v.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const logPatterns = needles.filter((n) => n.v).map((n) => n.what === "pin" ? `(^|[^0-9A-Za-z_:.\\-])${escapeRegex(n.v)}($|[^0-9A-Za-z_:.\\-])` : escapeRegex(n.v));
  const grep = spawnSync("grep", ["-E", "-l", "-f", "-", logs.server, logs.daemon], { input: logPatterns.join("\n") + "\n", encoding: "utf8" });
  const logGrep = { files: 2, patterns: logPatterns.length, hits: grep.stdout.trim().split("\n").filter(Boolean).length, status: grep.status };
  const sentHits = [];
  for (const r of sent) {
    if (!r.url.startsWith(origin) && !r.url.startsWith(origin.replace("http", "ws"))) continue; // loopback daemon traffic is local
    const blob = r.url + r.headers + r.body;
    for (const n of needles) if (n.v && !n.serverMaySee && n.what !== "pin" && blob.includes(n.v)) sentHits.push({ who: r.who, kind: r.kind, what: n.what });
    for (const n of needles) if (n.v && n.what === "pin" && new RegExp(`"${n.v}"`).test(blob)) sentHits.push({ who: r.who, kind: r.kind, what: "pin" });
  }
  return { log_grep: logGrep, files: files.length, needles: needles.length, key_material: counts, requests_to_server: sent.filter((r) => r.url.startsWith(origin) || r.url.startsWith("ws://127.0.0.1:" + httpPort)).length, file_hits: fileHits, sent_hits: sentHits };
}
const leaks = scanLeaks();
const capHits = leaks.file_hits.filter((h) => h.what === "capability_plain");
const realHits = leaks.file_hits.filter((h) => h.what !== "capability_plain");
results.push({ name: "logs_db_free_of_secrets", pass: leaks.log_grep.status === 1 && leaks.log_grep.hits === 0 && realHits.length === 0 && capHits.length === 0 && (filter !== null || (leaks.key_material.epoch_secret > 0 && leaks.key_material.identity_seed > 0 && leaks.key_material.grant_secret > 0)), ...leaks, file_hits: leaks.file_hits });
results.push({ name: "server_never_receives_k_s", pass: leaks.sent_hits.length === 0 && (filter !== null || leaks.key_material.epoch_secret > 0), requests_to_server: leaks.requests_to_server, sent_hits: leaks.sent_hits });
for (const r of results.slice(-2)) console.log(`${r.pass ? "PASS" : "FAIL"} ${r.name} ${JSON.stringify({ ...r, name: undefined })}`);

const binary = createHash("sha256").update(readFileSync(bin)).digest("hex");
const commit = spawnSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).stdout.trim();
const record = { run, created: new Date().toISOString(), commit, binary_sha256: binary, chromium: spawnSync("/usr/bin/chromium", ["--version"], { encoding: "utf8" }).stdout.trim(), results };
mkdirSync("test/torture/results", { recursive: true });
writeFileSync(join(out, "results.json"), JSON.stringify(record, null, 2));
writeFileSync(`test/torture/results/${run}.json`, JSON.stringify(record, null, 2));
console.log("\n| Test | Result |\n|---|---|\n" + results.map((r) => `| ${r.name} | ${r.pass ? "PASS" : "**FAIL**"} |`).join("\n"));
process.exit(results.every((r) => r.pass) ? 0 : 1);
