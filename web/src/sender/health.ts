import type { Quality } from "../session";

type Capture = { getVideoTracks(): Pick<MediaStreamTrack, "getSettings">[] };
const samples = new WeakMap<Capture, { at: number; seconds: number; cpu: number | null }>();
const bounded = (v: unknown, max: number): number | null => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= max ? v : null;

/** Allowlisted capture/encoder metrics. CPU is encode time / elapsed sample time, not process CPU. */
export function senderHealth(capture: Capture | null, q?: Pick<Quality, "timestamp" | "bitrate" | "fps" | "stats">) {
  if (!capture) return null;
  const stats = q?.stats ?? {};
  const encoded = bounded(stats.framesEncoded, Number.MAX_SAFE_INTEGER) ?? 0;
  const seconds = bounded(stats.totalEncodeTime, Number.MAX_SAFE_INTEGER);
  const at = q?.timestamp ?? 0;
  const previous = samples.get(capture);
  let cpu = previous?.cpu ?? null;
  if (seconds !== null && at > 0) {
    if (previous && at > previous.at) cpu = seconds >= previous.seconds ? Math.min(100, (seconds - previous.seconds) * 100000 / (at - previous.at)) : null;
    if (!previous || at !== previous.at) samples.set(capture, { at, seconds, cpu });
  }
  const settings = capture.getVideoTracks()[0]?.getSettings();
  const reason = stats.qualityLimitationReason;
  return {
    uplink_kbps: q && q.bitrate > 0 ? bounded(Math.round(q.bitrate / 1000), Number.MAX_SAFE_INTEGER) : null,
    encode_ms: encoded > 0 && seconds !== null ? bounded(Math.round(seconds / encoded * 10000) / 10, 60000) : null,
    encode_cpu_pct: cpu,
    fps: q && q.fps > 0 ? bounded(q.fps, 1000) : null,
    width: settings?.width && settings.width > 0 ? bounded(Math.max(1, Math.round(settings.width)), 16384) : null,
    height: settings?.height && settings.height > 0 ? bounded(Math.max(1, Math.round(settings.height)), 16384) : null,
    quality_limitation: reason === undefined ? "none" : ["none", "cpu", "bandwidth"].includes(String(reason)) ? String(reason) : "other",
  };
}
