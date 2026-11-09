/**
 * The live quality HUD's model (FEATURES.md "Track D2" §1): pure mapping from
 * state the app already has (the session's own getStats sample, the rendered
 * frame timeline, the E2EE worker's reports) to what the HUD shows. Nothing
 * here polls the network, and HudSampler caps recomputation at 2 Hz.
 */

/** Which rendition a viewer is getting and its temporal layer. */
export interface MediaLayer {
  rendition: "native" | "low" | null;
  /** Current temporal layer, 1-based (T1 = base). */
  temporal: number | null;
  temporalMax: number | null;
}

/**
 * Seam for the media controller (track A1): whatever decides the viewer's
 * rendition and layer can implement this. The default reads the layer the
 * SFU reports on the heartbeat (`Session.quality.layer`).
 */
export interface MediaLayerSource {
  layer(): MediaLayer;
}

/** The publisher encodes L1T3 (PLAN "Media"): three temporal layers. */
export const PUBLISHED_TEMPORAL_LAYERS = 3;

/**
 * `Session.quality.layer` as the heartbeat writes it: `L1T3` (native, spatial
 * 1, temporal 3), `180p/L1T2` (the 320×180 low rendition), or the initial
 * `L1T3 requested` before any report.
 */
export function parseLayer(s: string | null | undefined): MediaLayer {
  const none: MediaLayer = { rendition: null, temporal: null, temporalMax: null };
  if (!s || /requested/.test(s)) return none;
  const m = /^(180p\/)?L(\d+)T(\d+)$/.exec(s.trim());
  if (!m) return none;
  const temporal = Number(m[3]);
  if (!Number.isInteger(temporal) || temporal < 1 || temporal > 8) return none;
  return { rendition: m[1] ? "low" : "native", temporal, temporalMax: PUBLISHED_TEMPORAL_LAYERS };
}

export class SessionLayerSource implements MediaLayerSource {
  constructor(private readonly read: () => string | null | undefined) {}
  layer(): MediaLayer {
    return parseLayer(this.read());
  }
}

export type PathLabel = "UDP" | "TCP" | "TURN/UDP" | "TURN/TCP" | "TURN/TLS";

/**
 * The selected candidate pair as `Session.quality.path` records it:
 * `"TURN/" + relayProtocol` when either end is a relay candidate, else the
 * local candidate's protocol, upper-cased. "—" (or anything else) is unknown.
 */
export function pathLabel(path: string | null | undefined): PathLabel | null {
  const p = (path ?? "").trim().toUpperCase();
  if (p === "UDP" || p === "TCP" || p === "TURN/UDP" || p === "TURN/TCP" || p === "TURN/TLS") return p;
  return null;
}

/** A gap between rendered frames longer than this is a render gap; with evidence of input, a freeze. */
export const FREEZE_GAP_MS = 500;

/** One HUD-rate sample of what was happening while frames did or didn't render. */
export interface ActivitySample {
  at: number;
  /** Frames the SFU received from the publisher (heartbeat `ingressFrames`). */
  ingress: number;
  /** RTP packets this viewer received (inbound-rtp `packetsReceived`). */
  packets: number;
  /** The host's curtain was showing (no new content is expected). */
  paused: boolean;
  /** This tab was hidden (the browser may stop rendering). */
  hidden: boolean;
}

/**
 * Gaps over 500 ms between rendered frames, in a timeline that only grows at
 * the end and may drop entries at the start (each call scans only new ones).
 *
 * - A gap while paused or hidden (any sample in or just before it) is ignored.
 * - Otherwise it's a **render gap**.
 * - It's a **freeze** only with evidence that the source or input advanced
 *   *during* the gap: two activity samples inside it, with SFU ingress frames
 *   or received packets increasing between them. A static screen produces no
 *   frames, so its gaps stay render gaps. A timestamp change at the next
 *   frame proves nothing, so it isn't used.
 */
export class FreezeCounter {
  freezes = 0;
  renderGaps = 0;
  /** Kept for compatibility: the confirmed freezes. */
  get count() {
    return this.freezes;
  }
  private lastAt = 0;
  private samples: ActivitySample[] = [];

  /** Record what was happening now (call at the HUD rate, frames or not). */
  sample(s: ActivitySample) {
    this.samples.push(s);
    // Two minutes of 2 Hz samples is plenty for any gap we classify.
    if (this.samples.length > 240) this.samples.splice(0, this.samples.length - 240);
  }

  private classify(from: number, to: number) {
    const before = [...this.samples].reverse().find((x) => x.at <= from);
    const inside = this.samples.filter((x) => x.at >= from && x.at <= to);
    if ((before && (before.paused || before.hidden)) || inside.some((x) => x.paused || x.hidden)) return;
    this.renderGaps++;
    for (let i = 0; i < inside.length; i++)
      for (let j = i + 1; j < inside.length; j++)
        if (inside[j]!.ingress > inside[i]!.ingress || inside[j]!.packets > inside[i]!.packets) {
          this.freezes++;
          return;
        }
  }

  observe(timeline: readonly { at: number }[]): number {
    let i = timeline.length;
    while (i > 0 && timeline[i - 1]!.at > this.lastAt) i--;
    for (; i < timeline.length; i++) {
      const at = timeline[i]!.at;
      if (this.lastAt > 0 && at - this.lastAt > FREEZE_GAP_MS) this.classify(this.lastAt, at);
      this.lastAt = at;
    }
    return this.freezes;
  }
}

