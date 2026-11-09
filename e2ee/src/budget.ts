/**
 * Sender-side PIN guess budget.
 *
 * Every PIN handshake *reserves* a guess before the sender emits its CPace
 * share, and the reservation is persisted first. A reservation is released
 * only by a successful key confirmation. Wrong PINs, invalid shares, aborted
 * and timed-out handshakes all settle as failures, and a reservation found
 * stale after a crash or reload is counted as a failure too. So abandoning a
 * handshake never refunds a guess, and parallel handshakes cannot overshoot
 * the limits because reservations count against them.
 *
 * Limits (defaults from docs/PLAN.md):
 * - 5 failures in one viewer session → that session waits 60 s, doubling on
 *   every further block of 5 (60, 120, 240 s …).
 * - 5 failures on the stream within 10 min → stream-wide pacing: every
 *   further PIN attempt, from any session, starts at least 2^(n−5) s after
 *   the previous attempt (n failures in the window, plus attempts in flight),
 *   capped at 60 s. It doesn't depend on the viewer-chosen session id.
 * - 20 failures on the stream → new PIN admissions freeze until the owner
 *   unlocks or rotates the PIN. Admitted viewers and grant re-admissions are
 *   unaffected (they don't spend guesses). Unlocking also clears the pacing.
 *
 * The viewer session id is chosen by the viewer, so the per-session backoff
 * only slows honest-but-confused viewers. Rotating ids gains nothing against
 * the stream-wide pacing, and the per-stream freeze is the hard bound on an
 * attacker (20 guesses at a 4-digit PIN ≈ 0.2 %).
 */
import { toBase64Url, randomBytes } from "./bytes.ts";
import { Emitter } from "./events.ts";
import type { AtomicStorage, Json } from "./storage.ts";

export interface BudgetLimits {
  perSession: number;
  perStream: number;
  backoffBaseMs: number;
  handshakeTimeoutMs: number;
  /** Stream-wide failures within `paceWindowMs` before pacing starts. */
  paceAfter: number;
  paceWindowMs: number;
  paceBaseMs: number;
  paceMaxMs: number;
}

export const DEFAULT_LIMITS: BudgetLimits = {
  perSession: 5,
  perStream: 20,
  backoffBaseMs: 60_000,
  handshakeTimeoutMs: 30_000,
  paceAfter: 5,
  paceWindowMs: 600_000,
  paceBaseMs: 1_000,
  paceMaxMs: 60_000,
};

/** Recent failure times kept for pacing (more than perStream is never needed). */
const MAX_RECENT = 64;

export type FailureOutcome = "wrong_pin" | "invalid" | "abandoned" | "timeout";
export type SettleOutcome = "success" | FailureOutcome;

export interface BudgetEvents extends Record<string, unknown> {
  failure: { viewerSid: string; outcome: FailureOutcome; sessionFailures: number; streamFailures: number; at: number };
  backoff: { viewerSid: string; until: number; durationMs: number };
  frozen: { streamFailures: number; at: number };
  unlocked: { at: number };
  success: { viewerSid: string; at: number };
}

interface SessionState {
  window: number; // failures since the last backoff
  level: number; // backoffs so far
  lockedUntil: number;
}
interface TicketState {
  sid: string;
  at: number;
}
interface State {
  v: 1;
  streamFailures: number;
  frozen: boolean;
  sessions: Record<string, SessionState>;
  tickets: Record<string, TicketState>;
  /** Stream-wide failure times within the pacing window, oldest first. */
  recent: number[];
}

export type Reservation =
  | { ok: true; ticket: string; triesLeft: number }
  | { ok: false; reason: "frozen" | "backoff"; retryAfterMs: number; triesLeft: number };

export interface SettleResult {
  triesLeft: number;
  retryAfterMs: number;
  frozen: boolean;
}

export interface BudgetStatus {
  streamFailures: number;
  frozen: boolean;
  inflight: number;
}

function isNum(x: unknown): x is number {
  return typeof x === "number" && Number.isFinite(x);
}

function isObj(x: Json | undefined): x is { [k: string]: Json } {
  return typeof x === "object" && x !== null && !Array.isArray(x);
}

export class GuessBudget extends Emitter<BudgetEvents> {
  readonly limits: BudgetLimits;
  readonly #storage: AtomicStorage;
  readonly #key: string;
  readonly #now: () => number;

  constructor(opts: { storage: AtomicStorage; streamId: string; now?: () => number; limits?: Partial<BudgetLimits> }) {
    super();
    this.#storage = opts.storage;
    this.#key = `budget:${opts.streamId}`;
    this.#now = opts.now ?? Date.now;
    this.limits = { ...DEFAULT_LIMITS, ...opts.limits };
  }

