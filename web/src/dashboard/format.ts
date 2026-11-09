import type { Access, Connection, Geo, PinPolicy, Stream, Viewer } from "./types";

export type Tone = "ok" | "warn" | "stamp" | "mute" | "ink";

/** "<1m", "5m", "1h 2m", "3d 4h". */
export function durationShort(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 60000));
  const d = Math.floor(total / 1440),
    h = Math.floor((total % 1440) / 60),
    m = total % 60;
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return "<1m";
}

/** "0:42", "12:03", "1:02:03". */
export function clock(ms: number): string {
  const t = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(t / 3600),
    m = Math.floor((t % 3600) / 60),
    s = t % 60;
  const ss = String(s).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

export function ago(at: number, now: number): string {
  const d = now - at;
  if (d < 60_000) return "just now";
  return `${durationShort(d)} ago`;
}

export function dateTime(at: number): string {
  return new Date(at).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function pinSummary(p: Access["pin"] | PinPolicy | null | undefined): {
  text: string;
  tone: Tone;
} {
  if (!p) return { text: "PIN unknown", tone: "mute" };
  if (p.kind === "none" || ("required" in p && !p.required))
    return { text: "No PIN", tone: "warn" };
  if (p.kind === "passphrase") return { text: "Passphrase", tone: "ok" };
  if (p.kind === "digits")
    return { text: p.length ? `PIN · ${p.length} digits` : "PIN · digits", tone: "ok" };
  return { text: "PIN unknown", tone: "mute" };
}

export function phaseOf(s: Stream): { text: string; tone: Tone; live: boolean } {
  const paused = s.effective?.access.latches.paused;
  switch (s.phase) {
    case "live":
      return paused
        ? { text: "Paused", tone: "warn", live: true }
        : { text: "Live", tone: "stamp", live: true };
    case "paused":
      return { text: "Paused", tone: "warn", live: true };
    case "picking":
      return { text: "Picking a screen", tone: "mute", live: true };
    case "starting":
      return { text: "Starting", tone: "mute", live: true };
    case "pending":
      return { text: "Waiting for sender", tone: "mute", live: true };
    case "ended":
      return { text: "Ended", tone: "mute", live: false };
    default:
      return { text: "Unknown", tone: "mute", live: s.ended_at === null };
  }
}

export function endReason(r: string | null): string {
  switch (r) {
    case "stopped":
      return "Stopped";
    case "window_closed":
      return "Sender window closed";
    case "expired":
      return "Expired";
    case "deleted":
      return "Deleted";
    case "state_lost":
      return "Sender state lost";
    case null:
      return "";
    default:
      return r;
  }
}

/** 0–4 bars from server-observed RTT and loss; -1 without stats. */
export function signalBars(stats: Connection["stats"] | null | undefined): number {
  if (!stats || (stats.rtt_ms === null && stats.loss_pct === null)) return -1;
  const rtt = stats.rtt_ms ?? 0,
    loss = stats.loss_pct ?? 0;
  if (rtt <= 80 && loss <= 1) return 4;
  if (rtt <= 150 && loss <= 3) return 3;
  if (rtt <= 300 && loss <= 8) return 2;
  if (rtt <= 1000 && loss <= 25) return 1;
  return 0;
}

/** Regional-indicator flag for an ISO 3166-1 alpha-2 code. */
export function flag(country: string | null | undefined): string {
  if (!country || !/^[A-Za-z]{2}$/.test(country)) return "";
  return String.fromCodePoint(
    ...[...country.toUpperCase()].map((c) => 0x1f1a5 + c.charCodeAt(0)),
  );
}

export function place(geo: Geo | null | undefined): string {
  if (!geo) return "";
  return [geo.city, geo.country].filter(Boolean).join(", ");
}

export function pathText(p: Connection["path"]): string {
  switch (p) {
    case "udp":
      return "direct UDP";
    case "tcp":
      return "direct TCP";
    case "turn_udp":
      return "relay (TURN/UDP)";
    case "turn_tcp":
      return "relay (TURN/TCP)";
    case "turn_tls":
      return "relay (TURN/TLS)";
    default:
      return "unknown";
  }
}

export function viewerName(v: Viewer): string {
  return v.connections[0]?.reported.label ?? "";
}

export function rowTitle(v: Viewer): string {
  const n = viewerName(v);
  if (n) return n;
  if (v.status === "joining") return "Joining…";
  return v.row_code ? `Viewer ${v.row_code}` : "Viewer";
}

export function statusOf(v: Viewer): { text: string; tone: Tone; dot: string } {
  switch (v.status) {
    case "watching":
      return { text: "Watching", tone: "ok", dot: "bg-ok" };
    case "reconnecting":
      return { text: "Reconnecting", tone: "warn", dot: "bg-warn" };
    case "knocking":
      return { text: "Knocking", tone: "stamp", dot: "bg-stamp" };
    case "joining":
      return { text: "Joining", tone: "mute", dot: "bg-ink-faint" };
    case "left":
      return { text: "Left", tone: "mute", dot: "bg-paper-dark" };
    default:
      return { text: "Unknown", tone: "mute", dot: "bg-ink-faint" };
  }
}

export function watchTime(v: Viewer, now: number): string {
  if (!v.sender?.admitted_at) return "";
  const end = v.status === "left" && v.left_at ? v.left_at : now;
  return durationShort(end - v.sender.admitted_at);
}

/** How long a stream has before it expires, or "" if it doesn't. */
export function expiry(s: Stream, now: number): string {
  if (!s.expires_at || s.ended_at) return "";
  const d = s.expires_at - now;
  return d <= 0 ? "expiring" : `expires in ${durationShort(d)}`;
}

export function kbps(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  return n >= 1000 ? `${(n / 1000).toFixed(1)} Mb/s` : `${n} kb/s`;
}