/** The newest decoded clock-row delay, if the newest frame carried one recently. */
export function latestG2G(timeline: readonly { at: number; delay: number | null }[], now: number, maxAgeMs = 2000): number | null {
  const last = timeline.at(-1);
  if (!last || last.delay === null || now - last.at > maxAgeMs) return null;
  return last.delay >= 0 && last.delay < 60_000 ? Math.round(last.delay) : null;
}

export interface E2EEHud {
  /** Newest epoch handed to the decrypt worker. */
  epoch: number | null;
  /** Frames the worker dropped, all reasons. */
  failures: number;
  /** The most frequent drop reason, if any. */
  top: string | null;
}

export function e2eeHud(epoch: number | null, drops: Readonly<Record<string, number>>): E2EEHud {
  let failures = 0;
  let top: string | null = null;
  let topN = 0;
  for (const [reason, n] of Object.entries(drops)) {
    if (!Number.isFinite(n) || n <= 0) continue;
    failures += n;
    if (n > topN) (top = reason), (topN = n);
  }
  return { epoch, failures, top };
}

/** What the session sample offers (a subset of `Session.quality`). */
export interface SessionSample {
  rtt: number;
  loss: number;
  bitrate: number;
  path: string;
  fps: number;
  timestamp: number;
}

export interface ViewerHud {
  rtt_ms: number | null;
  loss_pct: number | null;
  bitrate_kbps: number | null;
  fps: number | null;
  path: PathLabel | null;
  rendition: MediaLayer["rendition"];
  /** e.g. "T3 of 3". */
  temporal: string | null;
  /** Confirmed freezes (see FreezeCounter). */
  freezes: number;
  /** All gaps over 500 ms outside pauses and hidden periods. */
  render_gaps: number;
  g2g_ms: number | null;
  e2ee: E2EEHud;
}

export function viewerHud(i: { sample: SessionSample | null; layer: MediaLayer; freezes: number; renderGaps?: number; g2g: number | null; e2ee: E2EEHud }): ViewerHud {
  const q = i.sample && i.sample.timestamp ? i.sample : null;
  const num = (v: number | undefined, ok: (v: number) => boolean) => (q && v !== undefined && Number.isFinite(v) && ok(v) ? v : null);
  const bitrate = num(q?.bitrate, (v) => v > 0);
  const path = pathLabel(q?.path);
  return {
    // RTT and path come from the same selected candidate pair: with a pair, 0 is a real (sub-millisecond) RTT.
    rtt_ms: path === null || num(q?.rtt, (v) => v >= 0) === null ? null : Math.round(q!.rtt),
    loss_pct: num(q?.loss, (v) => v >= 0 && v <= 100),
    bitrate_kbps: bitrate === null ? null : Math.round(bitrate / 1000),
    fps: num(q?.fps, (v) => v > 0) === null ? null : Math.round(q!.fps),
    path,
    rendition: i.layer.rendition,
    temporal: i.layer.temporal === null ? null : `T${i.layer.temporal}${i.layer.temporalMax ? ` of ${i.layer.temporalMax}` : ""}`,
    freezes: i.freezes,
    render_gaps: i.renderGaps ?? i.freezes,
    g2g_ms: i.g2g,
    e2ee: i.e2ee,
  };
}

/** Sender health as `senderHealth()` reports it in `SenderState.health`. */
export interface SenderHealth {
  uplink_kbps: number | null;
  encode_ms: number | null;
  encode_cpu_pct: number | null;
  fps: number | null;
  width: number | null;
  height: number | null;
  quality_limitation: string;
}

export interface SenderAudio {
  /** The capture has an audio track that is still live. */
  present: boolean;
  /** Muted by the curtain (pause). */
  muted: boolean;
  kbps: number | null;
}

export interface SenderHud {
  uplink_kbps: number | null;
  encode_ms: number | null;
  encode_cpu_pct: number | null;
  quality_limitation: string;
  fps: number | null;
  resolution: string | null;
  audio: string;
}

export function senderHud(h: SenderHealth | null, audio: SenderAudio): SenderHud {
  return {
    uplink_kbps: h?.uplink_kbps ?? null,
    encode_ms: h?.encode_ms ?? null,
    encode_cpu_pct: h?.encode_cpu_pct === null || h?.encode_cpu_pct === undefined ? null : Math.round(h.encode_cpu_pct),
    quality_limitation: h?.quality_limitation ?? "none",
    fps: h?.fps === null || h?.fps === undefined ? null : Math.round(h.fps),
    resolution: h?.width && h?.height ? `${h.width}×${h.height}` : null,
    audio: !audio.present ? "none" : audio.muted ? "muted (curtain)" : audio.kbps !== null ? `on · ${audio.kbps} kb/s` : "on",
  };
}

/** Kilobits per second between two cumulative byte counters. */
export function kbps(prev: { bytes: number; at: number } | null, cur: { bytes: number; at: number }): number | null {
  if (!prev || cur.at <= prev.at || cur.bytes < prev.bytes) return null;
  return Math.round(((cur.bytes - prev.bytes) * 8) / (cur.at - prev.at));
}

/**
 * Recompute at most every `intervalMs` (default 500 ms, i.e. 2 Hz). Renders
 * in between get the cached model, so the HUD never adds work per render.
 */
export class HudSampler<T> {
  private value: T | undefined;
  private at = -Infinity;
  constructor(
    private readonly compute: () => T,
    private readonly intervalMs = 500,
    private readonly now: () => number = () => performance.now(),
  ) {}
  get(): T {
    const t = this.now();
    if (this.value === undefined || t - this.at >= this.intervalMs) {
      this.value = this.compute();
      this.at = t;
    }
    return this.value;
  }
}
