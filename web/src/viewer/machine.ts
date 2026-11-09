/**
 * The viewer's screen state machine (PLAN "Viewer UX"). A pure function of
 * a snapshot of the controller and its media session, plus one piece of
 * memory: when the current connection trouble started. Every screen the
 * viewer can draw comes from here, so each transition is unit-tested.
 *
 *   connecting ─▶ pin ⇄ (checking) ─▶ knock ─▶ admitted ─▶ live ⇄ paused
 *        │         │  └─▶ lockout ─┘     │                   │ ▲
 *        │         └─▶ waiting (host away before admission)  ▼ │
 *        │                                            reconnecting
 *   unsupported / invalid (before any network)   removed · ended (terminal)
 *
 * A short outage never shows: trouble shorter than RECONNECT_GRACE_MS keeps
 * the live screen (the last frame holds). After that a calm "Reconnecting"
 * badge, and after RECONNECT_LONG_MS a "still trying" line. Never an error.
 */
import type { PinPolicy } from "../viewer-ui/PinScreen";

export const RECONNECT_GRACE_MS = 2000;
export const RECONNECT_LONG_MS = 15_000;
export const CONNECT_SLOW_MS = 10_000;

export type RemovedReason = "kicked" | "locked" | "denied" | "frozen" | "full";

/** The controller's own view (what admission decided). */
export type View =
  | { kind: "invalid" }
  | { kind: "unsupported"; missing: string[] }
  | { kind: "pin"; policy: PinPolicy }
  | { kind: "waiting" }
  | { kind: "verifying" }
  | { kind: "knock"; code: string }
  | { kind: "media" }
  | { kind: "ended"; reason: string }
  | { kind: "removed"; reason: RemovedReason };

export interface Snapshot {
  view: View;
  /** The controller is checking a typed PIN with the host right now. */
  checkingPin: boolean;
  /** The PIN policy the PIN screen uses (the sender's hint, else 4 digits). */
  pinPolicy: PinPolicy;
  lockedUntil: number | null;
  /** Admitted and the host's sender is away (it reloaded or dropped). */
  hostAway: boolean;
  /** Admitted and re-admitting with the grant (sender back, missing epoch). */
  readmitting: boolean;
  /** The media session, once one exists. */
  session: {
    state: "waiting" | "connecting" | "live" | "reconnecting" | "ended";
    marker: "paused" | "ended" | null;
    firstFrame: number;
    hasStream: boolean;
    signalingOpen: boolean;
  } | null;
  /** When the page started connecting (ms, same clock as `now`). */
  startedAt: number;
  now: number;
}

export type Trouble = "network" | "host";

export type Screen =
  | { kind: "invalid" }
  | { kind: "unsupported"; missing: string[] }
  | { kind: "connecting"; slow: boolean }
  | { kind: "pin"; policy: PinPolicy; checking: boolean; lockout: boolean }
  | { kind: "waiting" }
  | { kind: "knock"; code: string }
  /** Admitted with keys, the first frame isn't decoded yet. */
  | { kind: "admitted"; slow: boolean }
  | { kind: "live" }
  | { kind: "paused" }
  | { kind: "reconnecting"; cause: Trouble; long: boolean }
  | { kind: "removed"; reason: RemovedReason }
  | { kind: "ended"; reason: string };

export type ScreenKind = Screen["kind"];

/** What's wrong with an admitted stream right now, if anything. */
export function trouble(s: Snapshot): Trouble | null {
  if (s.view.kind !== "media" || !s.session) return null;
  if (s.hostAway) return "host";
  const ss = s.session;
  if (ss.state === "reconnecting" || (ss.state === "connecting" && ss.firstFrame > 0)) return "network";
  if (!ss.signalingOpen && ss.firstFrame > 0) return "network";
  if (s.readmitting && ss.firstFrame > 0) return "network";
  return null;
}

/**
 * Remembers when the current trouble began. Call `update` on every render;
 * any healthy snapshot resets it.
 */
export class OutageClock {
  since: number | null = null;
  update(t: Trouble | null, now: number): number | null {
    if (!t) this.since = null;
    else if (this.since === null) this.since = now;
    return this.since;
  }
}

/** Screens nothing leaves: the page shows them until it is reloaded. */
export function isTerminal(k: ScreenKind | View["kind"]): boolean {
  return k === "removed" || k === "ended" || k === "invalid" || k === "unsupported";
}

/**
 * Holds the first terminal screen for good. A late callback (a queued video
 * frame, an admission answer that arrives after a close) can change the
 * snapshot, but there is no transition out of ended or removed. It latches on
 * what the controller and the session decided (a terminal view, or signaling
 * closed for good), not on the sender's in-picture "ended" marker alone, which
 * is a pixel hint that the terminal close confirms a moment later.
 */
export class TerminalLatch {
  held: Screen | null = null;
  hold(screen: Screen, s: Snapshot): Screen {
    if (this.held) return this.held;
    if (isTerminal(screen.kind) && (isTerminal(s.view.kind) || s.session?.state === "ended")) this.held = screen;
    return screen;
  }
}

export function screenFor(s: Snapshot, outageSince: number | null): Screen {
  const v = s.view;
  // The server closed signaling for good (the stream ended): from any live state.
  if (s.session?.state === "ended" && v.kind !== "invalid" && v.kind !== "unsupported" && v.kind !== "removed" && v.kind !== "ended") return { kind: "ended", reason: "stopped" };
  switch (v.kind) {
    case "invalid":
    case "unsupported":
    case "removed":
    case "ended":
      return v;
    case "pin":
      return { kind: "pin", policy: v.policy, checking: false, lockout: !!s.lockedUntil && s.now < s.lockedUntil };
    case "knock":
      return v;
    case "waiting":
      return { kind: "waiting" };
    case "verifying":
      // A typed PIN stays on the PIN screen ("Checking with the host…") until the answer.
      if (s.checkingPin) return { kind: "pin", policy: s.pinPolicy, checking: true, lockout: false };
      return { kind: "connecting", slow: s.now - s.startedAt > CONNECT_SLOW_MS };
    case "media":
      break;
  }
  const ss = s.session;
  // The sender's "Ended" picture arrives before the terminal signal.
  if (ss?.marker === "ended" || ss?.state === "ended") return { kind: "ended", reason: "stopped" };
  if (!ss?.hasStream || !ss.firstFrame) return { kind: "admitted", slow: s.now - s.startedAt > CONNECT_SLOW_MS };
  if (ss.marker === "paused") return { kind: "paused" };
  const t = trouble(s);
  if (!t || outageSince === null || s.now - outageSince < RECONNECT_GRACE_MS) return { kind: "live" };
  return { kind: "reconnecting", cause: t, long: s.now - outageSince >= RECONNECT_LONG_MS };
}
