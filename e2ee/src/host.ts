/**
 * SenderHost: the sender's whole E2EE control plane behind one object.
 * Routes ordered relay messages per peer to a SenderAdmission or to the key
 * manager, and implements the owner actions (kick, rotate PIN, lock, unlock).
 *
 * Two kinds of identity, never mixed:
 * - `peerId` is the transport identity the app uses for a viewer connection
 *   (e.g. the SFU's viewer id). The relay controls it. It only routes
 *   messages; nothing trusts it, and a HELLO on a peer never touches an
 *   existing admission.
 * - `admissionId` names one authenticated admission (one successful CPace
 *   run). It is created by the sender, links the session to the grant it was
 *   admitted with, and outlives the transport. Owner actions take it.
 *
 * Commits of new admissions, kicks and PIN rotations are serialised, and a
 * commit re-checks revocation (PIN generation, lock, grant validity, abort)
 * immediately before it registers the session and releases the epoch key.
 *
 * Revocation is write-ahead (see grants.ts). A kick hands a tombstone for
 * the grant to storage, and a PIN rotation a grant-generation bump, before
 * anything changes in memory; then it revokes in memory (routing, grant,
 * channel keys, the key manager's switch cutoff) and only then cleans up the
 * grant table. A restart therefore never revives a revoked grant once its
 * fence write landed. If that write fails, the revocation still applies in
 * memory, but media stops (key manager hold) and every admission, grant
 * re-admissions included, is refused until a retry (backoff, at most 2 s
 * apart, driven by tick()) makes it durable: `revocationNotDurable` and the
 * `revocationDurability` event tell the UI. A failed kick throws, its
 * records stay listed with `kickIncomplete`, and it can be retried; the
 * kicked sessions never get another epoch either way.
 */
import { type AdmissionCommit, type CommitResult, type KnockPreparation, normalizePin, SenderAdmission, type SenderAdmissionContext } from "./admission.ts";
import { type BudgetLimits, GuessBudget } from "./budget.ts";
import { type Bytes, copy, randomBytes, toBase64Url } from "./bytes.ts";
import type { AnnotationRecord } from "./annotation.ts";
import { type EpochKey, SenderSession } from "./channel.ts";
import { E2EEError, isE2EEError } from "./errors.ts";
import { Emitter } from "./events.ts";
import { type Grant, GrantStore } from "./grants.ts";
import { assertLinkSecret, type SenderIdentity } from "./identity.ts";
import { assignRowCode, rowCodeIndex } from "./row-code.ts";
import { SenderKeyManager } from "./keys.ts";
import { type AtomicStorage, retryDelayMs, TimedStorage } from "./storage.ts";
import { AdmissionMode, decodeMessage, MsgType, peekType, RejectReason, rejectReasonName, type RejectReasonId } from "./wire.ts";

export interface SenderHostOptions {
  identity: SenderIdentity;
  streamId: string;
  /** null = explicit "no PIN": the link secret alone admits. */
  pin: string | null;
  pinVersion?: number;
  locked?: boolean;
  knock?: boolean;
  knockTimeoutMs?: number;
  maxKnocks?: number;
  /** Additional recovery gate; unlike lock, also blocks grant reconnects. */
  reconciling?: boolean;
  /** The mandatory 32-byte fragment secret (see buildFragment). */
  linkSecret: Uint8Array;
  storage: AtomicStorage;
  send(peerId: string, bytes: Bytes): void;
  /** New active epoch: post it to the sender transform worker. Called once per epoch. */
  onEpoch(key: EpochKey): void;
  /**
   * Stop encrypting now: post `sender-suspend` to the sender worker
   * (postSenderSuspend). A kick could not get a fresh epoch active by its
   * cutoff, so the current key may be held by a kicked viewer. The next
   * `onEpoch` resumes encryption.
   */
  onSuspend(): void;
  onKeyframeNeeded?(): void;
  /** A kick's cutoff: post it to the sender worker (postSenderCutoff). See SenderKeyManagerOptions.onCutoff. */
  onCutoff?(c: { within: number; stale: number }): void;
  now?(): number;
  limits?: Partial<BudgetLimits>;
  grantTtlMs?: number;
  rekeyIntervalMs?: number;
  ackTimeoutMs?: number;
  fastAckTimeoutMs?: number;
  /** Every storage operation fails (E2EEError "storage") after this; default 1 s. */
  storageTimeoutMs?: number;
}

/** Display labels are reported by the viewer (via the app/SFU) and unauthenticated. */
export const MAX_VIEWER_LABEL_LENGTH = 64;

/** One authenticated admission, as shown in the owner's viewer table. */
export interface ViewerInfo {
  /** Sender-assigned id. Pass it to kick() and the other owner actions. */
  admissionId: string;
  viewerId: string;
  rowCode: string;
  viaGrant: boolean;
  admittedAt: number;
  /** True while the session receives rekeys. */
  connected: boolean;
  /**
   * A kick removed this admission's keys but failed to persist (storage
   * error). It gets no further epochs; call kick() again to finish.
   */
  kickIncomplete: boolean;
  /** Transport id the app routes by. Relay-controlled: never base a decision on it. */
  peerId: string;
  /**
   * Viewer-reported and unauthenticated: the viewer or the relay can set or
   * swap it. Display only, and show it as such.
   */
  viewerReportedLabel: string | null;
}

export type KnockEnd = "denied" | "timeout" | "peer_closed" | "replaced" | "pin_rotated" | "stopped" | "commit_failed" | "queue_full" | "protocol_error";
export interface KnockInfo { knockId: string; peerId: string; rowCode: string; at: number; expiresAt: number }
interface KnockRecord { info: KnockInfo; adm: SenderAdmission }

