import { pct, summarize, freshTimeline, bandwidthMetrics, bandwidthPass, meanBitrate, mediaFloorPass, nativeMetrics, nativePass, nativeHoldPass, paddingPass, nativeCadencePass } from "./torture-metrics.mjs";
import { cryptoSummary, gapForensics } from "./torture-crypto.mjs";
import { chromium } from "@playwright/test";
import { spawn, spawnSync } from "node:child_process";
import {
  readFileSync,
  writeFileSync,
  mkdirSync,
  appendFileSync,
} from "node:fs";
import { resolve } from "node:path";
import { createHash } from "node:crypto";
import { createServer, request } from "node:http";
import { createSocket } from "node:dgram";
const root = process.cwd(),
  run =
    process.env.TORTURE_RUN ?? new Date().toISOString().replaceAll(":", "-"),
  out = resolve("test/torture/out", run);
mkdirSync(out, { recursive: true });
mkdirSync("test/torture/results", { recursive: true });
const duration = Number(process.env.TORTURE_SECONDS ?? 12),
  filter = process.env.TORTURE_FILTER,
  stressMax = Number(process.env.TORTURE_STRESS_MAX ?? 64);
const bin = process.env.TORTURE_SERVER_BIN ?? "./build/passstream";
// Audio A/B: "none" (production default without captured audio: no audio
// m-line on the publisher; the viewer's is omitted with ?noaudio), "mline"
// (the silent transceivers of fix round 1 on both PCs, no audio sent) or
// "tone" (a 440 Hz test tone, audio sent). Unset: production default.
const audioMode = process.env.TORTURE_AUDIO ?? "";
// netem seed for the bad-Wi-Fi loss/jitter PRNG, matched across an A/B pair.
const netemSeed = process.env.TORTURE_NETEM_SEED ?? "";
const harnessHashes = Object.fromEntries(["web/torture.mjs", "web/torture-metrics.mjs"].map(path => [path, createHash("sha256").update(readFileSync(path)).digest("hex")]));
const origin = "http://127.0.0.1:18080";
let server,
  browser,
  log,
  resources = [],
  resourceTimer,
  errors = [],
  dataDir,
  bulk;