  /** Corrupt or foreign state fails closed: treated as frozen. */
  #load(j: Json | undefined): State {
    if (j === undefined) return { v: 1, streamFailures: 0, frozen: false, sessions: {}, tickets: {}, recent: [] };
    const frozenState: State = { v: 1, streamFailures: this.limits.perStream, frozen: true, sessions: {}, tickets: {}, recent: [] };
    if (!isObj(j) || j.v !== 1 || !isNum(j.streamFailures) || typeof j.frozen !== "boolean") return frozenState;
    if (!isObj(j.sessions) || !isObj(j.tickets)) return frozenState;
    const sessions: Record<string, SessionState> = {};
    for (const [k, s] of Object.entries(j.sessions)) {
      if (!isObj(s) || !isNum(s.window) || !isNum(s.level) || !isNum(s.lockedUntil)) return frozenState;
      sessions[k] = { window: s.window, level: s.level, lockedUntil: s.lockedUntil };
    }
    const tickets: Record<string, TicketState> = {};
    for (const [k, t] of Object.entries(j.tickets)) {
      if (!isObj(t) || typeof t.sid !== "string" || !isNum(t.at)) return frozenState;
      tickets[k] = { sid: t.sid, at: t.at };
    }
    // State saved before pacing existed has no `recent`; a malformed one fails closed.
    const recent: number[] = [];
    if (j.recent !== undefined) {
      if (!Array.isArray(j.recent)) return frozenState;
      for (const t of j.recent) {
        if (!isNum(t)) return frozenState;
        recent.push(t);
      }
    }
    return { v: 1, streamFailures: j.streamFailures, frozen: j.frozen, sessions, tickets, recent };
  }

  #toJson(s: State): Json {
    return {
      v: 1,
      streamFailures: s.streamFailures,
      frozen: s.frozen,
      sessions: Object.fromEntries(Object.entries(s.sessions).map(([k, v]) => [k, { ...v }])),
      tickets: Object.fromEntries(Object.entries(s.tickets).map(([k, v]) => [k, { ...v }])),
      recent: [...s.recent],
    };
  }

  /**
   * When the next PIN attempt may start, stream-wide. Attempts in flight count
   * as failures-to-be and as the latest start, so parallel handshakes don't
   * slip in together when a wait ends. 0 while fewer than `paceAfter`.
   */
  #paceReadyAt(s: State): number {
    const tickets = Object.values(s.tickets);
    const n = s.recent.length + tickets.length;
    if (n < this.limits.paceAfter) return 0;
    const delay = Math.min(this.limits.paceMaxMs, this.limits.paceBaseMs * 2 ** (n - this.limits.paceAfter));
    const last = Math.max(s.recent.at(-1) ?? 0, ...tickets.map((t) => t.at));
    return last + delay;
  }

  /** Apply one failure to state; returns events to emit after commit. */
  #fail(s: State, sid: string, outcome: FailureOutcome, now: number, events: Array<() => void>): void {
    const sess = (s.sessions[sid] ??= { window: 0, level: 0, lockedUntil: 0 });
    sess.window += 1;
    s.streamFailures += 1;
    s.recent.push(now);
    if (s.recent.length > MAX_RECENT) s.recent.splice(0, s.recent.length - MAX_RECENT);
    const sessionFailures = sess.window;
    const streamFailures = s.streamFailures;
    events.push(() => this.emit("failure", { viewerSid: sid, outcome, sessionFailures, streamFailures, at: now }));
    if (sess.window >= this.limits.perSession) {
      const durationMs = this.limits.backoffBaseMs * 2 ** sess.level;
      sess.lockedUntil = now + durationMs;
      sess.level += 1;
      sess.window = 0;
      const until = sess.lockedUntil;
      events.push(() => this.emit("backoff", { viewerSid: sid, until, durationMs }));
    }
    if (!s.frozen && s.streamFailures >= this.limits.perStream) {
      s.frozen = true;
      events.push(() => this.emit("frozen", { streamFailures, at: now }));
    }
  }

  /** Convert reservations older than the handshake timeout into failures. */
  #sweep(s: State, now: number, events: Array<() => void>): void {
    for (const [id, t] of Object.entries(s.tickets)) {
      if (now - t.at >= this.limits.handshakeTimeoutMs) {
        delete s.tickets[id];
        this.#fail(s, t.sid, "timeout", now, events);
      }
    }
    s.recent = s.recent.filter((t) => now - t < this.limits.paceWindowMs);
    // Forget idle sessions so attacker-chosen ids don't grow state forever.
    for (const [sid, sess] of Object.entries(s.sessions)) {
      if (sess.window === 0 && sess.lockedUntil <= now && !Object.values(s.tickets).some((t) => t.sid === sid)) {
        // Keep the backoff level for a while after a lock so repeated blocks still double.
        if (sess.level === 0 || now - sess.lockedUntil > 24 * 3600_000) delete s.sessions[sid];
      }
    }
  }

  #triesLeft(s: State, sid: string): number {
    const inflightSess = Object.values(s.tickets).filter((t) => t.sid === sid).length;
    const inflight = Object.keys(s.tickets).length;
    const sess = s.sessions[sid];
    const a = this.limits.perSession - (sess?.window ?? 0) - inflightSess;
    const b = this.limits.perStream - s.streamFailures - inflight;
    return Math.max(0, Math.min(a, b));
  }

  async #mutate<T>(fn: (s: State, now: number, events: Array<() => void>) => T): Promise<T> {
    let result!: T;
    let events: Array<() => void> = [];
    await this.#storage.update(this.#key, (cur) => {
      events = [];
      const now = this.#now();
      const s = this.#load(cur);
      this.#sweep(s, now, events);
      result = fn(s, now, events);
      return this.#toJson(s);
    });
    for (const e of events) e();
    return result;
  }

  /** Reserve one guess for `viewerSid`. Persisted before resolving. */
  reserve(viewerSid: string): Promise<Reservation> {
    return this.#mutate((s, now): Reservation => {
      const sess = s.sessions[viewerSid];
      if (s.frozen) return { ok: false, reason: "frozen", retryAfterMs: 0, triesLeft: 0 };
      if (sess && sess.lockedUntil > now) {
        return { ok: false, reason: "backoff", retryAfterMs: sess.lockedUntil - now, triesLeft: 0 };
      }
      const ready = this.#paceReadyAt(s);
      if (ready > now) return { ok: false, reason: "backoff", retryAfterMs: ready - now, triesLeft: 0 };
      if (this.#triesLeft(s, viewerSid) <= 0) {
        // Every remaining guess is reserved by an in-flight handshake.
        const oldest = Math.min(...Object.values(s.tickets).map((t) => t.at));
        const retryAfterMs = Number.isFinite(oldest) ? Math.max(0, oldest + this.limits.handshakeTimeoutMs - now) : 0;
        return { ok: false, reason: "backoff", retryAfterMs, triesLeft: 0 };
      }
      const ticket = toBase64Url(randomBytes(12));
      s.tickets[ticket] = { sid: viewerSid, at: now };
      return { ok: true, ticket, triesLeft: this.#triesLeft(s, viewerSid) };
    });
  }

  /**
   * Settle a reservation. Unknown tickets (already swept as timed out) are a
   * no-op apart from reporting the current state.
   */
  settle(ticket: string, outcome: SettleOutcome): Promise<SettleResult> {
    return this.#mutate((s, now, events): SettleResult => {
      const t = s.tickets[ticket];
      const sid = t?.sid ?? "";
      if (t) {
        delete s.tickets[ticket];
        if (outcome === "success") {
          // A correct PIN clears that session's history (typo then success).
          delete s.sessions[sid];
          events.push(() => this.emit("success", { viewerSid: sid, at: now }));
        } else {
          this.#fail(s, sid, outcome, now, events);
        }
      }
      const sess = s.sessions[sid];
      const locked = sess !== undefined && sess.lockedUntil > now;
      // A failure tells the viewer how long the next try must wait: its own
      // session's backoff, or the stream-wide pacing, whichever is longer.
      const paced = outcome === "success" || s.frozen ? 0 : Math.max(0, this.#paceReadyAt(s) - now);
      const wait = Math.max(locked ? sess.lockedUntil - now : 0, paced);
      return {
        // While backing off there are no tries *now*; retryAfterMs says when.
        triesLeft: t && wait === 0 && !s.frozen ? this.#triesLeft(s, sid) : 0,
        retryAfterMs: wait,
        frozen: s.frozen,
      };
    });
  }

  /** Read the durable freeze without mutating reservations (startup inspection). */
  async inspect(): Promise<BudgetStatus> {
    const s = this.#load(await this.#storage.get(this.#key));
    return { streamFailures: s.streamFailures, frozen: s.frozen, inflight: Object.keys(s.tickets).length };
  }

  /** Sweep timeouts and report. Call periodically so abandoned handshakes surface promptly. */
  status(): Promise<BudgetStatus> {
    return this.#mutate((s) => ({ streamFailures: s.streamFailures, frozen: s.frozen, inflight: Object.keys(s.tickets).length }));
  }

  /** Owner action: unfreeze and forget all failures and the pacing (also used on PIN rotation). */
  unlock(): Promise<void> {
    return this.#mutate((s, now, events) => {
      s.streamFailures = 0;
      s.frozen = false;
      s.sessions = {};
      s.recent = [];
      // Outstanding reservations are void: settling one later is a no-op, so a
      // held handshake can neither pace newcomers nor time out into a failure.
      s.tickets = {};
      events.push(() => this.emit("unlocked", { at: now }));
    });
  }
}
