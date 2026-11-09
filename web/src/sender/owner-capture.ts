/** Plaintext capture stays on the owner machine. Nothing in this module uses
 * signaling or the remote server; its only sink is the authenticated daemon. */
import { fail, type Result } from "./authority";

export const MAX_PIXELS = 3840 * 2160;
export const MAX_FRAME = 4 << 20;
export const MAX_RECORDING = 32 << 20;

/** Exact RGBA comparison avoids issuing change IDs for identical pictures. */
export function pictureChanged(previous: Uint8ClampedArray | undefined, next: Uint8ClampedArray): boolean {
  if (!previous || previous.length !== next.length) return true;
  for (let i = 0; i < next.length; i++) if (previous[i] !== next[i]) return true;
  return false;
}

export function base64(bytes: Uint8Array): string {
  let s = "";
  for (let i = 0; i < bytes.length; i += 32768) s += String.fromCharCode(...bytes.subarray(i, i + 32768));
  return btoa(s);
}

export interface CaptureGate {
  live: boolean;
  paused: boolean;
  suspended: boolean;
  attached: boolean;
}
export function captureAllowed(g: CaptureGate, track: Pick<MediaStreamTrack, "enabled" | "muted" | "readyState"> | undefined): boolean {
  return g.live && !g.paused && !g.suspended && g.attached && !!track && track.enabled && !track.muted && track.readyState === "live";
}

export class OwnerCapture {
 readonly metrics={readbacks:0,encodes:0};
  private readonly video = document.createElement("video");
  private readonly canvas = document.createElement("canvas");
  private previous?: Uint8ClampedArray;
 private stream?: MediaStream;
 private demandUntil = 0;
 private lastMarker = "";
 private recordStarted = 0;
  private timer: ReturnType<typeof setInterval>;
  private busy = false;
  private open = false;
  private generation = 0;
  private readonly prefix = crypto.randomUUID();
  private seq = 0;
  private recorder?: MediaRecorder;
  private recordTimer?: ReturnType<typeof setTimeout>;
  private chunks: Blob[] = [];
  private recordBytes = 0;
  private recordLimit = MAX_RECORDING;
  private recordGeneration = 0;
  private recordDone?: Promise<void>;
  private recordResolve?: () => void;
  private completed = false;
  private error = "";

  constructor(private readonly gate: () => CaptureGate, private readonly send: (v: unknown) => boolean, private readonly sendArtifact: (v: unknown) => Promise<boolean>) {
    this.video.muted = true;
    this.video.playsInline = true;
    this.timer = setInterval(() => { this.sync(); void this.sample(); }, 500);
  }

  source(stream: MediaStream) {
    this.reset();
    this.stream = stream;
 if (Date.now()<this.demandUntil) this.attachVideo();
    for (const t of stream.getTracks()) {
      t.addEventListener("mute", () => this.sync());
      t.addEventListener("ended", () => this.sync());
    }

  }

  private allowed() {
    return captureAllowed(this.gate(), this.stream?.getVideoTracks()[0]);
  }

  /** Called synchronously from the video's latch hook, before any await. */
  sync() {
    const allowed = this.allowed();
    if (!allowed && this.open) this.reset();
    this.open = allowed;
    if (Date.now() >= this.demandUntil) this.idle();
    else if (allowed) this.attachVideo();
  }

  demand(until: number) {
    this.demandUntil = Math.max(0, Math.min(until, Date.now() + 90_000));
    this.sync();
    if (!this.allowed()) {this.lastMarker="";this.marker("paused");}
  }
  private attachVideo() {
    if (this.video.srcObject === this.stream) return;
    this.video.srcObject = this.stream ?? null;
    void this.video.play().catch(() => undefined);
  }
  private clearPicture() {
    this.generation++; this.previous = undefined;
    this.canvas.width = this.canvas.height = 0;
  }
  private marker(status: string) {
    this.clearPicture();
    if (this.lastMarker !== status) { this.lastMarker = status; this.send({ type: "capture", status }); }
  }
  private idle() {
    if (this.video.srcObject || this.previous) {
      this.video.pause(); this.video.srcObject = null;
      this.marker("idle");
    }
  }
  reset() {
    this.open = false;
    this.clearPicture();
 this.video.pause();this.video.srcObject=null;
    this.recordGeneration++;
    this.chunks = [];
    this.recordBytes = 0;
    this.completed = false;
    clearTimeout(this.recordTimer);
    if (this.recorder?.state !== "inactive") this.recorder?.stop();
    this.lastMarker="";this.marker("paused");
  }