export interface HostEvents extends Record<string, unknown> {
  knock: KnockInfo;
  knockEnded: { knockId: string; reason: KnockEnd };
  admitted: { admissionId: string; peerId: string; viaGrant: boolean };
  rejected: { peerId: string; reason: string };
  /** Admissions removed by an owner kick. Close these transports. */
  kicked: { admissionIds: string[]; peerIds: string[] };
  /** The transport went away (or was superseded); the admission record stays kickable. */
  disconnected: { admissionId: string; peerId: string };
  protocolError: { peerId: string; error: E2EEError };
  /**
   * An annotation body (PROTOCOL §20) that authenticated under this
   * admission's pairwise key. `rowCode` comes from the admission record, never
   * from the message: stamp downstream records with it.
   */
  annotation: { admissionId: string; viewerId: string; rowCode: string; epoch: number; body: Bytes };
  /**
   * Emitted synchronously when an owner kick is requested, before it queues
   * or writes anything: these admissions may no longer annotate, and nothing
   * more is sent to them. Drop their pending ink and clear what they drew.
   */
  annotationRevoked: { admissionIds: string[]; rowCode: string };
  /**
   * A revocation could not be written to storage (`notDurable: true`; media
   * is stopped and admissions refused, retrying), or it now is (`false`).
   * Tell the owner: until then a sender restart could let a revoked viewer back.
   */
  revocationDurability: { notDurable: boolean; error?: string };
}

interface AdmissionRecord {
  viewerId: string;
  rowCode: string;
  id: string;
  grantId: string;
  grantIdBytes: Bytes;
  grantExpiresAt: number;
  viaGrant: boolean;
  admittedAt: number;
  peerId: string;
  connected: boolean;
  label: string | null;
  /** Revoked by a kick that has not finished persisting. */
  kickIncomplete: boolean;
  /** Was connected when that kick started: its transport still needs closing. */
  closePending: boolean;
}

/** A revocation to write ahead: a generation bump (PIN rotation) and/or tombstones (kick). */
interface FenceIntent {
  bump?: boolean;
  pinVersion?: number;
  grants?: readonly { id: Bytes; expiresAt: number }[];
}

/** unlock() needs a decision: rotate to a new PIN (default), or explicitly keep the old one. */
export type UnlockOptions = { newPin: string | null; version?: number } | { keepPin: true };

export class SenderHost extends Emitter<HostEvents> {
  readonly budget: GuessBudget;
  readonly grants: GrantStore;
  readonly keys: SenderKeyManager;
  readonly #o: SenderHostOptions;
  readonly #ctx: SenderAdmissionContext;
  readonly #handshakes = new Map<string, SenderAdmission>();
  readonly #knocks = new Map<string, KnockRecord>();
  readonly #committingCodes = new Set<string>();
  #stopped = false;
  #stopHold: (() => void) | null = null;
  readonly #records = new Map<string, AdmissionRecord>();
  /** peerId → admissionId of the connected admission on that transport. */
  readonly #byPeer = new Map<string, string>();
  /** Grants (b64url ids) whose annotation access ended when a kick was requested. */
  readonly #annotationBlocked = new Set<string>();
  readonly #now: () => number;
  #pin: string | null;
  #pinGeneration = 0;
  /** False while a PIN rotation has started but not finished persisting. */
  #rotationDone = true;
  #locked = false;
  #frozen = false;
  #pinVersion = 0;
  #fencePinVersion = 0;
  #rotationCall = false;
  #versionMismatch = false;
  #reconciling = false;
  #rotationTarget: { pin: string | null; version: number } | null = null;