// Browser exceptions include navigation URLs; never retain capability fragments.
const redact = (s) => s.replace(/((?:https?|wss?):\/\/[^\s"'<>#]+)#[^\s"'<>]+/g, "$1#[redacted]");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const cmd = (name, args) => {
  const p = spawnSync(name, args, { encoding: "utf8" });
  if (p.status !== 0)
    throw Error(`${name} ${args.join(" ")}: ${p.stderr || p.stdout}`);
  return p.stdout;
};
// Keep TCP packets at the namespace's MTU before applying narrow-link queues.
const offloads = JSON.parse(cmd("python3", ["test/torture/offloads.py"]));
function tc(args) {
  return cmd("tc", args);
}
// A timestamped datagram measures residence in the same FIFO as TCP and RTP.
// It is received locally after one qdisc traversal, so no clock sync or echo
// assumption is involved. At two 64-byte payloads/s its IP rate is 1.5 kbit/s.
async function queueProbe() {
  const socket=createSocket("udp4"), samples=[];
  socket.on("message", b=>samples.push({sent_at:Number(b.readBigInt64BE()),at:Date.now(),delay_ms:Date.now()-Number(b.readBigInt64BE())}));
  await new Promise(r=>socket.bind(18083,"127.0.0.1",r));
  tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","1","u32","match","ip","protocol","17","0xff","match","ip","dport","18083","0xffff","flowid","1:3"]);
  const timer=setInterval(()=>{const b=Buffer.alloc(64);b.writeBigInt64BE(BigInt(Date.now()));socket.send(b,18083,"127.0.0.1")},500);
  return {samples,stop(){clearInterval(timer);socket.close()},metrics(from,to){
    const observed=samples.filter(s=>s.at>=from&&s.at<=to).map(s=>s.delay_ms);
    return {samples:observed.length,p50_ms:pct(observed,.5),p95_ms:pct(observed,.95),max_ms:observed.length?Math.max(...observed):null};
  }};
}
function clear() {
  spawnSync("tc", ["qdisc", "del", "dev", "lo", "root"]);
}
function netem(spec = "") {
  clear();
  if (spec)
    tc(["qdisc", "add", "dev", "lo", "root", "netem", ...spec.split(" ")]);
}
function shapedRoot() {
  tc(["qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio"]);
  // All unmatched traffic must bypass the impaired band, regardless of DSCP.
  tc([
    "filter",
    "add",
    "dev",
    "lo",
    "parent",
    "1:",
    "protocol",
    "all",
    "prio",
    "99",
    "u32",
    "match",
    "u32",
    "0",
    "0",
    "flowid",
    "1:1",
  ]);
}
function udpBlackhole() {
  clear();
  shapedRoot();
  for (const direction of ["src", "dst"])
    tc([
      "filter",
      "add",
      "dev",
      "lo",
      "parent",
      "1:",
      "protocol",
      "ip",
      "prio",
      "1",
      "u32",
      "match",
      "ip",
      direction,
      "127.0.0.2/32",
      "flowid",
      "1:1",
    ]);
  tc([
    "filter",
    "add",
    "dev",
    "lo",
    "parent",
    "1:",
    "protocol",
    "ip",
    "prio",
    "2",
    "u32",
    "match",
    "ip",
    "protocol",
    "17",
    "0xff",
    "flowid",
    "1:3",
  ]);
  tc([
    "qdisc",
    "add",
    "dev",
    "lo",
    "parent",
    "1:3",
    "handle",
    "30:",
    "netem",
    "loss",
    "100%",
  ]);
}
function sampleResource() {
  const sample = { at: Date.now(), load: Number(readFileSync("/proc/loadavg", "utf8").split(" ")[0]) };
  try {
    const status = readFileSync(`/proc/${server.pid}/status`, "utf8");
    const stat = readFileSync(`/proc/${server.pid}/stat`, "utf8").split(" ");
    Object.assign(sample, {
      rssKiB: Number(status.match(/VmRSS:\s+(\d+)/)?.[1] ?? 0),
      ticks: Number(stat[13]) + Number(stat[14]),
    });
  } catch {}
  resources.push(sample);
}
async function launchServer() {
  log = spawn(
    bin,
    [
      "serve",
      "--data",
      dataDir,
      "--listen",
      "127.0.0.1:18080",
      "--base-url",
      origin,
      "--ice-udp",
      "127.0.0.1:18443",
      "--ice-tcp",
      "127.0.0.1:18443",
      "--turn",
      "127.0.0.1:13478",
    ],
    { cwd: root, stdio: ["ignore", "pipe", "pipe"] },
  );
  server = log;
  log.stdout.on("data", (b) => appendFileSync(`${out}/server.log`, b));
  log.stderr.on("data", (b) => appendFileSync(`${out}/server.log`, b));
  for (let i = 0; i < 200; i++) {
    try {
      if ((await fetch(origin + "/readyz")).ok) return;
    } catch {}
    await sleep(25);
  }
  throw Error("server startup failed");
}
async function createStream(body = {}) {
  const key = readFileSync(`${dataDir}/owner-key`, "utf8").trim();
  const r = await fetch(origin + "/api/v1/streams", {
    method: "POST",
    headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) throw Error(`create ${r.status}`);
  return r.json();
}
// The harness reads the PIN the way an owner does: reveal it in the sender window.
async function readPin(p) {
  await p.getByTestId("reveal").click();
  await p.getByTestId("reveal-confirm").click();
  return (await p.getByTestId("pin").textContent()).trim();
}
async function page() {
  const p = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  // Root-netem profiles also impair the page's TCP fetch before media starts.
  p.setDefaultNavigationTimeout(180000);
  p.on("pageerror", (e) => errors.push(redact(String(e))));
  p.on("console", (m) => {
    if (m.type() === "error") errors.push(redact(m.text()));
  });
  return p;
}
const sourceQualifications = new WeakMap();
async function publisher(v, source = process.env.TORTURE_SOURCE ?? "clock", qualify = true) {
  const p = await page();
  // The server-served sender: test source, sender-generated PIN, E2EE on.
  const audioQuery = audioMode === "mline" ? "&audio_mline=1" : audioMode === "tone" ? "&tone=440" : "";
  await p.goto(v.host_url.replace("/host#", `/host?test=1&safe=0&source=${source}${process.env.TORTURE_TCP ? "&transport=tcp" : ""}${audioQuery}#`));
  await p.getByTestId("test-source").click();
  if (!v.pin_none) v.pin = await readPin(p);
  v.link = await p.evaluate(() => window.passstreamSender.link);
  try {
    await p.waitForFunction(() => window.passstream?.state === "live", null, {
      timeout: 20000,
    });
  } catch (e) {
    console.log(
      "publisher debug",
      await p.evaluate(async () => ({
        state: window.passstream?.state,
        ice: window.passstream?.pc?.iceConnectionState,
        stats: [...(await window.passstream.pc.getStats()).values()].filter(
          (s) => s.type.includes("candidate"),
        ),
      })),
    );
    throw e;
  }
  if (qualify) await qualifySource(p,source);
  return p;
}
async function qualifySource(p, source, since = -Infinity) {
  if (source === "screen") {
    try { await p.waitForFunction(since => {
      const samples=window.passstream.samples.filter(s=>s.timestamp>=since).slice(-4);
      return samples.length===4 && samples.every(s=>s.fps>=12&&s.bitrate>=1_500_000&&s.bitrate<=2_500_000);
    }, since, { timeout: 60000 }); }
    catch (error) {
      error.qualification = await snapshot(p);
      error.qualificationPeers = await Promise.all(browser.contexts().flatMap(c=>c.pages()).filter(page=>page!==p).map(page=>snapshot(page).catch(()=>null)));
      throw error;
    }
    sourceQualifications.set(p, await p.evaluate(() => ({
      at: Date.now(),
      elapsed_ms: Date.now() - (performance.timeOrigin + window.passstream.started),
      samples: window.passstream.samples.slice(-4).map(s => ({at:s.timestamp,fps:s.fps,bitrate:s.bitrate})),
    })));
  }
}
function sourceWorkload(source, raw, from, to, qualification) {
  if (source !== "screen") return undefined;
  const samples=raw.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to);
  const bitrate=pct(samples.map(s=>s.bitrate),.5),fps=pct(samples.map(s=>s.fps),.5);
  return {qualification,offered_bps:bitrate,encoded_fps:fps,pass:!!qualification&&bitrate>=1_500_000&&bitrate<=2_500_000};
}
async function viewer(v, timeout = 30000) {
  const p = await page();
  if (process.env.TORTURE_TRACE_RVFC === "1") await p.addInitScript(() => {
    window.passstreamFrameMetadata = [];
    const request = HTMLVideoElement.prototype.requestVideoFrameCallback;
    HTMLVideoElement.prototype.requestVideoFrameCallback = function(callback) {
      return request.call(this, (now, metadata) => {
        callback(now, metadata);
        window.passstreamFrameMetadata.push({at:Date.now(),now,presentedFrames:metadata.presentedFrames,rtpTimestamp:metadata.rtpTimestamp,receiveTime:metadata.receiveTime,captureTime:metadata.captureTime,freshCount:window.passstream?.freshness?.count});
        if (window.passstreamFrameMetadata.length > 1000) window.passstreamFrameMetadata.shift();
      });
    };
  });
  if (process.env.TORTURE_RELAY_UDP === "1") await p.addInitScript(() => {
    const PeerConnection = RTCPeerConnection;
    window.RTCPeerConnection = class extends PeerConnection {
      constructor(config) {
        super({...config,iceTransportPolicy:"relay",iceServers:config.iceServers?.map(server=>({...server,urls:(Array.isArray(server.urls)?server.urls:[server.urls]).filter(url=>url.includes("transport=udp"))}))});
      }
    };
  });
  const query = [process.env.TORTURE_TCP ? "transport=tcp" : "", audioMode === "none" ? "noaudio=1" : ""].filter(Boolean).join("&");
  await p.goto(query ? v.link.replace("/v#", `/v?${query}#`) : v.link);
  try {
    if (v.pin) {
      await p.getByTestId("pin-screen").waitFor({ timeout });
      await p.locator("input").first().fill(v.pin);
    }
    await p.waitForFunction(() => window.passstream?.firstFrame > 0, null, {
      timeout,
    });
  } catch {}
  return p;
}
async function snapshot(p) {
  return p.evaluate(async () => {
    const s = window.passstream;
    return {
      capturedAt: Date.now(),
      startedAt: performance.timeOrigin + s.started,
      videoSize: {width:document.querySelector("video")?.videoWidth,height:document.querySelector("video")?.videoHeight},
      ttff: s.firstFrame,
      quality: s.quality,
      decoderFreshFrames: s.freshness?.count,
      frameMetadata: window.passstreamFrameMetadata,
      receiverStats: window.passstreamFrameMetadata && s.pc ? [...(await s.pc.getStats()).values()].filter(v=>v.type==="inbound-rtp") : undefined,
      senderStats: s.role === "publish" && s.pc ? [...(await s.pc.getStats()).values()].filter(v=>v.type==="outbound-rtp") : undefined,
      samples: s.samples,
      timeline: s.frameTimeline,
      states: s.states,
      events: s.events,
      replacements: s.replacements,
      codecMap: s.pc?.localDescription?.sdp.split("\n").filter(l=>/^a=(rtpmap|fmtp|rtcp-fb|extmap):/.test(l)),
      remoteCodecMap: s.pc?.remoteDescription?.sdp.split("\n").filter(l=>/^a=(rtpmap|fmtp|rtcp-fb|extmap):/.test(l)),
    };
  });
}
// Per-SSRC RTP feedback from the page's own stats (both kinds, both directions).
async function rtpFeedback(p) {
 return p.evaluate(async()=>{ const out=[]; const pc=window.passstream?.pc; if(!pc) return out;
  const sdp=pc.localDescription?.sdp??"";
  (await pc.getStats()).forEach(r=>{ if(r.type!=="inbound-rtp"&&r.type!=="outbound-rtp") return;
   out.push({type:r.type,kind:r.kind,ssrc:r.ssrc,packets:r.packetsReceived??r.packetsSent,bytes:r.bytesReceived??r.bytesSent,lost:r.packetsLost,nack:r.nackCount,pli:r.pliCount,fir:r.firCount,retransmitted:r.retransmittedPacketsSent,fec_received:r.fecPacketsReceived,frames:r.framesDecoded??r.framesEncoded,freezes:r.freezeCount,concealed:r.concealedSamples});
  });
  return {m_lines:{audio:(sdp.match(/^m=audio/gm)??[]).length,video:(sdp.match(/^m=video/gm)??[]).length},ssrcs:out};
 });
}
async function selectedPort(p) {
 return p.evaluate(async()=>{ const stats=await window.passstream.pc.getStats();
 for(const s of stats.values()) if(s.type==="transport" && s.selectedCandidatePairId) return stats.get(stats.get(s.selectedCandidatePairId)?.localCandidateId)?.port;
 });
}
// Follow every local candidate, including replacement PCs and relay paths.
// Filters accumulate for the scenario; a replacement cannot escape impairment.
async function impairViewers(pages, spec, directions=["dport"]) {
 clear(); shapedRoot();
 const ports=new Set(), paths=[], counters=[], selected=new Map();
 let busy=false;
 async function refresh() {
  if (busy) return;
  busy=true;
  try {
   for (const [viewer,page] of pages.entries()) {
    const observation=await page.evaluate(async()=>{
     const pc=window.passstream?.pc; if (!pc || pc.connectionState==="closed") return {candidates:[]};
     const stats=await pc.getStats(),values=[...stats.values()];
     const transport=values.find(s=>s.type==="transport" && s.selectedCandidatePairId);
     const pair=stats.get(transport?.selectedCandidatePairId),local=stats.get(pair?.localCandidateId);
     return {candidates:values.filter(s=>s.type==="local-candidate" && s.port).map(s=>({port:s.port,protocol:s.protocol})),path:local?{port:local.port,protocol:local.protocol,pair:pair.id}:null};
    });
    for (const c of observation.candidates) {
     const key=`${c.protocol}/${c.port}`;
     if (ports.has(key)) continue;
     ports.add(key);
     for (const direction of directions) tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","1","u32","match","ip","protocol",c.protocol==="tcp"?"6":"17","0xff","match","ip",direction,String(c.port),"0xffff","flowid","1:3"]);
    }
    if(observation.path && selected.get(viewer)!==observation.path.pair) {
     const path=observation.path;
     if(!ports.has(`${path.protocol}/${path.port}`)) throw Error("selected path has no impairment filter");
     selected.set(viewer,path.pair);paths.push({at:Date.now(),viewer,...path});
    }
   }
  } finally { busy=false }
 }
 await refresh();
 tc(["qdisc","add","dev","lo","parent","1:3","handle","30:","netem",...spec.split(" ")]);
 const timer=setInterval(()=>refresh().catch(e=>errors.push(redact(String(e)))),50);
 const countTimer=setInterval(()=>counters.push({at:Date.now(),raw:cmd("tc",["-s","-j","qdisc","show","dev","lo"])}),500);
 return {
  change(spec) { tc(["qdisc","change","dev","lo","parent","1:3","handle","30:","netem",...spec.split(" ")]) },
  stop() {
   clearInterval(timer); clearInterval(countTimer);
   // Verify traffic still traverses the impairment after every path change.
   const counts=counters.map(c=>({at:c.at,bytes:JSON.parse(c.raw).find(q=>q.handle==="30:")?.bytes??0}));
   const verified=paths.every(p=>{
    const after=counts.filter(c=>c.at>=p.at && c.at<=p.at+4000);
    return after.length>=2 && after.at(-1).bytes>after[0].bytes;
   });
   return {paths,filters:[...ports],refresh_ms:50,counters:counts,verified};
  }
 };
}
// Apply the SFU's outgoing leg before ICE starts, without putting publisher
// ingress and viewer egress into one artificial shared rate-limited queue.
async function impairBeforeJoin(publisherPage,spec) {
 clear();shapedRoot();
 tc(["qdisc","add","dev","lo","parent","1:3","handle","30:","netem",...spec.split(" ")]);
 const publisherPorts=await publisherPage.evaluate(async()=>[...(await window.passstream.pc.getStats()).values()].filter(s=>s.type==="local-candidate"&&s.port).map(s=>s.port));
 // Exempt the already established publisher and HTTP application traffic.
 // Everything else, including viewer feedback and TURN replacements, crosses
 // the impaired band from the first ICE packet onwards.
 for(const port of [...new Set([...publisherPorts,18080])]) for(const direction of ["sport","dport"])
  tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","1","u32","match","ip",direction,String(port),"0xffff","flowid","1:1"]);
 tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","2","u32","match","ip","dst","127.0.0.1/32","flowid","1:3"]);
 const counters=[];const timer=setInterval(()=>counters.push({at:Date.now(),bytes:JSON.parse(cmd("tc",["-s","-j","qdisc","show","dev","lo"])).find(q=>q.handle==="30:")?.bytes??0}),500);
 return {change(spec){tc(["qdisc","change","dev","lo","parent","1:3","handle","30:","netem",...spec.split(" ")])},stop(){clearInterval(timer);return {before_join:true,filters:["all viewer traffic, publisher and HTTP exempt"],counters,verified:counters.length>1&&counters.at(-1).bytes>counters[0].bytes}}};
}
function configureAQM(rate,delay=0) {
 tc(["qdisc","del","dev","lo","parent","1:3","handle","30:"]);
 tc(["qdisc","add","dev","lo","parent","1:3","handle","30:","htb","default","1"]);
 tc(["class","add","dev","lo","parent","30:","classid","30:1","htb","rate",rate,"ceil",rate,"burst","1500","cburst","1500"]);
 if(delay) tc(["qdisc","add","dev","lo","parent","30:1","handle","40:","netem","delay",`${delay}ms`]);
 tc(["qdisc","add","dev","lo","parent",delay?"40:1":"30:1","handle","50:","fq_codel","limit","1000","target","5ms","interval","100ms"]);
}
const results = [], loadRejected = [];
async function scenario(name, fn) {
  if (filter && !new RegExp(filter).test(name)) return;
  const attempts = Number(process.env.TORTURE_LOAD_ATTEMPTS ?? 5);
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const result = await scenarioAttempt(name, fn, attempt);
    if (!result.load_invalid) {
      results.push(result);
      if (!result.pass && process.env.TORTURE_STOP_ON_FAILURE === "1")
        throw Error(`quality gate failed for ${name}; this pin stopped with its result retained`);
      return;
    }
    // Retry only a predeclared host-load violation, never a quality failure.
    loadRejected.push(result);
  }
  throw Error(`host load invalidated ${attempts} attempts of ${name}; all retained`);
}
async function scenarioAttempt(name, fn, attempt) {
  const loadWaitStarted=Date.now(), loadLimit=Number(process.env.TORTURE_MAX_LOAD ?? 0);
  const loadDeadline=loadWaitStarted+Number(process.env.TORTURE_LOAD_WAIT_MS ?? 30000);
  while(loadLimit>0 && Number(readFileSync("/proc/loadavg","utf8").split(" ")[0])>loadLimit && Date.now()<loadDeadline) await sleep(1000);
  const loadWaitMs=Date.now()-loadWaitStarted;
  if(process.env.TORTURE_REQUIRE_LOW_LOAD==="1" && loadLimit>0 && Number(readFileSync("/proc/loadavg","utf8").split(" ")[0])>loadLimit) throw Error(`low-load entry condition was not met for ${name}`);
  clear();
  errors = [];
  resources = [];
  dataDir = `${out}/data-${name}-attempt${attempt}`;
  await launchServer();
  resourceTimer = setInterval(sampleResource, 250);
  browser = await chromium.launch({
    executablePath: "/usr/bin/chromium",
    headless: true,
    args: [
      "--no-sandbox",
      "--allow-loopback-in-peer-connection",
      "--disable-dev-shm-usage",
      "--autoplay-policy=no-user-gesture-required",
      "--disable-background-timer-throttling",
      "--disable-renderer-backgrounding",
      "--disable-backgrounding-occluded-windows",
    ],
  });
  let result = {
    name,
    attempt,
    started: new Date().toISOString(),
    duration_s: duration,
    forced_relay_udp: process.env.TORTURE_RELAY_UDP === "1",
    load_wait_ms: loadWaitMs,
 load_start: readFileSync("/proc/loadavg","utf8").trim(),
  };
  try {
    Object.assign(result, await fn());
  } catch (e) {
    result.error = redact(String(e));
    if(e.qualification) result.publisher=e.qualification;
    if(e.qualificationPeers) result.viewers=e.qualificationPeers.filter(Boolean);
    result.pass = false;
  } finally {
    clear();
    bulk?.kill("SIGTERM");
    bulk = undefined;
    clearInterval(resourceTimer);
    sampleResource();
    await browser?.close();
    if (server?.exitCode === null) {
      server.kill("SIGTERM");
      await new Promise((r) => server.once("exit", r));
    }
  }
  result.load_end=readFileSync("/proc/loadavg","utf8").trim();
  result.load_peak_1m = Math.max(Number(result.load_start.split(" ")[0]), Number(result.load_end.split(" ")[0]), ...resources.map(r => r.load));
  const observedLimit = Number(process.env.TORTURE_MAX_OBSERVED_LOAD ?? 0);
  result.load_invalid = observedLimit > 0 && result.load_peak_1m > observedLimit;
  const positive = resources.filter((r) => r.rssKiB > 0);
  result.server_rss_peak_mib =
    Math.max(0, ...positive.map((r) => r.rssKiB)) / 1024;
  const a = positive[0],
    z = positive.at(-1);
  result.server_cpu_pct =
    a && z && z.ticks >= a.ticks
      ? ((z.ticks - a.ticks) / 100 / ((z.at - a.at) / 1000)) * 100
      : null;
  if (result.observation_from) {
    const observed = positive.filter(
      (r) => r.at >= result.observation_from && r.at <= result.observation_to,
    );
    const first = observed[0],
      last = observed.at(-1);
    result.server_cpu_observation_pct =
      first && last
        ? ((last.ticks - first.ticks) / 100 / ((last.at - first.at) / 1000)) *
          100
        : null;
    result.server_rss_observation_peak_mib =
      Math.max(0, ...observed.map((r) => r.rssKiB)) / 1024;
  }
  result.resources = resources;
  result.browser_errors = errors;
  writeFileSync(`${out}/${name}${result.load_invalid ? `.load-attempt${attempt}` : ""}.json`, JSON.stringify(result, null, 2));
  console.log(
    JSON.stringify({
      ...result,
      resources: undefined,
      publisher: undefined,
      metrics: Array.isArray(result.metrics)
        ? `${result.metrics.length} viewer metrics`
        : result.metrics,
      viewers: undefined,
      browser_errors: errors.length,
    }),
  );
  return result;
}
async function profile(name, spec, seconds = duration) {
  await scenario(name, async () => {
    const stream = await createStream();
    const pub = await publisher(stream);
    netem(spec);
    const v = await viewer(stream, 45000);
    const from = Date.now();
    await sleep(seconds * 1000);
    const to = Date.now();
    clear();
    const raw = await snapshot(v);
    const m = summarize(raw, from, to);
    const pass =
      !!raw.ttff &&
      m.frames > 0 &&
      m.pixel_samples > 0 &&
      m.last_frame_age_ms < 1500 &&
      (process.env.TORTURE_RELAY_UDP !== "1" || m.path === "TURN/UDP") &&
      (name !== "bad_wifi" || m.freezes_over_500ms <= seconds / 300) && (name !== "bad_wifi_both_legs" || m.max_gap_ms<=550);
    return { spec, observation_s: seconds, metrics: m, viewers: [raw], publisher: await snapshot(pub), pass };
  });
}
try {
  for (const production of [false,true]) await scenario(production?"production_cold_start":"screen_cold_start",async()=>{
    const source = production?"screen-production":process.env.TORTURE_COLD_SOURCE ?? "screen";
    const stream=await createStream(),pub=await publisher(stream,source,false);
    const start=await pub.evaluate(()=>performance.timeOrigin+window.passstream.started);
    await sleep(Math.max(0,start+10600-Date.now()));
    const diagnosticTimes=await pub.evaluate(()=>new Promise((resolve,reject)=>{
      const channel=window.passstream.channel;
      const timer=setTimeout(()=>{channel.removeEventListener("message",receive);reject(Error("source diagnostic timed out"))},2000);
      function receive(event){try{const message=JSON.parse(event.data);if(message.diagnostics&&message.repair?.sourceFrameTimes){clearTimeout(timer);channel.removeEventListener("message",receive);resolve(message.repair.sourceFrameTimes)}}catch{}}
      channel.addEventListener("message",receive);
      channel.send(JSON.stringify({type:"heartbeat",diagnostics:true}));
    }));
    try { await pub.waitForFunction(()=>{
      const samples=window.passstream.samples.slice(-4);
      return samples.length===4 && samples.every(s=>s.fps>=12);
    },null,{timeout:20000}); } catch {}
    const publisherRaw=await snapshot(pub),samples=publisherRaw.samples.filter(s=>s.timestamp>=start&&s.timestamp<=start+10000);
    const steady=publisherRaw.samples.find((s,i,a)=>a.slice(i,i+4).length===4&&a.slice(i,i+4).every(x=>x.fps>=12));
    const gaps=times=>{
      const points=[start,...times.filter(t=>t>=start&&t<=start+10000),start+10000];
      return points.slice(1).flatMap((at,i)=>at-points[i]>500?[{from_ms:points[i]-start,to_ms:at-start,duration_ms:at-points[i]}]:[]);
    };
    const sourceTimes=[...new Set([...diagnosticTimes,...publisherRaw.samples.flatMap(s=>s.repair?.sourceFrameTimes??[])])].sort((a,b)=>a-b);
    return {diagnostic:!production,source,explanation:"Publisher encoder/rate-control startup with no viewers, measured at ingress before SFU forwarding; not an SFU bandwidth gate",window_ms:10000,time_to_steady_fps_ms:steady?steady.timestamp-start:null,steady_definition:"four consecutive 500ms samples at >=12 encoded fps",min_fps_first10s:samples.length?Math.min(...samples.map(s=>s.fps)):null,min_positive_fps_first10s:samples.some(s=>s.fps>0)?Math.min(...samples.filter(s=>s.fps>0).map(s=>s.fps)):null,publisher_frame_times_first10s:sourceTimes.filter(t=>t>=start&&t<=start+10000),publisher_gaps_first10s:gaps(sourceTimes),steady_bitrate_p50_bps:pct(publisherRaw.samples.slice(-4).map(s=>s.bitrate),.5),keyframe_bytes:publisherRaw.quality.repair?.sourceKeyframeBytes,parameters:await pub.evaluate(()=>window.passstream.pc.getSenders()[0].getParameters()),publisher:publisherRaw,pass:!production || !!steady && steady.timestamp-start<=10000 && gaps(sourceTimes).every(g=>g.duration_ms<=1000)};
  });
  await scenario("screen_source",async()=>{
    const stream=await createStream(),pub=await publisher(stream,"screen");
    const v=await viewer(stream,5000);await sleep(5000);
    const raw=await snapshot(v),publisherRaw=await snapshot(pub),from=Date.now()-4000,to=Date.now();
    const qualification=sourceQualifications.get(pub);
    const offered=pct(qualification.samples.map(s=>s.bitrate),.5);
    const workload=sourceWorkload("screen",publisherRaw,qualification.samples[0].at,qualification.samples.at(-1).at,qualification);
    const postJoin={offered_bps:summarize(publisherRaw,from,to).bitrate_p50_bps,encoded_fps:pct(publisherRaw.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).map(s=>s.fps),.5)};
    const keyframeBytes=Math.max(...raw.samples.map(s=>s.repair?.sourceKeyframeBytes??0));
    const thumbnailSamples=raw.samples.filter(s=>s.layer?.startsWith("180p/"));
    const nativeStamps=new Set(raw.samples.flatMap(s=>s.repair?.fullLayerTimestamps??[]));
    const nativeFull=raw.timeline.find(f=>nativeStamps.has(f.rtpTimestamp))?.at-raw.startedAt;
    const thumbnailKey=Math.max(0,...thumbnailSamples.map(s=>s.repair?.keyframeBytes??0));
    return {source:"2560x1440 detailed editor + animated terminal, motion encoder with an independent 180p fallback",source_workload:workload,post_join_encoder:postJoin,offered_bps:offered,keyframe_bytes:keyframeBytes,thumbnail_keyframe_bytes:thumbnailKey,thumbnail_seen:thumbnailSamples.length>0,native_full_layer_ms:nativeFull,viewers:[raw],publisher:publisherRaw,encoder:await pub.evaluate(async()=>({parameters:window.passstream.pc.getSenders()[0].getParameters(),stats:[...(await window.passstream.pc.getStats()).values()].filter(s=>s.type==="outbound-rtp"||s.type==="media-source")})),pass:raw.ttff>0 && workload.pass && keyframeBytes>=150_000 && thumbnailSamples.length>0 && nativeFull>0 && nativeFull<=2000 && raw.videoSize.width===2560 && raw.videoSize.height===1440};
  });
  await scenario("good", async () => {
    const stream = await createStream();
    await publisher(stream);
    let viewers = [];
    for (let i = 0; i < 5; i++) {
      const p = await viewer(stream);
      await sleep(2000);
      viewers.push(await snapshot(p));
      await p.close();
    }
    const times = viewers.map((v) => v.ttff);
    const fullLayer = viewers.map(v=>{
      const stamps=new Set(v.samples.flatMap(s=>s.repair?.fullLayerTimestamps ?? []));
      return v.timeline.find(f=>stamps.has(f.rtpTimestamp))?.at-v.startedAt;
    });
    return {
      ttff_p50_ms: pct(times, 0.5),
      ttff_p95_ms: pct(times, 0.95),
      full_layer_ms: fullLayer,
      metrics: viewers.map((v) => summarize(v, v.timeline[0]?.at ?? v.capturedAt, v.capturedAt)),
      viewers,
      pass: times.every(Boolean) && pct(times, 0.5) <= 150 && fullLayer.every(t=>t>0&&t<=2000) && viewers.every(v=>v.quality.layer==="L1T3"),
    };
  });
  await profile(
    "bad_wifi_both_legs",
    "delay 40ms 10ms distribution normal loss gemodel 1% 19% 100% 0%",
    Math.max(30, duration),
  );
  await scenario("bad_wifi", async () => {
    const stream=await createStream(), pub=await publisher(stream), v=await viewer(stream,45000);
    // Per-frame E2EE timings and sizes (torture-crypto.mjs).
    await pub.evaluate(()=>window.passstreamSender.trace(true)); await v.evaluate(()=>window.passstreamViewer.trace(true));
    const spec="delay 40ms 10ms distribution normal loss gemodel 1% 19% 100% 0%"+(netemSeed?` seed ${netemSeed}`:"");
    const impairment=await impairViewers([v],spec,["sport","dport"]);
    const seconds=Number(process.env.TORTURE_WIFI_SECONDS ?? 300), from=Date.now();
    const checkpoint=setInterval(async()=>{try{writeFileSync(`${out}/wifi-checkpoint.json`,JSON.stringify(await snapshot(v)))}catch{}},30000);
    await sleep(seconds*1000);
    clearInterval(checkpoint);
    const to=Date.now(), raw=await snapshot(v), m=summarize(raw,from,to);
    const evidence=impairment.stop();
    const rx=await v.evaluate(()=>window.passstreamViewer.dumpTrace()), tx=await pub.evaluate(()=>window.passstreamSender.dumpTrace());
    const e2ee={sender:cryptoSummary(tx,0),viewer:cryptoSummary(rx,1),viewer_audio:cryptoSummary(rx,1,1),gaps:gapForensics(raw,rx,0,from,to)};
    writeFileSync(`${out}/bad_wifi-trace.json`,JSON.stringify({tx,rx}));
    const feedback={viewer:await rtpFeedback(v),publisher:await rtpFeedback(pub),sfu:raw.quality?.repair??null};
    return {e2ee,feedback,audio_mode:audioMode||"default",netem_seed:netemSeed||null,from,to,leg:"viewer only, both directions",impairment:evidence,spec,observation_s:seconds,metrics:m,viewers:[raw],publisher:await snapshot(pub),pass:evidence.verified && m.pixel_samples>0 && m.last_frame_age_ms<1500 && m.freezes_over_500ms===0 && m.max_gap_ms<=550 && m.g2g_p95_ms<=305};
  });
  await scenario("bad_wifi_prejoin",async()=>{
   const stream=await createStream(),pub=await publisher(stream);
   const spec="delay 40ms 10ms distribution normal loss gemodel 1% 19% 100% 0%";
   const impairment=await impairBeforeJoin(pub,spec),v=await viewer(stream,45000),seconds=Number(process.env.TORTURE_WIFI_SECONDS??300),from=Date.now();
   await sleep(seconds*1000);const to=Date.now(),raw=await snapshot(v),metrics=summarize(raw,from,to),evidence=impairment.stop();
   return {from,to,spec,observation_s:seconds,impairment:evidence,metrics,viewers:[raw],publisher:await snapshot(pub),pass:evidence.verified&&metrics.pixel_samples>0&&metrics.last_frame_age_ms<1500&&metrics.freezes_over_500ms===0&&metrics.g2g_p95_ms<=305};
  });
  await scenario("non_pixel_clock",async()=>{
    const stream=await createStream(),pub=await publisher(stream,"plain"),v=await viewer(stream);
    const impairment=await impairViewers([v],"delay 40ms 10ms distribution normal loss gemodel 1% 19% 100% 0%",["sport","dport"]);
    const from=Date.now();await sleep(30000);const to=Date.now(),raw=await snapshot(v),evidence=impairment.stop();
    const metrics=summarize(raw,from,to),repair=raw.quality.repair;
    const protectedSamples=raw.samples.filter(s=>s.layer==="L1T3"&&s.repair?.decoderReported&&s.repair.freshAgeMs<1000&&s.repair.wireBudget>2*s.repair.targetBitrate&&s.repair.estimate?.averageLoss>.01);
    return {source:"canvas without timestamp pixels",metrics,protection_override_samples:protectedSamples.length,viewers:[raw],publisher:await snapshot(pub),impairment:evidence,pass:evidence.verified && metrics.pixel_samples===0 && metrics.frames>50 && metrics.max_gap_ms<=550 && repair?.decoderReported===true && repair.freshFrames>30 && repair.freshAgeMs<1000 && raw.quality.layer==="L1T3" && protectedSamples.length>0};
  });
  await scenario("wifi_replacement",async()=>{
   const stream=await createStream(),pub=await publisher(stream),v=await viewer(stream);
   const impairment=await impairViewers([v],"delay 40ms 10ms distribution normal loss gemodel 1% 19% 100% 0%",["sport","dport"]);
   await sleep(5000);await v.evaluate(()=>window.passstream.replace());
   await sleep(12000);const raw=await snapshot(v),evidence=impairment.stop();
   return {viewers:[raw],publisher:await snapshot(pub),impairment:evidence,pass:evidence.verified && evidence.paths.length>=2 && freshTimeline(raw,Date.now()-2000,Date.now()).length>=3};
  });
  await scenario("viewer_no_heartbeat",async()=>{
    const stream=await createStream(),pub=await publisher(stream),v=await viewer(stream);
    await v.waitForFunction(()=>window.passstream.heartbeatReady===true);
    await v.evaluate(()=>{
      const session=window.passstream;
      session.channel.onmessage=null;session.channel.onopen=null;
      session.channel.close();session.heartbeatReady=false;
    });
    const from=Date.now();await sleep(16000);const to=Date.now(),raw=await snapshot(v);
    const ready=await v.evaluate(()=>window.passstream.heartbeatReady);
    return {impairment:"heartbeat channel closed while media continues beyond the 15s startup deadline",metrics:summarize(raw,from,to),viewers:[raw],publisher:await snapshot(pub),pass:!ready&&raw.replacements===0&&freshTimeline(raw,to-2000,to).length>=3};
  });
  await scenario("stale_video_recovery",async()=>{
   const stream=await createStream(),pub=await publisher(stream),v=await viewer(stream);
   await sleep(2000);const before=await snapshot(v);
   await v.evaluate(()=>{
    const pc=window.passstream.pc,original=pc.getStats.bind(pc);
    pc.getStats=async()=>{const stats=await original(),copy=new Map(stats);for(const [id,s] of stats)if(s.type==="candidate-pair")copy.set(id,{...s,currentRoundTripTime:2});return copy};
   });
   await sleep(600);
   await pub.evaluate(()=>{window.realCaptureNow=Date.now;Date.now=()=>window.realCaptureNow()-5000});
   let stale;
   try {await sleep(3500);stale=await snapshot(v)}
   finally {await pub.evaluate(()=>{Date.now=window.realCaptureNow})}
   const from=Date.now();await sleep(5000);const to=Date.now(),raw=await snapshot(v);
   const fresh=freshTimeline(raw,from,to,1000,from),first=fresh.find((f,i)=>fresh.slice(i,i+3).length===3&&fresh[i+2].at-f.at<=1000);
   const metrics=bandwidthMetrics(raw,from+3000,to);
   return {impairment:"publisher capture clock 5s behind for 3.5s; RTP/heartbeat continue; initial viewer reports inflated 2000ms RTT",replacements_during_stale:stale.replacements-before.replacements,recovery_ms:first?first.at-from:null,metrics,viewers:[raw],publisher:await snapshot(pub),pass:stale.replacements>before.replacements&&!!first&&first.at-from<3000&&bandwidthPass(metrics)};
  });
  for (const loss of [1, 5, 10, 20, 30])
    await profile(`loss_${loss}`, `delay 20ms 5ms loss ${loss}%`);
  for (const rtt of [50, 100, 200, 400, 800])
    await profile(`rtt_${rtt}`, `delay ${rtt / 2}ms 5ms`);
  await profile("reorder", "delay 30ms 10ms reorder 25% 50%");
  await scenario("screen_keyframe_spam",async()=>{
   const stream=await createStream(),pub=await publisher(stream,"screen"),pages=[];
   for(let i=0;i<3;i++) pages.push(await viewer(stream));
   await sleep(10000);const from=Date.now(),before=(await snapshot(pub)).quality.keyframes;
   const timer=setInterval(()=>pages[0].evaluate(()=>{const d=window.passstream.channel;if(d?.readyState==="open")d.send(JSON.stringify({type:"keyframe",freshFrames:0,at:Date.now()}))}).catch(()=>{}),10);
   try { await sleep(30000) } finally {clearInterval(timer)}
   const to=Date.now(),viewers=await Promise.all(pages.map(snapshot)),publisherRaw=await snapshot(pub),keys=publisherRaw.quality.keyframes-before,metrics=viewers.slice(1).map(v=>bandwidthMetrics(v,from,to));
   return {from,to,duration_s:30,native_key_budget_s:10,native_keys:keys,metrics,viewers,publisher:publisherRaw,pass:keys<=4&&metrics.every(m=>bandwidthPass(m)&&m.max_gap_ms===0)&&viewers.slice(1).every(v=>nativePass(nativeMetrics(v,from,to))&&nativeCadencePass(v,publisherRaw,from,to))};
  });
  // Admission and history independence: each rate runs with and without an
  // already established unconstrained decoder. Score the full 30s after N=20s.
  for (const source of ["screen","screen-production"]) for (const rate of source==="screen"?["3mbit","5mbit","10mbit"]:["10mbit"]) for (const beforeJoin of [false,true]) {
   await scenario(`${source==="screen"?"screen":"production"}_native_${rate}_${beforeJoin?"prejoin":"established"}`,async()=>{
    const stream=await createStream(),pub=await publisher(stream,source);
    let v, impairment;
    if (beforeJoin) { impairment=await impairBeforeJoin(pub,`delay 40ms rate ${rate} limit 1000`); v=await viewer(stream); }
    else {
     v=await viewer(stream);
     await v.waitForFunction(()=>document.querySelector("video")?.videoWidth===2560&&window.passstream.quality.layer==="L1T3",null,{timeout:20000});
     impairment=await impairViewers([v],`delay 40ms rate ${rate} limit 1000`);
    }
    // One extra sampling interval at each edge guarantees at least 30s
    // between the cumulative wire/padding counter samples themselves.
    const start=Date.now(); await sleep(20000); const from=Date.now(); await sleep(31000); const to=Date.now();
    const raw=await snapshot(v),native=nativeMetrics(raw,from,to),admission=nativeMetrics(raw,start,to),metrics=bandwidthMetrics(raw,from,to);
    const publisherRaw=await snapshot(pub),workload=sourceWorkload(source,publisherRaw,from,to,sourceQualifications.get(pub));
    const evidence=impairment?.stop();
    return {start,from,to,rate,before_join:beforeJoin,source_workload:workload,native_deadline_s:20,native,admission,metrics,viewers:[raw],publisher:publisherRaw,impairment:evidence,pass:(!evidence||evidence.verified)&&(!workload||workload.pass)&&admission.first_native_ms!==null&&admission.first_native_ms<=20000&&nativeHoldPass(native)&&nativeCadencePass(raw,publisherRaw,from,to)&&paddingPass(native)&&bandwidthPass(metrics)};
   });
  }
  await scenario("screen_native_flap",async()=>{
   const stream=await createStream(),pub=await publisher(stream,"screen"),v=await viewer(stream);
   const impairment=await impairViewers([v],"delay 40ms rate 5mbit limit 1000"),intervals=[];
   for (const rate of ["5mbit","256kbit","5mbit"]) {
    impairment.change(`delay 40ms rate ${rate} limit 1000`);
    const start=Date.now(); await sleep(20000); const from=Date.now(); await sleep(31000); const to=Date.now(),raw=await snapshot(v);
    const publisherRaw=await snapshot(pub),native=nativeMetrics(raw,from,to),admission=nativeMetrics(raw,start,to),metrics=bandwidthMetrics(raw,from,to),workload=sourceWorkload("screen",publisherRaw,from,to,sourceQualifications.get(pub));
    intervals.push({start,from,to,rate,native,admission,metrics,source_workload:workload,pass:workload.pass&&paddingPass(native)&&bandwidthPass(metrics)&&(rate==="256kbit"||admission.first_native_ms!==null&&admission.first_native_ms<=20000&&nativeHoldPass(native)&&nativeCadencePass(raw,publisherRaw,from,to))});
   }
   const evidence=impairment.stop(); return {native_deadline_s:20,intervals,viewers:[await snapshot(v)],publisher:await snapshot(pub),impairment:evidence,pass:evidence.verified&&intervals.every(i=>i.pass)};
  });
  for (const source of ["clock","screen"]) {
  const prefix=source==="screen"?"screen_":"";
  await scenario(prefix+"viewer_256k", async()=>{
   const stream=await createStream(),pub=await publisher(stream,source),v=await viewer(stream);
   await qualifySource(pub,source,Date.now());
   const impairment=await impairViewers([v],"delay 40ms rate 256kbit limit 1000");
   await sleep(10000);
   const from=Date.now(); await sleep(Math.max(30,duration)*1000); const to=Date.now();
   const raw=await snapshot(v),metrics=bandwidthMetrics(raw,from,to),evidence=impairment.stop();
   const publisherRaw=await snapshot(pub),workload=sourceWorkload(source,publisherRaw,from,to,sourceQualifications.get(pub));
   return {convergence_s:10,source_workload:workload,metrics,viewers:[raw],publisher:publisherRaw,impairment:evidence,pass:evidence.verified && (!workload||workload.pass) && bandwidthPass(metrics)};
  });
  await scenario(prefix+"bandwidth_flaps", async()=>{
   const stream=await createStream(),pub=await publisher(stream,source),v=await viewer(stream);
   await qualifySource(pub,source,Date.now());
   const impairment=await impairViewers([v],"delay 40ms rate 50mbit limit 1000");
   const intervals=[];
   for (const rate of ["50mbit","256kbit","50mbit","256kbit","50mbit"]) {
    impairment.change(`delay 40ms rate ${rate} limit 1000`);
    const from=Date.now(); await sleep(Math.max(source==="screen"?30:12,duration)*1000); const to=Date.now(),raw=await snapshot(v);
    const fresh=freshTimeline(raw,from,to,1000,from);
    const recovery=fresh.find((f,i)=>fresh.slice(i,i+3).length===3 && fresh[i+2].at-f.at<=1000);
    const metrics=bandwidthMetrics(raw,from+3000,to);
    // Keep the original transition gates while exercising the detailed
    // workload long enough to include steady encoding after a requested key.
    const transition=source==="screen"?bandwidthMetrics(raw,from+3000,from+12000):undefined;
    const native=source==="screen"&&rate!=="256kbit"?nativeMetrics(raw,from+20000,to):undefined;
    const publisherRaw=await snapshot(pub),workload=sourceWorkload(source,publisherRaw,from+3000,to,sourceQualifications.get(pub));
    intervals.push({rate,from,to,native,observation_s:(to-from)/1000,source_workload:workload,recovery_ms:recovery?recovery.at-from:null,metrics,transition_12s_metrics:transition,pass:!!recovery && recovery.at-from<=3000 && (!workload||workload.pass) && bandwidthPass(metrics) && (!transition||bandwidthPass(transition)) && (!native||nativePass(native)&&nativeCadencePass(raw,publisherRaw,from+20000,to))});
    writeFileSync(`${out}/${prefix}bandwidth_flaps-step${intervals.length}.json`,JSON.stringify({interval:intervals.at(-1),recent_samples:raw.samples.slice(-12)},null,2));
   }
   const evidence=impairment.stop();
   return {intervals,viewers:[await snapshot(v)],publisher:await snapshot(pub),impairment:evidence,pass:evidence.verified && intervals.every(i=>i.pass)};
  });
  for (const [rate,bps,n,fifo=false,prejoin=false,rtt=0] of [["5mbit",5000000,1,false,true,160],["10mbit",10000000,2,true,true,160],["512kbit",512000,1,false,true,160],["512kbit",512000,2,true,true,160],["512kbit",512000,1],["2mbit",2000000,1],["512kbit",512000,2],["512kbit",512000,1,true],["512kbit",512000,2,true],["2mbit",2000000,1,true],["2mbit",2000000,2,true]]) await scenario(prefix+`competing_${fifo?"fifo_":""}${rate}_${n}v${prejoin?"_prejoin_rtt160":""}`,async()=>{
   const stream=await createStream(),pub=await publisher(stream,source),pages=[];
   let impairment;
   if(prejoin) { impairment=await impairBeforeJoin(pub,`delay ${rtt/2}ms rate ${rate} limit 1000`); if(!fifo) configureAQM(rate,rtt/2); }
   for(let i=0;i<n;i++) pages.push(await viewer(stream,45000));
   await qualifySource(pub,source,Date.now());
   impairment??=await impairViewers(pages,fifo?`delay 40ms rate ${rate} limit 1000`:"limit 1000",fifo?["sport","dport"]:["dport"]);
   // Keep fair-queue AQM and the original shared FIFO as separate gates.
   // The FIFO also records a contemporaneous queue-delay probe and has a
   // matched TCP-only control below; neither changes its pass criteria.
   if(!fifo&&!prejoin) configureAQM(rate);
   // Bulk packets share the bottleneck and rate limiter with downstream media.
   for(const direction of fifo?["sport","dport"]:["dport"]) tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","1","u32","match","ip","protocol","6","0xff","match","ip",direction,"18081","0xffff","flowid","1:3"]);
   const probe=fifo?await queueProbe():null;
   let bytes=0;
   const sink=createServer((req,res)=>{req.on("data",d=>bytes+=d.length);req.on("end",()=>res.end("ok"))});
   await new Promise(r=>sink.listen(18081,"127.0.0.1",r));
   const req=request("http://127.0.0.1:18081/",{method:"POST"});req.on("error",()=>{});
   const chunk=Buffer.alloc(4096,42);function pump(){while(!req.destroyed&&req.write(chunk)){}}req.on("drain",pump);pump();
   try {
    const convergence=10;
    await sleep(convergence*1000); const startBytes=bytes,from=Date.now();
    await sleep(Math.max(30,duration)*1000);const to=Date.now();
    const goodput=(bytes-startBytes)*8/((to-from)/1000);
    const viewers=await Promise.all(pages.map(snapshot)),metrics=viewers.map(v=>bandwidthMetrics(v,from,to)),evidence=impairment.stop();
    const publisherRaw=await snapshot(pub),offered=meanBitrate(publisherRaw,from,to),offeredFPS=pct(publisherRaw.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).map(s=>s.fps),.5);
    const workload=sourceWorkload(source,publisherRaw,from,to,sourceQualifications.get(pub));
    const fairness=metrics.map((m,i)=>({offered_bps:offered,applicable:offered>0&&offered*n<=bps*.5,received_bps:meanBitrate(viewers[i],from,to),bitrate_measurement:"time-weighted byte deltas",offered_fps:offeredFPS,decoded_fps:m.frames/((to-from)/1000),decoded_frames:m.frames,observation_s:(to-from)/1000,full_layer_share:viewers[i].samples.filter(s=>s.timestamp>=from&&s.timestamp<=to&&s.layer==="L1T3").length/Math.max(1,viewers[i].samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).length)}));
    const native=source==="screen"?viewers.map(v=>nativeMetrics(v,from,to)):undefined;
    const keyRate=8*Math.max(publisherRaw.quality.repair?.sourceKeyframeBytes??0,...publisherRaw.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).map(s=>s.repair?.sourceKeyframeBytes??0));
    // Key bursts can raise the mean offered bitrate above a fair share.
    // They must not hide the native floor when steady demand and a one-second
    // key both fit that share. Keep the existing byte/fps fairness gate too.
    const nativeAffordable=!!native&&Math.max(workload.offered_bps,keyRate)*n<=bps*.5;
    const fair=fairness.every(mediaFloorPass)&&(!native||native.every((m,i)=>!(nativeAffordable||fairness[i].applicable)||nativePass(m)&&nativeCadencePass(viewers[i],publisherRaw,from,to)));
    return {from,to,rate,source,source_workload:workload,fairness,native,native_floor_applicable:nativeAffordable,before_join:prejoin,propagation_ms:rtt,queue_model:fifo?`original shared deep FIFO, both directions, ${prejoin?rtt/2:40}ms delay, 1000 packets`:"HTB with FQ-CoDel, 1000 packet memory bound, 5ms target/100ms interval",convergence_s:convergence,tcp_goodput_bps:goodput,tcp_link_share:goodput/bps,queue_probe:probe?{metrics:probe.metrics(from,to),samples:probe.samples}:undefined,metrics,viewers,publisher:publisherRaw,impairment:evidence,pass:evidence.verified && (!workload||workload.pass) && fair && (!prejoin||viewers.every(v=>pct(v.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).map(s=>s.rtt),.5)>=150)) && viewers.every(v=>v.quality.repair?.initialRTTMs>0) && goodput>=bps*.3 && metrics.every(bandwidthPass)};
   } finally { probe?.stop();req.destroy();sink.closeAllConnections();await new Promise(r=>sink.close(r));impairment.stop() }
  });
  }
  await scenario("fifo_tcp_only_512kbit",async()=>{
   shapedRoot();
   tc(["qdisc","add","dev","lo","parent","1:3","handle","30:","netem","delay","40ms","rate","512kbit","limit","1000"]);
   for(const direction of ["sport","dport"]) tc(["filter","add","dev","lo","parent","1:","protocol","ip","prio","1","u32","match","ip","protocol","6","0xff","match","ip",direction,"18081","0xffff","flowid","1:3"]);
   const probe=await queueProbe();let bytes=0;
   const sink=createServer((req,res)=>{req.on("data",d=>bytes+=d.length);req.on("end",()=>res.end("ok"))});
   await new Promise(r=>sink.listen(18081,"127.0.0.1",r));
   const req=request("http://127.0.0.1:18081/",{method:"POST"});req.on("error",()=>{});
   const chunk=Buffer.alloc(4096,42);function pump(){while(!req.destroyed&&req.write(chunk)){}}req.on("drain",pump);pump();
   try {
    await sleep(10000);const from=Date.now(),startBytes=bytes;await sleep(Math.max(30,duration)*1000);const to=Date.now();
    const metrics=probe.metrics(from,to),goodput=(bytes-startBytes)*8/((to-from)/1000);
    return {diagnostic:true,queue_model:"original shared deep FIFO, both directions, 40ms delay, 1000 packets",convergence_s:10,queue_probe:{metrics,samples:probe.samples},tcp_goodput_bps:goodput,tcp_link_share:goodput/512000,pass:metrics.samples>0&&goodput>0};
   } finally {probe.stop();req.destroy();sink.closeAllConnections();await new Promise(r=>sink.close(r))}
  });
  await scenario("signaling_outage",async()=>{
   const stream=await createStream(),pub=await publisher(stream),v=await viewer(stream);
   await v.evaluate(()=>{window.wsAttempts=0;window.WebSocket=new Proxy(window.WebSocket,{construct(target,args){window.wsAttempts++;return new target(...args)}})});
   server.kill("SIGKILL");await new Promise(r=>server.once("exit",r));await sleep(12000);
   const attempts=await v.evaluate(()=>window.wsAttempts);
   await launchServer();await sleep(5000);const raw=await snapshot(v);
   return {attempts_in_12s:attempts,viewers:[raw],publisher:await snapshot(pub),pass:attempts<=10 && freshTimeline(raw,Date.now()-2000,Date.now()).length>=3};
  });
  await scenario("bulk_upload", async () => {
    const stream = await createStream();
    await publisher(stream);
    const v = await viewer(stream);
    netem("delay 40ms rate 5mbit");
    let bytes = 0;
    const sink = createServer((req, res) => {
      req.on("data", (b) => (bytes += b.length));
      req.on("end", () => res.end("ok"));
    });
    await new Promise((r) => sink.listen(18081, "127.0.0.1", r));
    const uploads = [];
    for (let i = 0; i < 2; i++) {
      const req = request("http://127.0.0.1:18081/", { method: "POST" });
      req.on("error", () => {});
      const chunk = Buffer.alloc(16384, 42);
      function pump() {
        while (!req.destroyed && req.write(chunk)) {}
      }
      req.on("drain", pump);
      pump();
      uploads.push(req);
    }
    const from = Date.now();
    await sleep(duration * 1000);
    uploads.forEach((r) => r.destroy());
    sink.closeAllConnections();
    await new Promise((r) => sink.close(r));
    const raw = await snapshot(v);
    return {
      load: "two competing streaming HTTP POST uploads over the same 5 Mbit/s qdisc",
      uploaded_bytes: bytes,
      metrics: summarize(raw, from, Date.now()),
      viewers: [raw],
      pass: raw.timeline.some((f) => f.at > Date.now() - 2000),
    };
  });
  async function event(name, action) {
    for (
      let trial = 1;
      trial <= Number(process.env.TORTURE_RECOVERY_TRIALS ?? 5);
      trial++
    ) {
      await scenario(name + (trial === 1 ? "" : `_trial${trial}`), async () => {
        const stream = await createStream(),
          p = await publisher(stream),
          v = await viewer(stream);
        await sleep(2000);
        const before = Date.now();
        const marker = await action({ p, v });
        const recoveryFrom = marker ?? before;
        await sleep(16000);
        const to = Date.now(),
          raw = await snapshot(v);
        const tcpAt =
          name === "udp_blackhole"
            ? raw.samples.find(
                (s) => s.timestamp > before && s.path.includes("TCP"),
              )?.timestamp
            : 0;
        const freshFrames = freshTimeline(raw,recoveryFrom,to,1500,recoveryFrom);
        const fresh = freshFrames.find(
          (f, i) =>
            freshFrames.slice(i, i + 3).length === 3 &&
            freshFrames[i + 2].at - f.at <= 1000,
        );
        const finalFresh = freshFrames.at(-1);
        const sustained =
          !!finalFresh &&
          to - finalFresh.at < 1500 &&
          freshFrames.filter((f) => f.at > to - 5000).length >= 5;
        const recovery = fresh ? fresh.at - recoveryFrom : null;
        // A recovery followed by another long freeze is not a recovery.
        const after = fresh ? freshFrames.filter((f) => f.at >= fresh.at) : [];
        let postGap = after.length ? to - after.at(-1).at : null;
        for (let i = 1; i < after.length; i++)
          postGap = Math.max(postGap, after[i].at - after[i - 1].at);
        return {
          trial,
          publisher: await snapshot(p),
          event_at: before,
          recovery_from: recoveryFrom,
          recovery_ms: recovery,
          recovery_target_ms: ["viewer_flap", "network_flap"].includes(name) ? 500 : 3000,
          post_recovery_max_gap_ms: postGap,
          sustained_recovery: sustained,
          metrics: summarize(raw, before, to),
          viewers: [raw],
          pass:
            recovery !== null &&
            recovery < 3000 &&
            (!["viewer_flap", "network_flap"].includes(name) || recovery <= 500) &&
            sustained &&
            postGap < 1500 &&
            (name !== "udp_blackhole" || raw.quality.path.includes("TCP")),
        };
      });
    }
  }
  await event("udp_blackhole", async () => {
    udpBlackhole();
  });
  await event("server_restart", async () => {
    server.kill("SIGKILL");
    await new Promise((r) => server.once("exit", r));
    await sleep(300);
    await launchServer();
    return Date.now();
  });
  async function clientFlap(p) {
    const port = await selectedPort(p);
    if (!port) throw Error("no selected UDP port");
    clear();
    shapedRoot();
    for (const direction of ["sport", "dport"])
      tc([
        "filter",
        "add",
        "dev",
        "lo",
        "parent",
        "1:",
        "protocol",
        "ip",
        "prio",
        "1",
        "u32",
        "match",
        "ip",
        "protocol",
        "17",
        "0xff",
        "match",
        "ip",
        direction,
        String(port),
        "0xffff",
        "flowid",
        "1:3",
      ]);
    tc([
      "qdisc",
      "add",
      "dev",
      "lo",
      "parent",
      "1:3",
      "handle",
      "30:",
      "netem",
      "loss",
      "100%",
    ]);
    await p.context().setOffline(true);
    await sleep(2500);
    clear();
    await p.context().setOffline(false);
    return Date.now();
  }
  await event("viewer_flap", async ({ v }) => clientFlap(v));
  await event("publisher_flap", async ({ p }) => clientFlap(p));
  await event("network_flap", async () => {
    netem("loss 100%");
    await sleep(2500);
    clear();
    return Date.now();
  });
  await scenario("cpu_throttle", async () => {
    const stream = await createStream();
    await publisher(stream);
    const v = await viewer(stream);
    const cdp = await v.context().newCDPSession(v);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 6 });
    const from = Date.now();
    await sleep(duration * 1000);
    const raw = await snapshot(v);
    return {
      throttle: "Chromium viewer main thread 6x (not a server CPU quota)",
      metrics: summarize(raw, from, Date.now()),
      viewers: [raw],
      pass: raw.timeline.some((f) => f.at > Date.now() - 2000),
    };
  });
  await scenario("server_cpu_throttle", async () => {
    const stream = await createStream();
    await publisher(stream);
    const v = await viewer(stream);
    const from = Date.now();
    let active = true;
    const worker = (async () => {
      while (active) {
        server.kill("SIGSTOP");
        await sleep(80);
        server.kill("SIGCONT");
        await sleep(20);
      }
    })();
    try {
      await sleep(duration * 1000);
    } finally {
      active = false;
      server.kill("SIGCONT");
      await worker;
    }
    const raw = await snapshot(v);
    return {
      throttle:
        "server SIGSTOP/SIGCONT 80ms/20ms duty cycle (20% scheduled time; not cgroup quota)",
      metrics: summarize(raw, from, Date.now()),
      viewers: [raw],
      pass: raw.timeline.some((f) => f.at > Date.now() - 2000),
    };
  });
  await scenario("terrible_viewer", async () => {
    const stream = await createStream();
    const p = await publisher(stream);
    const healthy = await viewer(stream),
      bad = await viewer(stream);
    await sleep(1000);
    const port = await bad.evaluate(async () => {
      const stats = await window.passstream.pc.getStats();
      for (const s of stats.values()) {
        if (
          s.type === "candidate-pair" &&
          s.nominated &&
          s.state === "succeeded"
        )
          return stats.get(s.localCandidateId)?.port;
      }
    });
    if (!port) throw Error("no selected viewer UDP port");
    clear();
    shapedRoot();
    tc([
      "filter",
      "add",
      "dev",
      "lo",
      "parent",
      "1:",
      "protocol",
      "ip",
      "prio",
      "1",
      "u32",
      "match",
      "ip",
      "protocol",
      "17",
      "0xff",
      "match",
      "ip",
      "dport",
      String(port),
      "0xffff",
      "flowid",
      "1:3",
    ]);
    tc([
      "qdisc",
      "add",
      "dev",
      "lo",
      "parent",
      "1:3",
      "handle",
      "30:",
      "netem",
      "delay",
      "400ms",
      "loss",
      "30%",
      "rate",
      "256kbit",
    ]);
    const from = Date.now();
    await sleep(duration * 1000);
    const a = await snapshot(healthy),
      b = await snapshot(bad);
    return {
      target_viewer_udp_port: port,
      metrics: summarize(a, from, Date.now()),
      bad_metrics: summarize(b, from, Date.now()),
      viewers: [a, b],
      pass:
        a.timeline.some((f) => f.at > Date.now() - 1500) &&
        summarize(a, from, Date.now()).freezes_over_500ms === 0,
    };
  });
  for (const n of [1, 4, 8, 16, 32, 64, 128]) {
    if (n > stressMax) break;
    await scenario(`stress_viewers_${n}`, async () => {
      const stream = await createStream();
      await publisher(stream);
      const pages = [];
      for (let i = 0; i < n; i++) pages.push(await viewer(stream, 15000));
      const from = Date.now();
      await sleep(duration * 1000);
      const to = Date.now(),
        raw = await Promise.all(pages.map(snapshot)),
        m = raw.map((v) => summarize(v, from, to));
      return {
        viewer_count: n,
        observation_from: from,
        observation_to: to,
        metrics: m,
        viewers: raw,
        pass:
          pct(
            m.map((x) => x.ttff_ms ?? Infinity),
            0.5,
          ) < 1000 &&
          m.every(
            (x) =>
              x.ttff_ms &&
              x.frames > 0 &&
              x.freezes_over_500ms === 0 &&
              x.last_frame_age_ms < 1500,
          ),
      };
    });
    if (results.at(-1)?.name === `stress_viewers_${n}` && !results.at(-1).pass)
      break;
  }
  for (const n of [1, 4, 8, 16, 32, 64, 128]) {
    if (n > stressMax) break;
    await scenario(`stress_streams_${n}`, async () => {
      const pages = [];
      for (let i = 0; i < n; i++) {
        const stream = await createStream();
        await publisher(stream);
        pages.push(await viewer(stream, 15000));
      }
      const from = Date.now();
      await sleep(duration * 1000);
      const to = Date.now(),
        raw = await Promise.all(pages.map(snapshot)),
        m = raw.map((v) => summarize(v, from, to));
      return {
        stream_count: n,
        viewer_count: n,
        observation_from: from,
        observation_to: to,
        metrics: m,
        viewers: raw,
        pass:
          pct(
            m.map((x) => x.ttff_ms ?? Infinity),
            0.5,
          ) < 1000 &&
          m.every(
            (x) =>
              x.ttff_ms &&
              x.frames > 0 &&
              x.freezes_over_500ms === 0 &&
              x.last_frame_age_ms < 1500,
          ),
      };
    });
    if (results.at(-1)?.name === `stress_streams_${n}` && !results.at(-1).pass)
      break;
  }
} finally {
  clear();
  await browser?.close();
  if (server?.exitCode === null) server.kill("SIGTERM");
  const metadata = {
    run,
    offloads,
    nice: Number(readFileSync("/proc/self/stat","utf8").split(") ")[1].split(" ")[16]),
    low_load_required: process.env.TORTURE_REQUIRE_LOW_LOAD==="1",
    load_limit: Number(process.env.TORTURE_MAX_LOAD ?? 0),
    observed_load_limit: Number(process.env.TORTURE_MAX_OBSERVED_LOAD ?? 0),
    load_rejected: loadRejected,
    created: new Date().toISOString(),
    binary_sha256: createHash("sha256").update(readFileSync(bin)).digest("hex"),
    harness_sha256: harnessHashes,
    commit: cmd("git", ["rev-parse", "HEAD"]).trim(),
    go: cmd("mise", ["exec", "--", "go", "version"]).trim(),
    chromium: cmd("/usr/bin/chromium", ["--version"]).trim(),
    kernel: cmd("uname", ["-sr"]).trim(),
    cpu: readFileSync("/proc/cpuinfo", "utf8").match(
      /model name\s+: (.*)/,
    )?.[1],
    memory: readFileSync("/proc/meminfo", "utf8").split("\n")[0],
    duration,
    results,
  };
  writeFileSync(
    `test/torture/results/${run}.json`,
    JSON.stringify(metadata, null, 2),
  );
  console.log(`Results: test/torture/results/${run}.json`);
}
if (results.some((r) => !r.pass)) process.exitCode = 1;