  private async sample() {
    if (Date.now()>=this.demandUntil || !this.open || !this.allowed() || this.busy || this.video.readyState < 2) return;
    const width = this.video.videoWidth, height = this.video.videoHeight;
    if (!width || !height || width > 3840 || height > 2160 || width * height > MAX_PIXELS) { this.error = "capture_too_large";this.marker("too_large"); return; }
    this.busy = true;
    const generation = this.generation;
    try {
      if (this.canvas.width !== width || this.canvas.height !== height) {
        this.canvas.width = width;
        this.canvas.height = height;
        this.previous = undefined;
      }
      const ctx = this.canvas.getContext("2d", { willReadFrequently: true });
      if (!ctx) return;
      ctx.drawImage(this.video, 0, 0);
      this.metrics.readbacks++;
      const pixels = ctx.getImageData(0, 0, width, height).data;
      if (!pictureChanged(this.previous, pixels)) return;
      this.metrics.encodes++;
      const blob = await new Promise<Blob | null>((resolve) => this.canvas.toBlob(resolve, "image/png"));
      if (generation !== this.generation || !this.allowed() || Date.now()>=this.demandUntil) return;
      if (!blob || blob.size > MAX_FRAME) { this.error = "capture_too_large";this.marker("too_large"); return; }
      const data = base64(new Uint8Array(await blob.arrayBuffer()));
      // Pause, recapture, disconnect or supersede can happen during PNG encoding.
      if (generation !== this.generation || !this.allowed() || Date.now()>=this.demandUntil) return;
      if (await this.sendArtifact({ type: "capture", status: "frame", id: `${this.prefix}.${++this.seq}`, width, height, data }) && generation === this.generation && this.allowed()) {
        this.previous = pixels;this.lastMarker="frame";
        this.error = "";
      }
    } catch { this.error = "capture_failed";if(generation===this.generation&&this.allowed())this.marker("stale"); }
    finally { this.busy = false; }
  }

  async command(op: string, args: Record<string, unknown>): Promise<Result> {
    this.sync();
    if (op === "record.status") return { ok: true, data: this.status() };
    if (op === "record.start") {
      if (!this.allowed()) return fail("paused");
      if (this.recorder && this.recorder.state !== "inactive") return fail("conflict", "recording already started");
      const duration = args.duration ?? 120, limit = args.max_bytes ?? MAX_RECORDING;
      if (Object.keys(args).some((k) => !["duration", "max_bytes"].includes(k)) || !Number.isInteger(duration) || Number(duration) < 1 || Number(duration) > 120 || !Number.isInteger(limit) || Number(limit) < 1024 || Number(limit) > MAX_RECORDING) return fail("invalid_argument");
      const mime = ["video/webm;codecs=vp9,opus", "video/webm;codecs=vp8,opus", "video/webm"].find((t) => typeof MediaRecorder !== "undefined" && MediaRecorder.isTypeSupported(t));
      if (!mime) return fail("not_supported", "WebM recording is unavailable");
      this.chunks = []; this.recordBytes = 0; this.completed = false; this.error = "";
      this.recordLimit = Number(limit);
      const generation = ++this.recordGeneration;
      const recorder = new MediaRecorder(this.stream!, { mimeType: mime, videoBitsPerSecond: 2_000_000 });
      this.recorder = recorder;
      this.recordDone = new Promise((resolve) => { this.recordResolve = resolve; });
      recorder.ondataavailable = (e) => {
        if (generation !== this.recordGeneration || !this.allowed()) return;
        if (this.recordBytes + e.data.size > this.recordLimit) {
          this.recordGeneration++; this.chunks = []; this.recordBytes = 0; this.error = "recording_too_large";
          if (recorder.state !== "inactive") recorder.stop();
          return;
        }
        this.recordBytes += e.data.size;
        this.chunks.push(e.data);
      };
      recorder.onerror = () => { this.error = "recording_failed"; this.reset(); };
      recorder.onstop = () => { void this.finishRecording(generation); };
      this.recordStarted=performance.now();
      recorder.start(250);
      this.recordTimer = setTimeout(() => { if (recorder.state !== "inactive") recorder.stop(); }, Number(duration) * 1000);
      return { ok: true, data: this.status() };
    }
    if (op === "record.stop") {
      if (!this.recorder) return fail("not_found", "no recording");
      if (this.recorder.state !== "inactive") this.recorder.stop();
      await this.recordDone;
      return this.error ? fail(this.error) : { ok: true, data: this.status() };
    }
    return fail("invalid_argument");
  }

  private async finishRecording(generation: number) {
    const resolve = this.recordResolve;
    try {
      clearTimeout(this.recordTimer);
      if (generation !== this.recordGeneration || !this.allowed()) return;
      const blob = new Blob(this.chunks, { type: "video/webm" });
      this.chunks = [];
      if (blob.size === 0 || blob.size > this.recordLimit) { this.error = "recording_failed"; return; }
      const id = crypto.randomUUID();
      const duration_ms=Math.min(120000,Math.round(performance.now()-this.recordStarted));
      for (let start = 0; start < blob.size; start += 512 << 10) {
        const part = new Uint8Array(await blob.slice(start, start + (512 << 10)).arrayBuffer());
        if (generation !== this.recordGeneration || !this.allowed()) return;
        if (!await this.sendArtifact({ type: "recording", id, duration_ms, data: base64(part), final: start + part.length === blob.size })) { this.error = "sender_offline"; return; }
      }
      this.completed = true;
    } finally { resolve?.(); }
  }

  status(): Record<string, unknown> {
    return { recording: this.recorder?.state === "recording", completed: this.completed, bytes: this.recordBytes, error: this.error };
  }

  stop() {
    this.reset();
    clearInterval(this.timer);
    this.video.srcObject = null;this.stream=undefined;
  }
}