  /** Grants (b64url ids) revoked in memory whose storage revocation has not completed. */
  readonly #revokedGrants = new Set<string>();
  /** Revocations whose fence write has not succeeded yet (merged; retried by tick()). */
  readonly #fence = {
    bump: false,
    pinVersion: undefined as number | undefined,
    grants: new Map<string, { id: Bytes; expiresAt: number }>(),
    /** Bumped on every new intent, so a write only clears what it covered. */
    version: 0,
    /** Open while a write failed and none has succeeded since: media held, admissions refused. */
    release: null as (() => void) | null,
    attempt: 0,
    retryAt: 0,
    retrying: false,
  };
  readonly #storage: TimedStorage;
  #chain: Promise<unknown> = Promise.resolve();

  private constructor(o: SenderHostOptions) {
    super();
    assertLinkSecret(o.linkSecret);
    if (o.pin !== null) normalizePin(o.pin);
    this.#o = o;
    this.#pin = o.pin;
    this.#locked = o.locked ?? false;
    this.#reconciling = o.reconciling ?? false;
    if (o.pinVersion !== undefined && (!Number.isSafeInteger(o.pinVersion) || o.pinVersion < 0)) throw new E2EEError("invalid_argument", "invalid PIN version");
    if (!Number.isInteger(o.knockTimeoutMs ?? 120_000) || (o.knockTimeoutMs ?? 120_000) < 1 || (o.knockTimeoutMs ?? 120_000) > 0xffff_ffff) throw new E2EEError("invalid_argument", "invalid knock timeout");
    if (!Number.isInteger(o.maxKnocks ?? 50) || (o.maxKnocks ?? 50) < 1 || (o.maxKnocks ?? 50) > 50) throw new E2EEError("invalid_argument", "maxKnocks must be 1–50");
    const linkSecret = copy(o.linkSecret);
    const now = (this.#now = o.now ?? Date.now);
    const storage = (this.#storage = new TimedStorage(o.storage, now, o.storageTimeoutMs));
    this.budget = new GuessBudget({ storage, streamId: o.streamId, now, limits: o.limits ?? {} });
    this.budget.on("frozen", () => { this.#frozen = true; });
    this.budget.on("unlocked", () => { this.#frozen = false; });
    this.grants = new GrantStore({ storage, streamId: o.streamId, now, ...(o.grantTtlMs ? { ttlMs: o.grantTtlMs } : {}) });
    this.keys = new SenderKeyManager({
      storage: o.storage,
      streamId: o.streamId,
      send: (admissionId, bytes) => {
        const r = this.#records.get(admissionId);
        if (!this.#stopped && r?.connected) o.send(r.peerId, bytes);
      },
      onActivate: o.onEpoch,
      onSuspend: o.onSuspend,
      now,
      ...(o.onKeyframeNeeded ? { onKeyframeNeeded: o.onKeyframeNeeded } : {}),
      ...(o.onCutoff ? { onCutoff: o.onCutoff } : {}),
      ...(o.rekeyIntervalMs ? { rekeyIntervalMs: o.rekeyIntervalMs } : {}),
      ...(o.ackTimeoutMs ? { ackTimeoutMs: o.ackTimeoutMs } : {}),
      ...(o.fastAckTimeoutMs ? { fastAckTimeoutMs: o.fastAckTimeoutMs } : {}),
      ...(o.storageTimeoutMs ? { storageTimeoutMs: o.storageTimeoutMs } : {}),
    });
    this.#ctx = {
      identity: o.identity,
      streamId: o.streamId,
      pin: () => this.#pin,
      pinGeneration: () => this.#pinGeneration,
      linkSecret,
      budget: this.budget,
      grants: this.grants,
      currentEpoch: () => this.keys.activeEpoch,
      commit: (adm, c) => this.#commit(adm, c),
      locked: () => this.#locked,
      blocked: () => this.#blocked(),
      ...(o.knock ? { prepareKnock: (adm: SenderAdmission, c: AdmissionCommit) => this.#prepareKnock(adm, c) } : {}),
      now,
    };
  }

  static async create(o: SenderHostOptions): Promise<SenderHost> {
    const h = new SenderHost(o);
    const state = await h.grants.inspect();
    h.#pinVersion = o.pinVersion ?? 0;
    h.#fencePinVersion = state.pinVersion;
    h.#versionMismatch = state.pinVersion !== h.#pinVersion;
    h.#rotationDone = state.rotationComplete;
    await h.keys.init();
    // Apply revocations a crash left written ahead but not applied. lookup()
    // enforces them regardless, so a failure here is harmless.
    await h.grants.recover().catch(() => undefined);
    return h;
  }

  static async inspect(storage: AtomicStorage, streamId: string): Promise<{ pinVersion: number; rotationComplete: boolean; frozen: boolean }> {
    const grants = new GrantStore({ storage, streamId });
    const budget = new GuessBudget({ storage, streamId });
    return { ...await grants.inspect(), frozen: (await budget.inspect()).frozen };
  }

  get pinVersion(): number { return this.#pinVersion; }
  get rotationComplete(): boolean { return this.#rotationDone && !this.#versionMismatch; }
  get rotationPending(): boolean { return this.#rotationTarget !== null || !this.rotationComplete; }
  setReconciling(value: boolean): void { this.#reconciling = value; }
  /** Recovery detected inconsistent records. Only a newer rotation can repair the fence. */
  enterStateMismatch(): void {
    this.#versionMismatch = true;
    this.#locked = true;
    this.#reconciling = true;
    this.#rotationTarget = null;
  }
  #blocked(): boolean { return this.#stopped || this.#reconciling || this.#versionMismatch || !this.#rotationDone || this.revocationNotDurable; }

  /**
   * True while a revocation (kick tombstone or PIN-rotation generation) is in
   * force in memory but not yet in storage. Meanwhile encryption is stopped
   * and every admission is refused with `locked`; tick() retries the write.
   */
  get revocationNotDurable(): boolean {
    return this.#fence.release !== null;
  }

  /**
   * Record a revocation and write the merged fence. The storage write starts
   * synchronously, so callers issue this before they change anything in
   * memory. Resolves true once durable; false if the write failed (then it
   * is retried from tick()). Never rejects.
   */
  #persistRevocation(intent: FenceIntent): Promise<boolean> {
    const f = this.#fence;
    if (intent.bump) { f.bump = true; f.pinVersion = intent.pinVersion; }
    for (const g of intent.grants ?? []) f.grants.set(toBase64Url(g.id), g);
    f.version++;
    return this.#flushFence();
  }

  async #flushFence(): Promise<boolean> {
    const f = this.#fence;
    const version = f.version;
    try {
      // A generation bump revokes every grant issued so far, tombstoned ones included.
      if (f.bump) await this.grants.bumpGeneration(f.pinVersion);
      else await this.grants.tombstone([...f.grants.values()]);
    } catch (e) {
      f.retryAt = this.#now() + retryDelayMs(f.attempt++);
      if (!f.release) {
        f.release = this.keys.hold();
        this.emit("revocationDurability", { notDurable: true, error: String(e) });
      }
      return false;
    }
    // A newer intent arrived during the write: write again so it is covered too.
    if (f.version !== version) return this.#flushFence();
    f.bump = false;
    f.pinVersion = undefined;
    f.grants.clear();
    f.attempt = 0;
    const release = f.release;
    if (release) {
      f.release = null;
      release();
      this.emit("revocationDurability", { notDurable: false });
    }
    return true;
  }

  static #notDurable(): E2EEError {
    return new E2EEError("storage", "revocation is not durable yet: media is stopped and admissions are refused until a retry stores it");
  }

  /** Serialise admission commits with owner actions. */
  #serial<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.#chain.then(fn, fn);
    this.#chain = run.catch(() => undefined);
    return run;
  }

  /** The owner's viewer table. */
  viewers(): ViewerInfo[] {
    return [...this.#records.values()].map((r) => ({
      admissionId: r.id,
      viewerId: r.viewerId,
      rowCode: r.rowCode,
      viaGrant: r.viaGrant,
      admittedAt: r.admittedAt,
      connected: r.connected,
      kickIncomplete: r.kickIncomplete,
      peerId: r.peerId,
      viewerReportedLabel: r.label,
    }));
  }

  knocks(): KnockInfo[] { return [...this.#knocks.values()].map((k) => ({ ...k.info })); }

  async #newRowCode(): Promise<string> {
    const used = await this.grants.rowCodes();
    for (const k of this.#knocks.values()) used.add(k.info.rowCode);
    for (const r of this.#records.values()) used.add(r.rowCode);
    for (const code of this.#committingCodes) used.add(code);
    return assignRowCode(used);
  }

  #prepareKnock(adm: SenderAdmission, c: AdmissionCommit): Promise<KnockPreparation> {
    return this.#serial(async () => {
      const refusal = (): KnockPreparation | null => {
        if (c.pinGeneration !== this.#pinGeneration || adm.aborted || this.#handshakes.get(adm.peerId) !== adm) return { ok: false, reason: RejectReason.Expired };
        if (this.#blocked() || this.#locked) return { ok: false, reason: RejectReason.Locked };
        if (this.#frozen) return { ok: false, reason: RejectReason.Frozen };
        return null;
      };
      const before = refusal();
      if (before) return before;
      const knockId = toBase64Url(randomBytes(16));
      if (this.#knocks.size >= (this.#o.maxKnocks ?? 50)) {
        this.emit("knockEnded", { knockId, reason: "queue_full" });
        return { ok: false, reason: RejectReason.Denied, retryAfterMs: 30_000 };
      }
      const rowCode = await this.#newRowCode();
      const after = refusal();
      if (after) return after;
      const timeoutMs = this.#o.knockTimeoutMs ?? 120_000;
      const at = this.#now();
      this.#knocks.set(knockId, { adm, info: { knockId, peerId: adm.peerId, rowCode, at, expiresAt: at + timeoutMs } });
      return { ok: true, rowCode, timeoutMs };
    });
  }

  #endKnock(k: KnockRecord, reason: KnockEnd, notify = true): void {
    if (!this.#knocks.delete(k.info.knockId)) return;
    const rejected = k.adm.state === "pending" ? k.adm.rejectPending(reason === "protocol_error" ? RejectReason.Invalid : RejectReason.Denied) : null;
    void k.adm.abort(reason);
    if (notify && rejected) this.#sendAdmission(k.adm, rejected);
    if (this.#handshakes.get(k.info.peerId) === k.adm) this.#handshakes.delete(k.info.peerId);
    this.emit("knockEnded", { knockId: k.info.knockId, reason });
  }

  #endPeerKnock(peer: string, reason: KnockEnd): void {
    for (const k of this.#knocks.values()) if (k.info.peerId === peer) this.#endKnock(k, reason);
  }

  async approve(knockId: string, o: { rowCode: string }): Promise<void> {
    const k = this.#knocks.get(knockId);
    if (!k) throw new E2EEError("unknown_knock", "knock has ended or never existed");
    if (this.#now() >= k.info.expiresAt) {
      this.#endKnock(k, "timeout");
      throw new E2EEError("unknown_knock", "knock expired");
    }
    rowCodeIndex(o?.rowCode);
    if (o.rowCode !== k.info.rowCode) throw new E2EEError("code_mismatch", "row code does not match this knock");
    // Remove synchronously: this id can never be approved twice or rebound.
    this.#committingCodes.add(k.info.rowCode);
    this.#knocks.delete(knockId);
    let ended = false;
    try {
      const message = await k.adm.approve();
      this.#sendAdmission(k.adm, message);
      if (k.adm.outcome?.kind !== "admitted") {
        ended = true;
        this.emit("knockEnded", { knockId, reason: "commit_failed" });
        const rejection = decodeMessage(message);
        const reason = rejection.type === MsgType.Reject ? rejectReasonName(rejection.reason) : "commit failed";
        throw new E2EEError(reason === "expired" ? "unknown_knock" : "conflict", reason, { reason });
      }
      this.#completeStep(k.info.peerId, k.adm);
    } catch (e) {
      if (!ended) {
        // Storage failure can end a verified approval too. Never leave the viewer waiting.
        this.#sendAdmission(k.adm, k.adm.rejectVerified());
        this.emit("knockEnded", { knockId, reason: "commit_failed" });
      }
      if (this.#handshakes.get(k.info.peerId) === k.adm) this.#handshakes.delete(k.info.peerId);
      throw e;
    } finally {
      this.#committingCodes.delete(k.info.rowCode);
    }
  }

  deny(knockId: string): void {
    const k = this.#knocks.get(knockId);
    if (!k) throw new E2EEError("unknown_knock", "knock has ended or never existed");
    this.#endKnock(k, "denied");
  }

  /** Stop is terminal for this host; no timer or recovery can reopen it. */
  async close(): Promise<void> {
    this.#stopped = true;
    this.#stopHold ??= this.keys.hold();
    for (const k of this.#knocks.values()) this.#endKnock(k, "stopped");
    for (const adm of this.#handshakes.values()) await adm.abort("stopped");
    this.#handshakes.clear();
    for (const id of this.#byPeer.values()) await this.#disconnect(id);
  }

  /** Attach a viewer-reported display label (unauthenticated, display only). */
  setViewerReportedLabel(admissionId: string, label: string | null): void {
    const r = this.#records.get(admissionId);
    if (!r) throw new E2EEError("invalid_argument", "unknown admission");
    if (label !== null && (label.length > MAX_VIEWER_LABEL_LENGTH || /\p{Cc}/u.test(label))) {
      throw new E2EEError("invalid_argument", "label too long or contains control characters");
    }
    r.label = label;
  }

  /**
   * Seal a batch of origin-stamped annotation records (PROTOCOL §20) for one
   * connected admission and send it to its peer. Returns false if the
   * admission isn't connected (or was kicked meanwhile); nothing is sent then.
   */
  /**
   * May this admission annotate (and receive annotations) right now? False
   * from terminal stop or an owner kick of its grant, before queued cleanup.
   */
  annotationAllowed(admissionId: string): boolean {
    const r = this.#records.get(admissionId);
    return !this.#stopped && !!r && r.connected && !r.kickIncomplete && !this.#annotationBlocked.has(r.grantId) && this.#byPeer.get(r.peerId) === admissionId && this.keys.hasSession(admissionId);
  }

  async sendAnnotations(admissionId: string, records: readonly AnnotationRecord[], current: () => boolean = () => true): Promise<boolean> {
    if (!current() || !this.annotationAllowed(admissionId)) return false;
    const r = this.#records.get(admissionId)!;
    let bytes: Bytes;
    try {
      bytes = await this.keys.sealAnnotations(admissionId, records);
    } catch (e) {
      if (isE2EEError(e, "unknown_admission")) return false;
      throw e;
    }
    if (!current() || !this.annotationAllowed(admissionId)) return false;
    this.#o.send(r.peerId, bytes);
    return true;
  }

  /** Feed one ordered relay message from a peer. Never throws for bad input. */
  async receive(peerId: string, bytes: Uint8Array): Promise<void> {
    let adm = this.#handshakes.get(peerId);
    try {
      const type = peekType(bytes);
      if (type === MsgType.Hello) {
        // A HELLO only replaces an unfinished handshake on this transport. It
        // never removes an admission: the relay can forge HELLOs.
        const previous = adm;
        adm = new SenderAdmission(this.#ctx, peerId);
        this.#handshakes.set(peerId, adm);
        // Teardown may synchronously produce a denial. The replacement already
        // owns the wire, so that old transcript must no longer send to this peer.
        this.#endPeerKnock(peerId, "replaced");
        await previous?.abort("restarted");
        if (this.#handshakes.get(peerId) !== adm) return;
        await this.#step(peerId, adm, bytes);
      } else if (type === MsgType.Response) {
        adm = this.#handshakes.get(peerId);
        if (!adm) throw new E2EEError("unexpected_message", "no handshake for peer");
        await this.#step(peerId, adm, bytes);
      } else if (type === MsgType.RekeyAck) {
        const id = this.#byPeer.get(peerId);
        if (!id) throw new E2EEError("unexpected_message", "no admitted session on this peer");
        await this.keys.handle(id, bytes);
      } else if (type === MsgType.Annotation) {
        const id = this.#byPeer.get(peerId);
        const rec = id ? this.#records.get(id) : undefined;
        if (!id || !rec || !this.annotationAllowed(id)) throw new E2EEError("unexpected_message", "no admitted session on this peer");
        const { epoch, body } = await this.keys.openAnnotation(id, bytes);
        // Kicked (requested or done), or superseded on this transport, during the decrypt.
        if (this.#byPeer.get(peerId) !== id || !this.annotationAllowed(id)) throw new E2EEError("unexpected_message", "admission ended");
        this.emit("annotation", { admissionId: id, viewerId: rec.viewerId, rowCode: rec.rowCode, epoch, body });
      } else {
        throw new E2EEError("unexpected_message", `sender does not accept type ${type}`);
      }
    } catch (e) {
      const err = isE2EEError(e) ? e : new E2EEError("malformed", String(e));
      if (adm && this.#handshakes.get(peerId) === adm && adm.state !== "committing") {
        this.#endPeerKnock(peerId, "protocol_error");
        await adm.abort("protocol_error");
        if (this.#handshakes.get(peerId) === adm) this.#handshakes.delete(peerId);
      }
      this.emit("protocolError", { peerId, error: err });
    }
  }

  /** Every handshake reply, including knock teardown/approval, owns one transcript. */
  #sendAdmission(adm: SenderAdmission, bytes: Bytes): void {
    if (this.#handshakes.get(adm.peerId) === adm) this.#o.send(adm.peerId, bytes);
  }

  async #step(peerId: string, adm: SenderAdmission, bytes: Uint8Array): Promise<void> {
    const out = await adm.handle(bytes);
    for (const m of out) this.#sendAdmission(adm, m);
    if (adm.state === "pending") {
      const k = [...this.#knocks.values()].find((k) => k.adm === adm);
      if (k) this.emit("knock", { ...k.info });
    }
    this.#completeStep(peerId, adm);
  }

  #completeStep(peerId: string, adm: SenderAdmission): void {
    const o = adm.outcome;
    if (!o || adm.state !== "done") return;
    if (this.#handshakes.get(peerId) === adm) this.#handshakes.delete(peerId);
    if (o.kind === "admitted") this.emit("admitted", { admissionId: o.admissionId, peerId, viaGrant: o.mode === AdmissionMode.Grant });
    else if (o.kind === "rejected") this.emit("rejected", { peerId, reason: o.reason });
  }

  /** Re-validate a verified handshake and register it. See SenderAdmissionContext.commit. */
  #commit(adm: SenderAdmission, c: AdmissionCommit): Promise<CommitResult> {
    return this.#serial(async (): Promise<CommitResult> => {
      // A revocation that is only in memory would not survive a restart: admit nobody until it is stored.
      if (this.#blocked()) return { ok: false, reason: RejectReason.Locked };
      let grant: Grant;
      let issued = false;
      if (c.mode === AdmissionMode.Grant) {
        const usable = c.grant !== null && !this.#revokedGrants.has(toBase64Url(c.grant.id));
        const current = usable ? await this.grants.lookup(c.grant!.id, c.viewerSid) : null;
        if (!current || !c.grant) return { ok: false, reason: RejectReason.GrantInvalid };
        current.secret.fill(0);
        grant = c.grant;
      } else {
        if (c.pinGeneration !== this.#pinGeneration) return { ok: false, reason: RejectReason.Expired };
        if (this.#locked) return { ok: false, reason: RejectReason.Locked };
        if ((await this.budget.status()).frozen) return { ok: false, reason: RejectReason.Frozen };
        grant = await this.grants.issue(c.viewerSid, c.rowCode ?? await this.#newRowCode());
        issued = true;
      }
      // No awaits from here to registration: these checks and the
      // registration are one atomic step with respect to everything else
      // (lock() and kicks act synchronously, so they are re-checked here).
      let refused: RejectReasonId | null = null;
      if (this.#handshakes.get(adm.peerId) !== adm) refused = RejectReason.Expired;
      else if (this.#blocked()) refused = RejectReason.Locked;
      else if (c.mode === AdmissionMode.Pin && c.pinGeneration !== this.#pinGeneration) refused = RejectReason.Expired;
      else if (c.mode === AdmissionMode.Pin && this.#locked) refused = RejectReason.Locked;
      else if (c.mode === AdmissionMode.Pin && this.#frozen) refused = RejectReason.Frozen;
      else if (c.mode === AdmissionMode.Grant && this.#revokedGrants.has(toBase64Url(grant.id))) refused = RejectReason.GrantInvalid;
      else if (!c.claim()) refused = RejectReason.Expired;
      if (refused !== null) {
        if (issued) await this.grants.revoke(grant.id);
        return { ok: false, reason: refused };
      }
      const id = toBase64Url(randomBytes(16));
      const prev = this.#byPeer.get(adm.peerId);
      const rec: AdmissionRecord = {
        viewerId: toBase64Url(grant.viewerId),
        rowCode: grant.rowCode,
        id,
        grantId: toBase64Url(grant.id),
        grantIdBytes: copy(grant.id),
        grantExpiresAt: grant.expiresAt,
        viaGrant: c.mode === AdmissionMode.Grant,
        admittedAt: this.#now(),
        peerId: adm.peerId,
        connected: true,
        label: null,
        kickIncomplete: false,
        closePending: false,
      };
      this.#records.set(id, rec);
      this.#byPeer.set(adm.peerId, id);
      // A new admission on the same transport supersedes the old one's routing.
      if (prev) await this.#disconnect(prev);
      const session = new SenderSession({ id, viewerSid: c.viewerSid, grantId: grant.id, keys: c.keys });
      try {
        const epoch = await this.keys.admit(session);
        return { ok: true, admissionId: id, session, epoch, grant };
      } finally {
        // Terminal close can precede queued registration, even if registration fails.
        if (!rec.connected) await this.keys.remove(id);
      }
    });
  }

  async #disconnect(admissionId: string): Promise<void> {
    const r = this.#records.get(admissionId);
    if (!r || !r.connected) return;
    r.connected = false;
    if (this.#byPeer.get(r.peerId) === admissionId) this.#byPeer.delete(r.peerId);
    await this.keys.remove(admissionId);
    this.emit("disconnected", { admissionId, peerId: r.peerId });
  }

  /** Relay peer closed: an unfinished handshake counts as an abandoned guess. */
  peerClosed(peerId: string): Promise<void> {
    this.#endPeerKnock(peerId, "peer_closed");
    const adm = this.#handshakes.get(peerId);
    // Abort before queueing so a commit waiting on storage observes the close.
    void adm?.abort("closed");
    return this.#serial(async () => {
      await adm?.abort("closed");
      if (this.#handshakes.get(peerId) === adm) this.#handshakes.delete(peerId);
      const id = this.#byPeer.get(peerId);
      if (id) await this.#disconnect(id);
    });
  }

  /**
   * The in-memory, synchronous part of a kick: every record admitted with
   * the same grant as `admissionId` stops being routed, the grant is refused
   * by commit(), and the key manager drops their channel keys and starts its
   * cutoff (they never get another epoch). Nothing here can fail halfway.
   */
  #evict(admissionId: string): { victims: AdmissionRecord[]; rekeyed: Promise<void> } {
    const rec = this.#records.get(admissionId);
    if (!rec) throw new E2EEError("invalid_argument", "unknown admission");
    const victims = [...this.#records.values()].filter((r) => r.grantId === rec.grantId);
    this.#revokedGrants.add(rec.grantId);
    for (const v of victims) {
      if (v.connected) v.closePending = true;
      v.connected = false;
      v.kickIncomplete = true;
      if (this.#byPeer.get(v.peerId) === v.id) this.#byPeer.delete(v.peerId);
    }
    const rekeyed = this.keys.kick(victims.map((v) => v.id));
    // Handled by the caller; never an unhandled rejection in the meantime.
    rekeyed.catch(() => undefined);
    return { victims, rekeyed };
  }

  /**
   * kick() and kickAndRotatePin(). The key manager's cutoff starts at the
   * call, not when the action reaches the front of the queue. Revocation is
   * in memory first, then persisted; the records go only when everything
   * succeeded, so a failed kick can be retried (fail closed meanwhile).
   */
  #ownerKick(admissionId: string, rotate: { pin: string | null; version?: number } | null): Promise<string[]> {
    try {
      if (!this.#records.has(admissionId)) throw new E2EEError("invalid_argument", "unknown admission");
      if (rotate && this.#rotationCall) throw new E2EEError("rotation_pending", "another rotation call is applying");
      if (rotate) this.#assertPinChanges(rotate.pin, rotate.version);
    } catch (e) {
      return Promise.reject(e);
    }
    if (rotate) this.#rotationCall = true;
    const release = this.keys.beginRevocation();
    // Annotation access ends now, synchronously, not when the kick reaches the
    // front of the queue: ink in flight is refused after its decrypt too.
    const target = this.#records.get(admissionId)!;
    this.#annotationBlocked.add(target.grantId);
    this.emit("annotationRevoked", { admissionIds: [...this.#records.values()].filter((r) => r.grantId === target.grantId).map((r) => r.id), rowCode: target.rowCode });
    const result = this.#serial(async () => {
      let evicted: { victims: AdmissionRecord[]; rekeyed: Promise<void> };
      let durable: Promise<boolean>;
      try {
        // Re-checked inside the lock, before any mutation.
        if (rotate) this.#assertPinChanges(rotate.pin, rotate.version);
        const rec = this.#records.get(admissionId);
        if (!rec) throw new E2EEError("invalid_argument", "unknown admission");
        // Write-ahead: the durable revocation is handed to storage before memory changes.
        const expiresAt = Math.max(...[...this.#records.values()].filter((r) => r.grantId === rec.grantId).map((r) => r.grantExpiresAt));
        durable = this.#persistRevocation(rotate ? { bump: true, pinVersion: this.#rotationVersion(rotate.version) } : { grants: [{ id: rec.grantIdBytes, expiresAt }] });
        evicted = this.#evict(admissionId);
      } finally {
        release();
      }
      const { victims, rekeyed } = evicted;
      const grantId = victims[0]!.grantId;
      for (const [peer, adm] of this.#handshakes) {
        if (adm.grantId === grantId) {
          const reply = await adm.abort("kicked", RejectReason.GrantInvalid);
          if (reply) this.#sendAdmission(adm, reply);
          if (this.#handshakes.get(peer) === adm) this.#handshakes.delete(peer);
        }
      }
      const rotating = rotate ? this.#rotate(rotate.pin, rotate.version, durable) : null;
      const ok = await durable;
      const results = await Promise.allSettled([rotating ?? Promise.resolve(), rekeyed]);
      for (const result of results) if (result.status === "rejected") throw result.reason;
      if (!rotate) {
        await this.grants.revoke(victims[0]!.grantIdBytes);
        if (!ok) throw SenderHost.#notDurable();
      }
      this.#revokedGrants.delete(grantId);
      for (const v of victims) this.#records.delete(v.id);
      this.emit("kicked", { admissionIds: victims.map((v) => v.id), peerIds: victims.filter((v) => v.closePending).map((v) => v.peerId) });
      return victims.map((v) => v.id);
    });
    return rotate ? result.finally(() => { this.#rotationCall = false; }) : result;
  }

  /**
   * Remove a viewer: revoke its grant, drop every connection and pending
   * handshake admitted with that grant, and move everyone else to a fresh
   * epoch within `fastAckTimeoutMs` (300 ms) of this call. If no fresh epoch
   * can be active by then, encryption stops (`onSuspend`) until one is.
   *
   * A kicked viewer who knows the (unchanged) PIN can run a new PIN
   * admission and get a new grant. Use kickAndRotatePin() to keep them out.
   */
  kick(admissionId: string): Promise<string[]> {
    return this.#ownerKick(admissionId, null);
  }

  /**
   * kick() and rotatePin() as one atomic owner action, with one fast rekey.
   * The new PIN must differ from the current one.
   */
  kickAndRotatePin(admissionId: string, pin: string | null, o: { version?: number } = {}): Promise<string[]> {
    if (pin !== null) normalizePin(pin);
    return this.#ownerKick(admissionId, { pin, version: o.version });
  }

  /**
   * New PIN: in-flight handshakes end, a handshake that verified against the
   * old PIN can no longer commit, all grants are revoked, the budget resets,
   * keys rotate. Admitted sessions stay (use kickAndRotatePin to remove one).
   * The new PIN must differ from the current one (compared after NFC
   * normalisation); unlock({ keepPin: true }) is the way to keep it.
   */
  rotatePin(pin: string | null, o: { version?: number } = {}): Promise<void> {
    if (pin !== null) normalizePin(pin);
    if (this.#rotationCall) return Promise.reject(new E2EEError("rotation_pending", "another rotation call is applying"));
    this.#rotationCall = true;
    return this.#serial(async () => {
      await this.#rotate(pin, o.version);
      await this.keys.rekey("pin_rotated");
    }).finally(() => { this.#rotationCall = false; });
  }

  /**
   * An unchanged PIN is not a rotation: it would reset the guess budget and
   * let a kicked viewer back in. Allowed only to finish a rotation to that
   * same PIN whose persistence failed.
   */
  #rotationVersion(version?: number): number { return version ?? this.#rotationTarget?.version ?? (this.#versionMismatch ? Math.max(this.#pinVersion, this.#fencePinVersion) + 1 : this.#pinVersion + (this.#rotationDone ? 1 : 0)); }

  #assertPinChanges(pin: string | null, version?: number): void {
    const next = this.#rotationVersion(version);
    if (!Number.isSafeInteger(next) || next < 1) throw new E2EEError("invalid_argument", "invalid PIN version");
    if (this.#rotationTarget) {
      if (next !== this.#rotationTarget.version || pin?.normalize("NFC") !== this.#rotationTarget.pin?.normalize("NFC")) throw new E2EEError("rotation_pending", "another rotation is pending");
      return;
    }
    if (!this.#versionMismatch && !this.#rotationDone) {
      if (next === this.#pinVersion && pin?.normalize("NFC") === this.#pin?.normalize("NFC")) return;
      throw new E2EEError("rotation_pending", "finish the pending PIN version and value before changing them");
    }
    if (next <= Math.max(this.#pinVersion, this.#fencePinVersion)) throw new E2EEError("invalid_argument", "PIN version must increase");
    const cur = this.#pin;
    const same = pin === null || cur === null ? pin === cur : pin.normalize("NFC") === cur.normalize("NFC");
    if (same) throw new E2EEError("invalid_argument", "the new PIN equals the current one (use unlock({ keepPin: true }) to keep it)");
  }

  /**
   * `fence`: kickAndRotatePin already wrote the generation bump ahead of its
   * eviction. Otherwise the bump is written here, before anything changes.
   */
  async #rotate(pin: string | null, version?: number, fence?: Promise<boolean>): Promise<void> {
    this.#assertPinChanges(pin, version);
    const next = this.#rotationVersion(version);
    this.#rotationTarget = { pin, version: next };
    // Write-ahead: every grant issued so far is refused once this lands.
    const durable = fence ?? this.#persistRevocation({ bump: true, pinVersion: next });
    // In memory: old-PIN handshakes can no longer commit from here on.
    this.#rotationDone = false;
    this.#pinVersion = next;
    this.#fencePinVersion = next;
    this.#versionMismatch = false;
    this.#pin = pin;
    this.#pinGeneration++;
    for (const k of this.#knocks.values()) this.#endKnock(k, "pin_rotated");
    for (const [id, adm] of this.#handshakes) {
      const reply = await adm.abort("pin_rotated", adm.grantId === null ? RejectReason.Expired : RejectReason.GrantInvalid);
      if (reply) this.#sendAdmission(adm, reply);
      if (this.#handshakes.get(id) === adm) this.#handshakes.delete(id);
    }
    const ok = await durable;
    // Housekeeping: the generation already refuses these grants.
    await this.grants.revokeAll();
    // Safe even if the bump is not durable yet: then nobody is admitted until it is.
    this.#revokedGrants.clear();
    await this.budget.unlock();
    // Disconnected admissions can never come back: their grants are gone.
    // An incomplete kick's records stay until a kick() retry reports them.
    for (const [id, r] of this.#records) if (!r.connected && !r.kickIncomplete) this.#records.delete(id);
    if (!ok) throw SenderHost.#notDurable();
    await this.grants.completeRotation(next);
    this.#rotationDone = true;
    this.#rotationTarget = null;
  }

  lock(locked: boolean): void {
    this.#locked = locked;
  }

  /**
   * Unfreeze PIN admissions after the guess budget ran out. By default this
   * rotates the PIN, because unlocking with the old one hands an attacker
   * another full budget of guesses at it. `{ keepPin: true }` is the explicit
   * override.
   */
  unlock(o: UnlockOptions): Promise<void> {
    if (o && "newPin" in o) return this.rotatePin(o.newPin, { version: o.version });
    if (o && "keepPin" in o && o.keepPin === true) return this.#serial(async () => {
      if (this.rotationPending || this.#reconciling) throw new E2EEError("rotation_pending", "PIN reconciliation is pending");
      // Like a rotation, cancel held handshakes first: their reservations would
      // otherwise keep pacing the next legitimate viewer and, on timeout,
      // refill the failures this unlock just cleared.
      for (const [id, adm] of this.#handshakes) {
        await adm.abort("unlocked");
        if (this.#handshakes.get(id) === adm) this.#handshakes.delete(id);
      }
      await this.budget.unlock();
    });
    return Promise.reject(new E2EEError("invalid_argument", "unlock needs { newPin } or { keepPin: true }"));
  }

  /**
   * Call when the sender's encryption worker is replaced (crash, reload of
   * the worker, a new SenderKeyRing). Terminate the old worker first. The new
   * one must not be given an epoch the old one used, or SFrame counters would
   * restart at 0 under the same key (nonce reuse). This activates a fresh
   * epoch within `fastAckTimeoutMs`; `onEpoch` delivers it. Frames are dropped
   * (fail closed) until then.
   */
  replaceEncryptor(): Promise<void> {
    return this.keys.replaceEncryptor();
  }

  /**
   * Timeouts: storage operations, handshakes, budget reservations, rekey
   * acks, the 10-minute rekey, old records; and retries of a revocation that
   * is not durable yet.
   */
  async tick(): Promise<void> {
    this.#storage.check();
    const now = this.#now();
    const f = this.#fence;
    if (f.release && !f.retrying && now >= f.retryAt) {
      f.retrying = true;
      await this.#flushFence().finally(() => {
        f.retrying = false;
      });
    }
    for (const k of this.#knocks.values()) if (now >= k.info.expiresAt) this.#endKnock(k, "timeout");
    for (const [id, adm] of this.#handshakes) {
      if (adm.state === "challenged" && now - adm.startedAt > this.budget.limits.handshakeTimeoutMs) {
        await adm.abort("timeout");
        // Persistent settlement may yield to a new HELLO on this peer.
        if (this.#handshakes.get(id) === adm) this.#handshakes.delete(id);
      }
    }
    for (const [id, r] of this.#records) if (!r.connected && r.grantExpiresAt <= now) this.#records.delete(id);
    await this.budget.status();
    await this.keys.tick();
  }

  /**
   * Convenience timers. The key manager ticks every `keyTickMs` so a kick
   * switches epochs close to its 300 ms deadline; the rest every `tickMs`.
   * Returns a stop function.
   */
  startTimers(o: { tickMs?: number; keyTickMs?: number } = {}): () => void {
    const a = setInterval(() => void this.tick(), o.tickMs ?? 250);
    const b = setInterval(() => void this.keys.tick(), o.keyTickMs ?? 50);
    return () => {
      clearInterval(a);
      clearInterval(b);
    };
  }
}
