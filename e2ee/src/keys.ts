/**
 * Sender-side epoch management.
 *
 * An epoch is 32 random bytes plus a monotonic number persisted per stream.
 * Rekey (on kick, PIN rotation, encryptor replacement, and every 10 minutes):
 *   1. allocate epoch e+1 and a fresh random secret,
 *   2. send it individually to every remaining admitted session over its
 *      PAKE-derived channel,
 *   3. switch encryption to e+1 when every session acked or the deadline
 *      passed, whichever is first, and ask the encoder for a keyframe.
 * The deadline is 300 ms for kicks and encryptor replacement, 1.5 s
 * otherwise. A rekey started while another is pending supersedes it but
 * keeps the earlier absolute deadline, so a stream of rekeys can never
 * postpone the switch. Kicked sessions are removed before step 2, so they
 * never receive e+1, and the sender drops their channel keys.
 *
 * Revocation cutoff: a kick is enforced independently of queued work. When
 * kick() (or beginRevocation()) is called, the kicked sessions are removed
 * and an absolute cutoff is armed synchronously: by `now + fastAckTimeoutMs`
 * the active epoch must be newer than every epoch allocated so far (those
 * are the epochs a kicked session may hold). tick() checks deadlines
 * synchronously, before it queues anything, so a long fan-out or a backlog
 * of kicks cannot hold the switch back. If no fresh epoch can be activated
 * by the cutoff (storage failure, a backlog that has not allocated one yet),
 * the sender fails closed: `onSuspend` stops encryption until the next
 * `onActivate`, and a fresh rekey is queued.
 *
 * An epoch a revoked session may hold (any epoch allocated before the kick
 * armed its cutoff) is never activated, even when its own deadline passes: it
 * is discarded and a fresh one takes its place.
 *
 * The same cutoff is also handed to the encryptor (`onCutoff`), which
 * enforces it on its own clock: past `within` without an epoch newer than
 * `stale`, the worker drops every frame by itself. The main thread can stall
 * (and tick() with it); the transform worker keeps running.
 *
 * Bounded recovery: while encryption is stopped, tick() retries recovery
 * (deliver a fresh epoch, or allocate one) with backoff, at most 2 s apart,
 * until it succeeds. Storage operations time out after `storageTimeoutMs`
 * (TimedStorage), so a hung write can't hold the queue and recovery with it.
 * `hold()` stops encryption on purpose (SenderHost: a revocation is not
 * durable yet); recovery starts when the hold is released.
 *
 * Encryption-key lifecycle (RFC 9605 §9.1, nonce uniqueness): an epoch key
 * reaches an encryptor exactly once, through `onActivate`. Each (epoch,
 * track) restarts its CTR at 0, so installing the same epoch in a second
 * encryptor (a recreated worker, a fresh SenderKeyRing) would reuse nonces.
 * The active secret is therefore not readable from this class. To replace an
 * encryptor, terminate the old one and call `replaceEncryptor()`: it
 * activates a fresh epoch, which `onActivate` delivers to the new encryptor.
 */
import { type Bytes, randomBytes } from "./bytes.ts";
import { type EpochKey, EPOCH_SECRET_BYTES, type SenderSession } from "./channel.ts";
import { E2EEError } from "./errors.ts";
import { Emitter } from "./events.ts";
import { type AtomicStorage, DEFAULT_STORAGE_TIMEOUT_MS, retryDelayMs, TimedStorage } from "./storage.ts";
import { type AnnotationRecord, ANNOTATION_MAX_EPOCHS, annotationEpochSecret } from "./annotation.ts";
import { expectMessage, MsgType } from "./wire.ts";

export const DEFAULT_REKEY_INTERVAL_MS = 10 * 60_000;
export const DEFAULT_ACK_TIMEOUT_MS = 1_500;
/** Kicks and encryptor replacement switch within this, acked or not. */
export const DEFAULT_FAST_ACK_TIMEOUT_MS = 300;
export const MAX_EPOCH = 0xffff_ffff;

