const sourceEncodings = new WeakMap<MediaStreamTrack, RTCRtpEncodingParameters>();
export function publisherEncoding(track: MediaStreamTrack): RTCRtpEncodingParameters {
  return { ...(sourceEncodings.get(track) ?? { maxBitrate: 4_000_000, scalabilityMode: "L1T3" }) };
}
const sourceTimers = new WeakMap<MediaStream, ReturnType<typeof setInterval>>();
export function stopTestSource(stream: MediaStream) {
  clearInterval(sourceTimers.get(stream));
  sourceTimers.delete(stream);
}
// 48 timestamp bits + 16 frame bits, robust black/white cells survive VP9.
export function testSource() {
  const kind = new URLSearchParams(location.search).get("source");
  if (kind === "screen-production") return screenSource(4_000_000, true);
  if (kind === "screen" || kind === "screen-startup" || kind === "screen-baseline")
    return screenSource(kind === "screen-startup" ? 3_000_000 : 2_000_000);
  const canvas = document.createElement("canvas");
  // `?source=720p`: the same scene scaled to exactly 1280×720 (the clock row
  // scales with it, so decoders that scale by width / 960 still read it).
  const k = kind === "720p" ? 1280 / 960 : 1;
  canvas.width = 960 * k;
  canvas.height = 540 * k;
  const ctx = canvas.getContext("2d")!;
  ctx.scale(k, k);
  let frame = 0;
  const draw = () => {
    const now = Date.now();
    ctx.fillStyle = "#eaf1f5";
    ctx.fillRect(0, 0, 960, 540);
    ctx.fillStyle = "#16455b";
    ctx.font = "bold 38px monospace";
    ctx.fillText("PassStream / moving text", 32, 130);
    ctx.font = "26px monospace";
    ctx.fillText(`${new Date(now).toISOString()} · frame ${frame}`, 32, 185);
    ctx.fillText("The quick brown fox jumps over the lazy dog.", 32, 245);
    ctx.fillText("0123456789 — sharp text, steady motion", 32, 290);
    ctx.fillStyle = "#287a9e";
    ctx.fillRect((frame * 11) % 860, 350, 100, 60);
    for (let bit = 0; kind !== "plain" && bit < 64; bit++) {
      const value =
        bit < 48
          ? Math.floor(now / 2 ** bit) % 2
          : Math.floor(frame / 2 ** (bit - 48)) % 2;
      ctx.fillStyle = value ? "#fff" : "#000";
      ctx.fillRect(bit * 12, 0, 12, 36);
    }
    frame++;
  };
  draw();
  const timer = setInterval(draw, 1000 / 15);
  const stream = canvas.captureStream(15);
  sourceTimers.set(stream, timer);
  stream
    .getVideoTracks()[0]
    .addEventListener("ended", () => clearInterval(timer));
  return stream;
}
/**
 * Paused and ended pictures are drawn by the sender (PLAN "Viewer UX"), so
 * they show in native fullscreen and PiP and travel encrypted. A row of 64
 * cells under the clock row marks them: a 32-bit magic, a 16-bit kind and
 * its complement. Real screen content practically never matches all 64.
 */
export type Marker = "paused" | "ended" | null;
const MARKER_MAGIC = 0x5053_4d4b; // "PSMK"
const MARKER_KIND = { paused: 1, ended: 2 } as const;
const MARKER_Y = 40;
function markerBits(kind: "paused" | "ended"): number[] {
  const k = MARKER_KIND[kind];
  const bits: number[] = [];
  for (let b = 0; b < 32; b++) bits.push(Math.floor(MARKER_MAGIC / 2 ** b) % 2);
  for (let b = 0; b < 16; b++) bits.push(Math.floor(k / 2 ** b) % 2);
  for (let b = 0; b < 16; b++) bits.push(Math.floor((~k & 0xffff) / 2 ** b) % 2);
  return bits;
}
/**
 * The status picture's size: never one any scaling of the capture can
 * produce. Switching to it then changes the encoded size on every encoding
 * (native, scaled by the encoder, and the independent 180p one, whose factor
 * is fixed by the capture's width), which forces the keyframe viewers need
 * right after the curtain's fence. A capture near 16:9 gets a 4:3 picture
 * (1920×1440); anything else gets 16:9 (1280×720). The aspect ratios differ by
 * at least 0.1, so no uniform scale (even rounded to even sizes) maps one to
 * the other. The 4:3 picture is large so its marker row stays readable on the
 * 180p encoding of a large capture: a 2560-wide capture scales it to 240 px
 * (2.25 px marker cells; at 1024 wide it was 128 px and unreadable).
 */
