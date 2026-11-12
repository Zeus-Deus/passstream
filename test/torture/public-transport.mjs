// Run only through TestPublicTransport in its isolated user/network namespace.
// Uses the actual encrypted product, with the explicitly instrumented test source.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { X509Certificate, createHash } from "node:crypto";
import { appendFileSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createInterface } from "node:readline";
import { redactText, redactReport, serializeReport } from "./public-transport-redact.mjs";

async function main() {

assert.equal(process.env.PASSSTREAM_PUBLIC_NETNS, "1", "requires the isolated Go driver");
const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
const run = process.env.TORTURE_RUN ?? "public-transport";
assert.match(run, /^[a-zA-Z0-9_-]+$/);
const out = resolve("test/torture/out", run);
mkdirSync(out, { recursive: true });
const temp = mkdtempSync(join(tmpdir(), "passstream-public-"));
const cert = join(temp, "cert.pem"), key = join(temp, "key.pem");
const openssl = spawnSync("openssl", ["req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-keyout", key, "-out", cert, "-days", "1", "-subj", "/CN=198.18.0.1", "-addext", "subjectAltName=IP:198.18.0.1"], { encoding: "utf8" });
assert.equal(openssl.status, 0, openssl.stderr);
const certificate = new X509Certificate(readFileSync(cert));
const spki = createHash("sha256").update(certificate.publicKey.export({ type: "spki", format: "der" })).digest("base64");
const origin = "https://198.18.0.1:18443";
const server = spawn(resolve(process.env.TORTURE_SERVER_BIN), ["serve", "--data", join(temp, "data"), "--listen", "127.0.0.1:18080", "--base-url", origin, "--public", "--media-ip", "198.18.0.1", "--ice-udp", "0.0.0.0:18843", "--ice-tcp", "0.0.0.0:18843", "--turn", "0.0.0.0:13478", "--turn-tls", "0.0.0.0:15349", "--turn-cert", cert, "--turn-key", key, "--trust-proxy", "127.0.0.1/32"], { stdio: ["ignore", "pipe", "pipe"] });
for (const input of [server.stdout, server.stderr]) {
  createInterface({ input }).on("line", line => appendFileSync(join(out, "server.log"), redactText(line) + "\n"));
}
const sleep = ms => new Promise(r => setTimeout(r, ms));
// A local TLS-terminating proxy, preserving Host and WebSocket upgrades.
const proxy = https.createServer({ cert: readFileSync(cert), key: readFileSync(key) }, (req, res) => {
  const upstream = http.request({ hostname: "127.0.0.1", port: 18080, method: req.method, path: req.url, headers: { ...req.headers, "x-forwarded-for": req.socket.remoteAddress } }, r => { res.writeHead(r.statusCode, r.headers); r.pipe(res); });
  upstream.on("error", () => { res.writeHead(502); res.end(); });
  req.pipe(upstream);
});
proxy.on("upgrade", (req, socket, head) => {
  const upstream = net.connect(18080, "127.0.0.1", () => {
    upstream.write(`${req.method} ${req.url} HTTP/${req.httpVersion}\r\n${req.rawHeaders.reduce((s, v, i) => s + v + (i % 2 ? "\r\n" : ": "), "")}X-Forwarded-For: ${socket.remoteAddress}\r\n\r\n`);
    if (head.length) upstream.write(head);
    socket.pipe(upstream).pipe(socket);
  });
  upstream.on("error", () => socket.destroy());
  socket.on("error", () => upstream.destroy());
  socket.on("close", () => upstream.destroy());
});
const routes = process.env.PUBLIC_TRANSPORT_ROUTE ? [process.env.PUBLIC_TRANSPORT_ROUTE] : ["udp", "tcp", "turn_udp", "turn_tcp", "turn_tls"];
for (const route of routes) assert.ok(["udp", "tcp", "turn_udp", "turn_tcp", "turn_tls"].includes(route));
const rtt = Number(process.env.PUBLIC_TRANSPORT_RTT ?? 0);
assert.ok(rtt === 0 || rtt === 800);
if (rtt) {
  for (const args of [
    ["qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio"],
    ["qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "delay", "400ms"],
    ["filter", "add", "dev", "lo", "parent", "1:", "protocol", "ip", "prio", "1", "u32", "match", "ip", "dst", "198.18.0.1/32", "flowid", "1:3"],
    ["filter", "add", "dev", "lo", "parent", "1:", "protocol", "all", "prio", "99", "u32", "match", "u32", "0", "0", "flowid", "1:1"],
  ]) { const result = spawnSync("tc", args, { encoding: "utf8" }); assert.equal(result.status, 0, result.stderr); }
}
const results = [];
let browser;
let failure;
try {
  for (let i = 0; i < 200; i++) {
    try { if ((await fetch("http://127.0.0.1:18080/readyz")).ok) break; } catch {}
    if (server.exitCode !== null) throw Error("server exited: " + readFileSync(join(out, "server.log"), "utf8"));
    await sleep(25);
  }
  await new Promise(r => proxy.listen(18443, "198.18.0.1", r));
  browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true, args: ["--no-sandbox", "--no-proxy-server", "--autoplay-policy=no-user-gesture-required", `--ignore-certificate-errors-spki-list=${spki}`] });
  for (const route of routes) {
    const context = await browser.newContext();
    const pages = [];
    const errors = [];
    try {
      // Choose one actual route for both peers; direct TCP is forced by
      // dropping just the advertised SFU's UDP traffic inside this namespace.
      if (route === "tcp") {
        const tc = spawnSync("tc", ["qdisc", "add", "dev", "lo", "clsact"], { encoding: "utf8" });
        assert.equal(tc.status, 0, tc.stderr);
        const filter = spawnSync("tc", ["filter", "add", "dev", "lo", "egress", "protocol", "ip", "flower", "ip_proto", "udp", "dst_ip", "198.18.0.1", "dst_port", "18843", "action", "drop"], { encoding: "utf8" });
        assert.equal(filter.status, 0, filter.stderr);
      }
      await context.addInitScript(route => {
        const Native = RTCPeerConnection;
        window.RTCPeerConnection = class extends Native {
          constructor(config) {
            const relay = route.startsWith("turn_");
            const urls = url => route === "turn_tls" ? url.startsWith("turns:") : route === "turn_tcp" ? url.startsWith("turn:") && url.endsWith("transport=tcp") : url.endsWith("transport=udp");
            super({ ...config, iceTransportPolicy: relay ? "relay" : "all", iceServers: relay ? config.iceServers.map(s => ({ ...s, urls: (Array.isArray(s.urls) ? s.urls : [s.urls]).filter(urls) })) : [] });
          }
        };
      }, route);
      const stream = await new Promise((resolve, reject) => {
        const request = https.request(origin + "/api/v1/streams", { method: "POST", ca: readFileSync(cert), headers: { Authorization: "Bearer " + readFileSync(join(temp, "data/owner-key"), "utf8").trim(), "Content-Type": "application/json" } }, response => {
          let body = "";
          response.on("data", data => body += data);
          response.on("end", () => {
            try { assert.equal(response.statusCode, 201); resolve(JSON.parse(body)); } catch (error) { reject(error); }
          });
        });
        request.on("error", reject);
        request.end("{}");
      });
      assert.ok(stream.host_url.startsWith(origin + "/host#"));
      const publisher = await context.newPage();
      pages.push(publisher);
      publisher.on("pageerror", e => errors.push(redactText(e)));
      await publisher.goto(stream.host_url.replace("/host#", "/host?test=1&safe=0#"));
      await publisher.getByTestId("test-source").click();
      await publisher.getByTestId("reveal").click();
      await publisher.getByTestId("reveal-confirm").click();
      const pin = (await publisher.getByTestId("pin").textContent()).trim();
      const link = await publisher.evaluate(() => window.passstreamSender.link);
      const viewer = await context.newPage();
      pages.push(viewer);
      viewer.on("pageerror", e => errors.push(redactText(e)));
      await viewer.goto(link);
      await viewer.getByTestId("pin-screen").waitFor({ timeout: 60000 });
      await viewer.locator("input").first().fill(pin);
      await viewer.waitForFunction(() => window.passstream?.firstFrame > 0, null, { timeout: 60000 });
      await viewer.waitForFunction(() => window.passstream?.frameTimeline.length >= 10, null, { timeout: 30000 });
      await sleep(3000);
      const evidence = await viewer.evaluate(async () => {
        const session = window.passstream;
        const stats = await session.pc.getStats();
        const transport = [...stats.values()].find(s => s.type === "transport" && s.selectedCandidatePairId);
        const pair = stats.get(transport?.selectedCandidatePairId);
        const local = stats.get(pair?.localCandidateId), remote = stats.get(pair?.remoteCandidateId);
        return { rtt_ms: pair?.currentRoundTripTime == null ? null : pair.currentRoundTripTime * 1000, frames: session.frameTimeline.length, ttff_ms: session.firstFrame, secure_context: isSecureContext, view: window.passstreamViewer.view.kind, local: local && { type: local.candidateType, protocol: local.protocol, relay_protocol: local.relayProtocol, url: local.url }, remote: remote && { address: remote.address, protocol: remote.protocol }, decrypt_drops: window.passstreamViewer.drops };
      });
      assert.equal(evidence.secure_context, true);
      assert.ok(evidence.frames >= 10, JSON.stringify(evidence));
      assert.equal(evidence.remote.address, "198.18.0.1");
      if (route.startsWith("turn_")) {
        assert.equal(evidence.local.type, "relay");
        assert.equal(evidence.local.relay_protocol, route.slice(5));
      } else assert.equal(evidence.remote.protocol, route);
      assert.deepEqual(errors, []);
      results.push({ route, pass: true, ...evidence });
      console.log(`${route}: ${evidence.frames} decrypted frames, TTFF ${evidence.ttff_ms.toFixed(1)}ms, ${evidence.local.type}/${evidence.local.protocol}/${evidence.local.relay_protocol ?? "direct"}`);
    } catch (e) {
      const diagnostics = await Promise.all(pages.map(p => p.evaluate(async () => {
        const session = window.passstream;
        return { state: session?.state, ice: session?.pc?.iceConnectionState, events: session?.events, candidates: session?.pc && [...(await session.pc.getStats()).values()].filter(s => s.type.includes("candidate")) };
      }).catch(error => ({ error: redactText(error) }))));
      results.push(redactReport({ route, pass: false, error: redactText(e), page_errors: errors, diagnostics }));
      throw e;
    } finally {
      await context.close();
      if (route === "tcp") spawnSync("tc", ["qdisc", "del", "dev", "lo", "clsact"]);
    }
  }
} catch (e) { failure = redactText(e); console.error(failure); }
finally {
  await browser?.close();
  proxy.closeAllConnections();
  await new Promise(r => proxy.close(r));
  server.kill("SIGTERM");
  await new Promise(r => server.once("exit", r));
  const result = { run, configured_rtt_ms: rtt, binary_sha256: createHash("sha256").update(readFileSync(process.env.TORTURE_SERVER_BIN)).digest("hex"), chromium: spawnSync("/usr/bin/chromium", ["--version"], { encoding: "utf8" }).stdout.trim(), date: new Date().toISOString(), scope: "isolated public-like IPv4 route smoke, real encrypted product, one publisher/viewer, synthetic test source; not a WAN/mobile/congestion gate", certificate: "temporary test certificate pinned by Chromium SPKI; no global certificate bypass", bind: "0.0.0.0", advertise: "198.18.0.1", proxy: "HTTPS with WebSocket upgrades to loopback HTTP", results, error: failure ?? null, pass: !failure && results.length === routes.length };
  mkdirSync(resolve("test/torture/results/transport"), { recursive: true });
  writeFileSync(resolve("test/torture/results/transport", run + ".json"), serializeReport(result));
}
if (failure) process.exitCode = 1;

}
// Also sanitize failures in setup/cleanup/report writing outside the route catch.
main().catch(error => { console.error(redactText(error?.stack ?? error)); process.exitCode = 1; });