export type RekeyReason = "initial" | "interval" | "kick" | "pin_rotated" | "encryptor_replaced" | "recovery" | "manual";

const FAST_REASONS: ReadonlySet<RekeyReason> = new Set(["kick", "encryptor_replaced", "recovery"]);

export interface KeyManagerEvents extends Record<string, unknown> {
  rekeyStarted: { epoch: number; reason: RekeyReason; sessions: number };
  activated: { epoch: number; reason: RekeyReason; acked: number; timedOut: string[] };
}

interface Pending {
  key: EpochKey;
  reason: RekeyReason;
  /** Absolute switch time; never later than a superseded rekey's. */
  deadline: number;
  waiting: Set<string>;
  acked: number;
}

/** The active epoch must be newer than `epoch` by `at`, or encryption stops. */
interface Cutoff {
  at: number;
  epoch: number;
}

/** Allocates strictly increasing epoch numbers, persisted so a reload never reuses one. */
export async function nextEpochNumber(storage: AtomicStorage, streamId: string): Promise<number> {
  const v = await storage.update(`epoch:${streamId}`, (cur) => (typeof cur === "number" ? cur + 1 : 1));
  if (typeof v !== "number" || v > MAX_EPOCH) throw new E2EEError("counter_exhausted", "epoch space exhausted");
  return v;
}

export interface SenderKeyManagerOptions {
  storage: AtomicStorage;
  streamId: string;
  /** Transport to one admitted viewer session (by session id). */
  send(sessionId: string, bytes: Bytes): void;
  /**
   * The epoch to encrypt with from now on: post it to the sender worker.
   * Called exactly once per epoch; never install a key anywhere else.
   */
  onActivate(key: EpochKey): void;
  /**
   * Stop encrypting now (post `sender-suspend` to the worker): a revocation
   * could not get a fresh epoch active by its cutoff. Frames are dropped
   * until the next `onActivate`.
   */
  onSuspend(): void;
  /** Ask the encoder for a keyframe so viewers can decode the new epoch immediately. */
  onKeyframeNeeded?(): void;
  /**
   * A revocation began or tightened: by `within` ms from now, encryption must
   * use an epoch newer than `stale`, or stop. Post it to the sender worker
   * (postSenderCutoff), which enforces it on its own clock, so a stalled main
   * thread (this class's tick() not running) cannot extend the cutoff.
   * Called synchronously, before any queued work.
   */
  onCutoff?(c: { within: number; stale: number }): void;
  now?(): number;
  rekeyIntervalMs?: number;
  ackTimeoutMs?: number;
  fastAckTimeoutMs?: number;
  /** Storage operations (epoch allocation) fail after this; default 1 s. */
  storageTimeoutMs?: number;
}

export class SenderKeyManager extends Emitter<KeyManagerEvents> {
  readonly #o: SenderKeyManagerOptions;
  readonly #storage: TimedStorage;
  readonly #sessions = new Map<string, SenderSession>();
  #active: EpochKey | null = null;
  #activeSince = 0;
  #pending: Pending | null = null;
  #cutoff: Cutoff | null = null;
  /** Highest epoch number allocated by this manager. */
  #allocated = 0;
  /** Revocations announced by beginRevocation() whose sessions are not removed yet. */
  #unresolved = 0;
  /** The encryptor has no key (onSuspend fired, no onActivate since). */
  #suspended = false;
  /** Newest epoch handed to onActivate. A key is never handed over twice. */
  #delivered = 0;
  /** Outstanding hold() calls: encryption stays stopped while any is open. */
  #holds = 0;
  /** A recovery attempt is queued or running. */
  #healing = false;
  #healAttempt = 0;
  #healNotBefore = 0;
  #chain: Promise<unknown> = Promise.resolve();

  constructor(o: SenderKeyManagerOptions) {
    super();
    this.#o = o;
    this.#storage = new TimedStorage(o.storage, () => this.#now(), o.storageTimeoutMs ?? DEFAULT_STORAGE_TIMEOUT_MS);
  }