export function statusSize(capture?: { width?: number; height?: number } | null): { width: number; height: number } {
  const w = capture?.width ?? 0,
    h = capture?.height ?? 0;
  if (w > 0 && h > 0 && Math.abs(w / h - 16 / 9) < 0.1) return { width: 1920, height: 1440 };
  return { width: 1280, height: 720 };
}

/**
 * A canvas track showing "Paused" or "Ended", redrawn at 5 fps so frames keep
 * flowing. Sized by statusSize(capture). Everything is drawn in 1280-wide
 * coordinates scaled to the canvas width; the marker row stays at the top,
 * where the decoder (which scales by width / 1280) reads it.
 */
export function statusSource(kind: "paused" | "ended", capture?: { width?: number; height?: number } | null): MediaStream {
  const size = statusSize(capture);
  const canvas = document.createElement("canvas");
  canvas.width = size.width;
  canvas.height = size.height;
  const ctx = canvas.getContext("2d")!;
  const s = size.width / 1280;
  // The text block (720 units tall at this scale) is centred; the marker isn't moved.
  const dy = (size.height / s - 720) / 2;
  const bits = markerBits(kind);
  let n = 0;
  const draw = () => {
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.fillStyle = "#101418";
    ctx.fillRect(0, 0, size.width, size.height);
    ctx.setTransform(s, 0, 0, s, 0, 0);
    bits.forEach((v, i) => {
      ctx.fillStyle = v ? "#fff" : "#000";
      ctx.fillRect(i * 12, MARKER_Y, 12, 24);
    });
    ctx.translate(0, dy);
    ctx.fillStyle = "#f4efe6";
    ctx.textAlign = "center";
    ctx.font = "600 96px system-ui, sans-serif";
    ctx.fillText(kind === "paused" ? "Paused" : "Ended", 640, 380);
    ctx.font = "32px system-ui, sans-serif";
    ctx.fillStyle = "#a9b0b8";
    ctx.fillText(kind === "paused" ? "The host paused their screen." : "The host stopped sharing.", 640, 450);
    // A slowly moving dot keeps the encoder producing frames.
    ctx.fillStyle = "#e8735c";
    ctx.beginPath();
    ctx.arc(600 + (n++ % 20) * 4, 520, 6, 0, Math.PI * 2);
    ctx.fill();
  };
  draw();
  const timer = setInterval(draw, 200);
  const stream = canvas.captureStream(5);
  sourceTimers.set(stream, timer);
  stream.getVideoTracks()[0].contentHint = "text";
  stream.getVideoTracks()[0].addEventListener("ended", () => clearInterval(timer));
  return stream;
}
export function decode(video: HTMLVideoElement, canvas: HTMLCanvasElement) {
  return decodeFrame(video, canvas).sample;
}
export function decodeFrame(video: HTMLVideoElement, canvas: HTMLCanvasElement): { sample: { delay: number; frame: number } | null; marker: Marker } {
  if (!video.videoWidth) return { sample: null, marker: null };
  if (canvas.width !== 768) canvas.width = 768;
  if (canvas.height !== 2) canvas.height = 2;
  const c = canvas.getContext("2d", { willReadFrequently: true })!;
  // Status pictures are drawn in 1280-wide coordinates (statusSize may make them 1920 wide; the scale follows the width).
  const statusScale = video.videoWidth / 1280;
  c.drawImage(video, 0, (MARKER_Y + 12) * statusScale, 768 * statusScale, statusScale, 0, 1, 768, 1);
  const markerData = c.getImageData(0, 1, 768, 1).data;
  const read = (bit: number) => {
    const off = (bit * 12 + 6) * 4;
    return markerData[off] + markerData[off + 1] + markerData[off + 2] > 384 ? 1 : 0;
  };
  let magic = 0, kind = 0, inv = 0;
  for (let bit = 0; bit < 32; bit++) magic += read(bit) * 2 ** bit;
  for (let bit = 0; bit < 16; bit++) kind += read(32 + bit) * 2 ** bit;
  for (let bit = 0; bit < 16; bit++) inv += read(48 + bit) * 2 ** bit;
  const marker: Marker = magic === MARKER_MAGIC && (kind ^ inv) === 0xffff ? kind === 1 ? "paused" : kind === 2 ? "ended" : null : null;
  // Both sources can reach the independent 180p encoding. Try their clock
  // geometries; only a plausible 48-bit timestamp counts as a clock sample.
  for (const width of [2560, 960]) {
    const scale = video.videoWidth / width;
    c.drawImage(video, 0, (width === 2560 ? 36 : 18) * scale, (width === 2560 ? 2048 : 768) * scale, scale, 0, 0, 768, 1);
    const data = c.getImageData(0, 0, 768, 1).data;
    let timestamp = 0, frame = 0;
    for (let bit = 0; bit < 64; bit++) {
      const off = (bit * 12 + 6) * 4;
      const on = data[off] + data[off + 1] + data[off + 2] > 384 ? 1 : 0;
      if (bit < 48) timestamp += on * 2 ** bit;
      else frame += on * 2 ** (bit - 48);
    }
    const delay = Date.now() - timestamp;
    if (delay >= 0 && delay < 120_000) return { sample: { delay, frame }, marker };
  }
  return { sample: null, marker };
}