  get rekeyIntervalMs(): number {
    return this.#o.rekeyIntervalMs ?? DEFAULT_REKEY_INTERVAL_MS;
  }
  get ackTimeoutMs(): number {
    return this.#o.ackTimeoutMs ?? DEFAULT_ACK_TIMEOUT_MS;
  }
  get fastAckTimeoutMs(): number {
    return this.#o.fastAckTimeoutMs ?? DEFAULT_FAST_ACK_TIMEOUT_MS;
  }
  #now(): number {
    return (this.#o.now ?? Date.now)();
  }

  /** Serialise every state change; crypto awaits must not interleave. */
  #serial<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.#chain.then(fn, fn);
    this.#chain = run.catch(() => undefined);
    return run;
  }

  async init(): Promise<void> {
    return this.#serial(async () => {
      if (this.#active) throw new E2EEError("state", "already initialised");
      const epoch = await this.#allocate();
      const now = this.#now();
      this.#pending = { key: this.#remember({ epoch, secret: randomBytes(EPOCH_SECRET_BYTES) }), reason: "initial", deadline: now, waiting: new Set(), acked: 0 };
      this.#tryActivate();
    });
  }

  /** Number of the epoch currently used for encryption. The secret is not exposed (see the header). */
  get activeEpoch(): number {
    if (!this.#active) throw new E2EEError("state", "not initialised");
    return this.#active.epoch;
  }
  get pendingEpoch(): number | null {
    return this.#pending?.key.epoch ?? null;
  }
  /** Absolute time by which the pending epoch is activated, acked or not. */
  get pendingDeadline(): number | null {
    return this.#pending?.deadline ?? null;
  }
  /**
   * True while encryption is stopped: a revocation missed its cutoff, a
   * rekey it needed failed, or a hold() is open. Recovery is retried by tick().
   */
  get suspended(): boolean {
    return this.#suspended;
  }
  /** Absolute time by which a revocation needs a fresh active epoch, if one is open. */
  get revocationCutoff(): number | null {
    return this.#revocationOpen() ? this.#cutoff!.at : null;
  }
  sessionIds(): string[] {
    return [...this.#sessions.keys()];
  }

  /**
   * Annotation inputs (PROTOCOL §20) of the newest allocated epochs. They are
   * one-way derivations of the epoch secrets and never leave this class.
   */
  readonly #annot = new Map<number, Bytes>();
  #remember(k: EpochKey): EpochKey {
    this.#annot.set(k.epoch, annotationEpochSecret(k));
    const keep = [...this.#annot.keys()].sort((a, b) => b - a).slice(0, ANNOTATION_MAX_EPOCHS);
    for (const [e, v] of this.#annot) if (!keep.includes(e)) (v.fill(0), this.#annot.delete(e));
    return k;
  }

  /**
   * Seal a downstream annotation batch for one session under the active
   * epoch. Not serialised behind rekeys: it reads only the session's own
   * counter. Throws `unknown_admission` for a removed (kicked, left) session.
   */
  sealAnnotations(sessionId: string, records: readonly AnnotationRecord[]): Promise<Bytes> {
    const s = this.#sessions.get(sessionId);
    if (!s) return Promise.reject(new E2EEError("unknown_admission", "no such session"));
    const epoch = this.activeEpoch;
    const secret = this.#annot.get(epoch);
    if (!secret) return Promise.reject(new E2EEError("unknown_key", "no annotation key for the active epoch"));
    return s.sealAnnotations(epoch, secret, records);
  }

  /** Open an upstream annotation from a session; accepted under any of the newest allocated epochs. */
  async openAnnotation(sessionId: string, bytes: Uint8Array): Promise<{ epoch: number; body: Bytes }> {
    const m = expectMessage(bytes, MsgType.Annotation);
    const s = this.#sessions.get(sessionId);
    if (!s) throw new E2EEError("unexpected_message", "annotation from unknown session");
    const removed = () => new E2EEError("unexpected_message", "annotation from removed session");
    let body: Bytes;
    try {
      body = await s.openAnnotation(m, this.#annot.get(m.epoch));
    } catch (e) {
      throw this.#sessions.get(sessionId) !== s ? removed() : e;
    }
    // Removed (kicked) during the decrypt: drop it.
    if (this.#sessions.get(sessionId) !== s) throw removed();
    return { epoch: m.epoch, body };
  }
  hasSession(id: string): boolean {
    return this.#sessions.has(id);
  }

  /**
   * Register a freshly admitted session and return the active epoch for its
   * admit bundle (viewer decryption only). If a rekey is pending, the session
   * is sent that epoch too, so it is never left behind by the switch.
   */
  admit(session: SenderSession): Promise<EpochKey> {
    return this.#serial(async () => {
      if (!this.#active) throw new E2EEError("state", "not initialised");
      const active = this.#active;
      this.#sessions.set(session.id, session);
      const p = this.#pending;
      if (p) {
        p.waiting.add(session.id);
        this.#o.send(session.id, await session.sealRekey(p.key));
      }
      return active;
    });
  }

  /** A viewer left without being kicked: stop sending it rekeys. */
  remove(sessionId: string): Promise<void> {
    return this.#serial(async () => {
      this.#sessions.get(sessionId)?.disposeAnnotations?.();
      this.#sessions.delete(sessionId);
      const p = this.#pending;
      if (p?.waiting.delete(sessionId) && p.waiting.size === 0) this.#tryActivate();
    });
  }

  /**
   * Remove the sessions and move everyone else to a fresh epoch (fast
   * switch). The removal and the cutoff take effect synchronously, before
   * any queued work; if the rekey fails, encryption stops (fail closed).
   */
  kick(sessionIds: Iterable<string>): Promise<void> {
    for (const id of sessionIds) {
      this.#sessions.get(id)?.disposeAnnotations?.();
      this.#sessions.delete(id);
    }
    this.#arm(this.#allocated);
    this.#o.onCutoff?.({ within: this.#cutoff!.at - this.#now(), stale: this.#allocated });
    return this.#serial(() => this.#rekey("kick")).catch((e: unknown) => {
      this.#failClosed();
      throw e;
    });
  }

  /**
   * Announce a revocation whose sessions will be passed to kick() later
   * (SenderHost calls this when an owner kick is requested, so its own queue
   * cannot delay the deadline). The cutoff starts now; until the returned
   * release function is called, no epoch counts as fresh. Call release right
   * after kick() was called, or when the revocation is abandoned.
   */
  beginRevocation(): () => void {
    this.#unresolved++;
    // No epoch requirement yet: kick() sets it once the sessions are removed.
    this.#arm(0);
    // The worker's copy is stricter from the start: every epoch allocated so
    // far may be held by the sessions about to be removed.
    this.#o.onCutoff?.({ within: this.#cutoff!.at - this.#now(), stale: this.#allocated });
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.#unresolved--;
      this.#settleCutoff();
      this.#maybeHeal();
    };
  }

  /**
   * Stop encryption until the returned release function is called
   * (SenderHost: a revocation could not be made durable yet). Epochs keep
   * rotating meanwhile. On release, recovery delivers the newest epoch if the
   * encryptor never had it and no revocation is open, else a fresh one.
   */
  hold(): () => void {
    this.#holds++;
    this.#stop();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.#holds--;
      this.#maybeHeal();
    };
  }

  /** Start (or tighten) the cutoff; epochs up to `stale` no longer count as fresh. */
  #arm(stale: number): void {
    const at = this.#now() + this.fastAckTimeoutMs;
    const c = this.#cutoff;
    this.#cutoff = { at: Math.min(c?.at ?? Infinity, at), epoch: Math.max(c?.epoch ?? 0, stale) };
  }

  rekey(reason: RekeyReason = "manual"): Promise<void> {
    return this.#serial(() => this.#rekey(reason));
  }

  /**
   * The sender's encryptor was (or is about to be) replaced: a recreated
   * worker or SenderKeyRing must never get an epoch an earlier encryptor used.
   * Starts a fast rekey; the new encryptor gets the fresh epoch via
   * `onActivate` and drops frames (fail closed) until then.
   */
  replaceEncryptor(): Promise<void> {
    return this.#serial(() => this.#rekey("encryptor_replaced"));
  }

  async #allocate(): Promise<number> {
    const epoch = await nextEpochNumber(this.#storage, this.#o.streamId);
    this.#allocated = Math.max(this.#allocated, epoch);
    return epoch;
  }

  async #rekey(reason: RekeyReason): Promise<void> {
    if (!this.#active) throw new E2EEError("state", "not initialised");
    const epoch = await this.#allocate();
    const key: EpochKey = this.#remember({ epoch, secret: randomBytes(EPOCH_SECRET_BYTES) });
    // Taken after the allocation: sessions kicked before it are not targets.
    const targets = [...this.#sessions.values()];
    const timeout = FAST_REASONS.has(reason) ? this.fastAckTimeoutMs : this.ackTimeoutMs;
    // An open revocation's cutoff bounds the deadline too: this epoch is newer than any a kicked session holds.
    const cutoff = this.#revocationOpen() ? this.#cutoff!.at : Infinity;
    const deadline = Math.min(this.#pending?.deadline ?? Infinity, this.#now() + timeout, cutoff);
    // Replaces any pending epoch: that one is never activated.
    this.#pending = { key, reason, deadline, waiting: new Set(targets.map((s) => s.id)), acked: 0 };
    this.emit("rekeyStarted", { epoch, reason, sessions: targets.length });
    for (const s of targets) {
      const sealed = await s.sealRekey(key);
      // A session kicked during the fan-out never gets the key.
      if (this.#sessions.get(s.id) === s) this.#o.send(s.id, sealed);
      else this.#pending?.waiting.delete(s.id);
    }
    if (this.#pending?.key.epoch === epoch && this.#pending.waiting.size === 0) this.#tryActivate();
    this.#enforce();
  }

  /** Process a REKEY_ACK from a session. Bad acks throw and change nothing. */
  handle(sessionId: string, bytes: Uint8Array): Promise<void> {
    return this.#serial(async () => {
      const ack = expectMessage(bytes, MsgType.RekeyAck);
      const s = this.#sessions.get(sessionId);
      if (!s) throw new E2EEError("unexpected_message", "ack from unknown session");
      const epoch = await s.openAck(ack);
      const p = this.#pending;
      if (p && p.key.epoch === epoch && p.waiting.delete(sessionId)) {
        p.acked += 1;
        if (p.waiting.size === 0) this.#tryActivate();
      }
    });
  }

  /**
   * Drive timeouts: call often (SenderHost.startTimers does, every 50 ms).
   * Deadlines, storage timeouts and recovery retries are handled
   * synchronously, before anything is queued, so queued work (a rekey
   * fan-out, a backlog of kicks, a hung storage call) cannot delay them.
   */
  tick(): Promise<void> {
    this.#storage.check();
    this.#enforce();
    this.#maybeHeal();
    return this.#serial(async () => {
      this.#enforce();
      const now = this.#now();
      if (!this.#pending && this.#active && !this.#revocationOpen() && now - this.#activeSince >= this.rekeyIntervalMs) {
        await this.#rekey("interval");
      }
    });
  }

  /** Synchronous: activate an overdue pending epoch; fail closed on a missed cutoff. */
  #enforce(): void {
    const now = this.#now();
    const p = this.#pending;
    if (p && now >= p.deadline) this.#tryActivate();
    if (this.#overdue()) this.#failClosed();
  }

  /** A revocation still needs a fresh epoch to become active. */
  #revocationOpen(): boolean {
    const c = this.#cutoff;
    return c !== null && this.#active !== null && (this.#unresolved > 0 || this.#active.epoch <= c.epoch);
  }

  /** An open revocation is past its cutoff: the active key must not be used. */
  #overdue(): boolean {
    return this.#revocationOpen() && this.#now() >= this.#cutoff!.at;
  }

  /** Allocated before an armed cutoff: a revoked session may hold it. */
  #stale(p: Pending): boolean {
    return this.#cutoff !== null && p.key.epoch <= this.#cutoff.epoch;
  }

  #settleCutoff(): void {
    if (this.#cutoff && !this.#revocationOpen()) this.#cutoff = null;
  }

  #stop(): void {
    if (this.#suspended) return;
    this.#suspended = true;
    this.#o.onSuspend();
  }

  /**
   * A revocation has no fresh active epoch: stop encrypting with a key a
   * kicked session may hold, and start recovery.
   */
  #failClosed(): void {
    if (!this.#revocationOpen()) return;
    this.#stop();
    this.#maybeHeal();
  }

  /** Encryption is stopped (or must stop) and nothing else will restart it. */
  #needsRecovery(): boolean {
    return this.#active !== null && this.#holds === 0 && (this.#suspended || this.#overdue());
  }

  /**
   * Start a recovery attempt if one is needed, none is in flight and the
   * backoff allows (100 ms doubling, at most 2 s apart). Called from tick(),
   * so recovery never depends on another event happening.
   */
  #maybeHeal(): void {
    if (this.#healing) return;
    if (!this.#needsRecovery()) {
      this.#healAttempt = 0;
      this.#healNotBefore = 0;
      return;
    }
    const now = this.#now();
    if (now < this.#healNotBefore) return;
    this.#healing = true;
    this.#healNotBefore = now + retryDelayMs(this.#healAttempt++);
    void this.#serial(() => this.#heal())
      .catch(() => undefined)
      .finally(() => {
        this.#healing = false;
        this.#maybeHeal();
      });
  }

  /**
   * One recovery attempt. Encryption resumes only through onActivate of an
   * epoch the encryptor never had and no revoked session can hold: a key is
   * never re-delivered. While a revocation is unresolved its sessions are
   * still registered, so a rekey would only reach them; the next attempt
   * (or kick()) rekeys once they are removed.
   */
  async #heal(): Promise<void> {
    if (!this.#needsRecovery() || this.#unresolved > 0) return;
    const p = this.#pending;
    if (p && !this.#stale(p)) {
      // A fresh epoch is on its way and activates by its deadline.
      this.#enforce();
      return;
    }
    if (this.#active!.epoch > this.#delivered && !this.#revocationOpen()) {
      this.#deliver();
      return;
    }
    await this.#rekey("recovery");
  }

  /** Activate the pending epoch, unless a revoked session may hold it: then discard it. */
  #tryActivate(): void {
    const p = this.#pending;
    if (!p) return;
    this.#pending = null;
    if (this.#stale(p)) return; // the kick's own rekey (or recovery) allocates a fresh one
    this.#active = p.key;
    this.#activeSince = this.#now();
    this.#settleCutoff();
    this.emit("activated", { epoch: p.key.epoch, reason: p.reason, acked: p.acked, timedOut: [...p.waiting] });
    this.#deliver();
  }

  /**
   * Hand the active epoch to the encryptor if that is safe and it never had
   * it. Held, or past an open revocation's cutoff: keep encryption stopped.
   */
  #deliver(): void {
    const a = this.#active;
    if (!a) return;
    if (this.#holds > 0 || this.#overdue()) {
      this.#stop();
      this.#maybeHeal();
      return;
    }
    if (a.epoch <= this.#delivered) return;
    const first = this.#delivered === 0;
    this.#delivered = a.epoch;
    this.#suspended = false;
    this.#o.onActivate(a);
    if (!first) this.#o.onKeyframeNeeded?.();
  }
}