// A detailed 1440p desktop: dense editor/terminal text, charts and a moving
// terminal pane. The pixel clock is only an independent latency instrument.
function screenSource(maxBitrate: number, production = false) {
  const canvas = document.createElement("canvas");
  canvas.width = 2560;
  canvas.height = 1440;
  const ctx = canvas.getContext("2d")!;
  const desktop = document.createElement("canvas");
  desktop.width = canvas.width;
  desktop.height = canvas.height;
  const bg = desktop.getContext("2d")!;
  bg.fillStyle = "#15202a";
  bg.fillRect(0, 0, 2560, 1440);
  let seed = 42;
  const random = () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed; };
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789_{}[]():;=+-/";
  const line = () => Array.from({ length: 67 }, () => alphabet[random() % alphabet.length]).join("");
  bg.font = "12px monospace";
  for (let column = 0; column < 4; column++) {
    for (let row = 0; row < 84; row++) {
      bg.fillStyle = ["#b2c8da", "#90c6a0", "#cbafdb", "#dab992"][random() % 4];
      bg.fillText(`${String(row + 1).padStart(3)}  ${line()}`, column * 640 + 8, row * 16 + 80);
    }
  }
  let frame = 0;
  const draw = () => {
    const now = Date.now();
    ctx.drawImage(desktop, 0, 0);
    ctx.fillStyle = "#203447";
    ctx.fillRect(1940, 1020, 600, 400);
    ctx.font = "8px monospace";
    for (let row = 0; row < 32; row++) {
      ctx.fillStyle = row % 3 ? "#c2d6e6" : "#88c5a3";
      ctx.fillText(`${frame * 32 + row} ${line()}`, 1948, 1034 + row * 12);
    }
    ctx.fillStyle = "#5bbbd0";
    ctx.fillRect((frame * 17) % 1650, 1410, 100, 24);
    for (let bit = 0; bit < 64; bit++) {
      const value = bit < 48 ? Math.floor(now / 2 ** bit) % 2 : Math.floor(frame / 2 ** (bit - 48)) % 2;
      ctx.fillStyle = value ? "#fff" : "#000";
      ctx.fillRect(bit * 32, 0, 32, 72);
    }
    frame++;
  };
  draw();
  const timer = setInterval(draw, 1000 / 15);
  const stream = canvas.captureStream(15);
  sourceTimers.set(stream, timer);
  const track = stream.getVideoTracks()[0];
  track.contentHint = production ? "text" : "motion";
  if (!production) sourceEncodings.set(track, { maxBitrate, maxFramerate: 15, scalabilityMode: "L1T3" });
  stream.getVideoTracks()[0].addEventListener("ended", () => clearInterval(timer));
  return stream;
}
