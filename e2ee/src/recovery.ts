/** Page-owned PIN intent and latch recovery (HOST-CONTROL §5.3–5.6).
 * The daemon mirror and capture-track detachment remain the caller's job.
 */
import { normalizePin } from "./admission.ts";
import { randomBytes } from "./bytes.ts";
import { E2EEError, isE2EEError } from "./errors.ts";
import { Emitter } from "./events.ts";
import { SenderHost, type SenderHostOptions } from "./host.ts";
import { type AtomicStorage, type Json, retryDelayMs, TimedStorage } from "./storage.ts";

export type PinPolicy = { kind: "digits"; length: number } | { kind: "passphrase" } | { kind: "none" };
export type Latches = { locked: boolean; paused: boolean; frozen: boolean };
export type PinState = { version: number; policy: PinPolicy; pin: string | null };
export type RotationIntent = PinState & { op: "rotate" | "unlock" | "kick"; admission_id?: string; repair?: boolean };
export type RecoveryRecord = { pin_state: PinState | null; pin_mismatch?: boolean; pending_rotation: RotationIntent | null; latches: Latches; knock: boolean };
export type RotationState = "idle" | "intent_persisted" | "applying" | "committing" | "retrying" | "mismatch";
export const CLEAR_LATCHES: Latches = { locked: false, paused: false, frozen: false };

export function mergeLatches(a: Latches, b: Latches): Latches {
  return { locked: a.locked || b.locked, paused: a.paused || b.paused, frozen: a.frozen || b.frozen };
}

function samePin(a: string | null, b: string | null): boolean { return a?.normalize("NFC") === b?.normalize("NFC"); }

export function validatePinPolicy(policy: PinPolicy, pin: string | null): void {
  if (!policy || typeof policy !== "object") throw new E2EEError("invalid_argument", "invalid PIN policy");
  if (policy.kind === "none") {
    if (pin !== null) throw new E2EEError("invalid_argument", "no-PIN policy requires null");
  } else if (policy.kind === "digits") {
    if (!Number.isInteger(policy.length) || policy.length < 4 || policy.length > 12 || typeof pin !== "string" || !new RegExp(`^[0-9]{${policy.length}}$`).test(pin)) throw new E2EEError("invalid_argument", "PIN must match digit policy");
  } else if (policy.kind === "passphrase") {
    if (pin === null || pin === undefined) throw new E2EEError("passphrase_required", "passphrase required");
    if (typeof pin !== "string" || normalizePin(pin).length < 8) throw new E2EEError("invalid_argument", "passphrase must be 8–256 bytes after NFC");
  } else throw new E2EEError("invalid_argument", "invalid PIN policy");
}

/** Uniform independent decimal digits; no floating-point range or modulo bias. */
export function generatePin(length: number, current: string | null = null): string {
  if (!Number.isInteger(length) || length < 4 || length > 12) throw new E2EEError("invalid_argument", "digit PIN length must be 4–12");
  for (;;) {
    let pin = "";
    while (pin.length < length) for (const n of randomBytes(length)) {
      if (n < 250 && pin.length < length) pin += String(n % 10);
    }
    if (!samePin(pin, current)) return pin;
  }
}

export type Inspection = Awaited<ReturnType<typeof SenderHost.inspect>>;
export function reconcilePin(record: RecoveryRecord, library: Inspection): "agree" | "apply_old" | "apply_pending" | "finalize" | "mismatch" {
  const v = record.pin_state?.version ?? -1;
  const p = record.pending_rotation;
  if (!Number.isSafeInteger(v) || (v < 0 && record.pin_state !== null) || (library.pinVersion < 0 && !p?.repair)) return "mismatch";
  if ((!record.pin_state || record.pin_mismatch) && !p?.repair) return "mismatch";
  if (!p) return library.pinVersion === v && library.rotationComplete ? "agree" : "mismatch";
  if (p.repair && p.version > v && record.latches.locked && record.latches.frozen) {
    if (library.pinVersion < p.version) return "apply_old";
    if (library.pinVersion === p.version) return library.rotationComplete ? "finalize" : "apply_pending";
    return "mismatch";
  }
  if (p.version !== v + 1) return "mismatch";
  if (library.pinVersion === v && library.rotationComplete) return "apply_old";
  if (library.pinVersion === p.version) return library.rotationComplete ? "finalize" : "apply_pending";
  return "mismatch";
}

export interface RotateOptions {
  policy: PinPolicy;
  value?: string | null;
  op?: RotationIntent["op"];
  admissionId?: string;
}
export interface RecoveryStatus { state: RotationState; reconciling: boolean; knockMismatch: boolean }
interface RecoveryEvents extends Record<string, unknown> { stateChanged: RecoveryStatus }

export interface RecoveryOptions {
  storage: AtomicStorage;
  streamId: string;
  /** Required only on first launch. Missing state on recovery is fatal. */
  initial?: { pin: string | null; policy: PinPolicy; version?: number; knock?: boolean; latches?: Latches };
  init?: { knock: boolean; latches: Latches };
  host: Omit<SenderHostOptions, "storage" | "streamId" | "pin" | "pinVersion" | "knock" | "locked" | "reconciling">;
  /** Apply pause by detaching ALL RTP tracks. Called synchronously before setting writes. */
  onLatches?(latches: Latches): void;
  onMismatch?(): void;
  /** Called after state and gates agree, including initial recovery. */
  onStateChange?(status: RecoveryStatus): void;
}

function object(j: Json | undefined): j is { [key: string]: Json } { return typeof j === "object" && j !== null && !Array.isArray(j); }
function readPolicy(j: Json | undefined): PinPolicy {
  if (object(j)) {
    if (j.kind === "none") return { kind: "none" };
    if (j.kind === "passphrase") return { kind: "passphrase" };
    if (j.kind === "digits" && typeof j.length === "number") return { kind: "digits", length: j.length };
  }
  throw new E2EEError("state_mismatch", "corrupt PIN policy");
}
function readPinState(j: Json | undefined): PinState {
  if (!object(j) || typeof j.version !== "number" || !Number.isSafeInteger(j.version) || j.version < 0 || (j.pin !== null && typeof j.pin !== "string")) throw new E2EEError("state_mismatch", "corrupt PIN state");
  const state = { version: j.version, policy: readPolicy(j.policy), pin: j.pin };
  validatePinPolicy(state.policy, state.pin);
  return state;
}
function parseRecord(j: Json | undefined): RecoveryRecord {
  if (!object(j) || typeof j.knock !== "boolean" || !object(j.latches)) throw new E2EEError("state_mismatch", "recovery record missing or corrupt");
  const l = j.latches;
  if (typeof l.locked !== "boolean" || typeof l.paused !== "boolean" || typeof l.frozen !== "boolean") throw new E2EEError("state_mismatch", "corrupt latches");
  const pin_state = j.pin_mismatch === true && j.pin_state === null ? null : readPinState(j.pin_state);
  let pending_rotation: RotationIntent | null = null;
  if (j.pending_rotation !== null) {
    const p = j.pending_rotation;
    if (!object(p) || (p.op !== "rotate" && p.op !== "unlock" && p.op !== "kick") || (p.op === "kick" && typeof p.admission_id !== "string")) throw new E2EEError("state_mismatch", "corrupt rotation intent");
    pending_rotation = { ...readPinState(p), op: p.op, ...(typeof p.admission_id === "string" ? { admission_id: p.admission_id } : {}), ...(p.repair === true ? { repair: true } : {}) };
    if (pin_state && samePin(pin_state.pin, pending_rotation.pin)) throw new E2EEError("state_mismatch", "unchanged pending PIN");
  }
  return { pin_state, ...(j.pin_mismatch === true ? { pin_mismatch: true } : {}), pending_rotation, knock: j.knock, latches: { locked: l.locked, paused: l.paused, frozen: l.frozen } };
}

/** Owns one running host and its page recovery record. Drive tick() with the host timer. */
export class HostRecovery extends Emitter<RecoveryEvents> {
  readonly host: SenderHost;
  readonly #o: RecoveryOptions;
  readonly #storage: TimedStorage;
  readonly #key: string;
  #record: RecoveryRecord;
  #state: RotationState = "idle";
  #retryAt = 0;
  #attempt = 0;
  #latchesDirty = false;
  #busy = false;
  #libraryVersion = 0;
  #generation = 0;
  #revision = 0;
  #knockMismatch = false;
  #terminalError: E2EEError | null = null;
  #terminalRepair = false;

  private constructor(o: RecoveryOptions, host: SenderHost, record: RecoveryRecord, storage: TimedStorage) {
    super();
    this.#o = o; this.host = host; this.#record = record; this.#storage = storage; this.#key = `ps:stream:${o.streamId}`;
    host.budget.on("frozen", () => {
      // Budget freeze is already durable before this callback.
      this.#record.latches.frozen = true;
      this.#applyLatches();
      this.#latchesDirty = true;
    });
  }

  static async create(o: RecoveryOptions): Promise<HostRecovery> {
    const storage = new TimedStorage(o.storage, o.host.now ?? Date.now, o.host.storageTimeoutMs);
    const key = `ps:stream:${o.streamId}`;
    const raw = await storage.get(key);
    let r: RecoveryRecord;
    let corrupt = false;
    try {
      if (raw === undefined && o.initial) {
        validatePinPolicy(o.initial.policy, o.initial.pin);
        r = { pin_state: { version: o.initial.version ?? 0, policy: o.initial.policy, pin: o.initial.pin }, pending_rotation: null, knock: o.initial.knock ?? false, latches: o.initial.latches ?? { ...CLEAR_LATCHES } };
      } else r = parseRecord(raw);
    } catch (e) {
      if (raw === undefined) throw e; // Lost secrets cannot be rebuilt.
      corrupt = true;
      r = { pin_state: null, pin_mismatch: true, pending_rotation: null, knock: true, latches: { locked: true, paused: true, frozen: true } };
    }
    const inspection = await SenderHost.inspect(storage, o.streamId);
    r.latches = mergeLatches(r.latches, o.init?.latches ?? CLEAR_LATCHES);
    r.latches.frozen ||= inspection.frozen;
    r.knock ||= o.init?.knock ?? false;
    const action = corrupt ? "mismatch" : reconcilePin(r, inspection);
    if (action === "mismatch") { r.latches = { ...r.latches, locked: true, frozen: true }; r.pin_mismatch = true; }
    o.onLatches?.({ ...r.latches });
    const initialized = await storage.update(key, (cur) => ({
      ...(object(cur) ? cur : {}), ...r,
      host_control_generation: (object(cur) && typeof cur.host_control_generation === "number" ? cur.host_control_generation : 0) + 1,
      host_control_revision: 0,
    }));
    const pinState = action === "apply_pending" || action === "finalize" ? r.pending_rotation! : r.pin_state;
    const host = await SenderHost.create({ ...o.host, storage: o.storage, streamId: o.streamId, pin: pinState?.pin ?? null, pinVersion: pinState?.version ?? 0, knock: r.knock, locked: r.latches.locked || r.latches.frozen, reconciling: action !== "agree" });
    const recovery = new HostRecovery(o, host, r, storage);
    recovery.#libraryVersion = inspection.pinVersion;
    if (!object(initialized) || typeof initialized.host_control_generation !== "number") throw new E2EEError("state_mismatch", "invalid recovery generation");
    recovery.#generation = initialized.host_control_generation;
    if (action === "mismatch") recovery.#mismatch();
    else if (action === "finalize") {
      recovery.#setState("committing");
      try { await recovery.#finish(); } catch (e) { recovery.#retry(e); }
    } else if (action !== "agree") await recovery.#apply(true).catch(() => undefined);
    recovery.#notify();
    return recovery;
  }

  get record(): RecoveryRecord { return structuredClone(this.#record); }
  get state(): RotationState { return this.#state; }
  get reconciling(): boolean { return this.#state !== "idle"; }
  get knockMismatch(): boolean { return this.#knockMismatch; }
  #notify(): void {
    const status = { state: this.state, reconciling: this.reconciling, knockMismatch: this.knockMismatch };
    this.emit("stateChanged", status);
    try { this.#o.onStateChange?.(status); } catch { /* UI failures cannot interrupt recovery. */ }
  }
  #setState(state: RotationState): void { this.#state = state; this.#notify(); }
  #now(): number { return (this.#o.host.now ?? Date.now)(); }
  async #persist(value: RecoveryRecord = this.#record): Promise<void> {
    const snapshot = structuredClone(value);
    const revision = ++this.#revision;
    const generation = this.#generation;
    await this.#storage.update(this.#key, (cur) => {
      if (!object(cur) || cur.host_control_generation !== generation) throw new E2EEError("state_mismatch", "recovery writer superseded");
      // A timed-out operation may land after a newer write, or after reload.
      if (typeof cur.host_control_revision === "number" && cur.host_control_revision >= revision) return cur;
      return { ...cur, ...snapshot, host_control_revision: revision };
    });
  }
  #applyLatches(): void {
    this.host.lock(this.#record.latches.locked || this.#record.latches.frozen);
    this.#o.onLatches?.({ ...this.#record.latches });
  }
  #mismatch(): void {
    this.#record.pin_mismatch = true;
    this.#record.latches.locked = this.#record.latches.frozen = true;
    this.host.enterStateMismatch();
    this.#applyLatches();
    this.#latchesDirty = true;
    this.#setState("mismatch");
    this.#o.onMismatch?.();
  }
  #retry(e: unknown): E2EEError {
    this.#setState("retrying");
    this.#retryAt = this.#now() + retryDelayMs(this.#attempt++);
    return new E2EEError("storage", String(e), { pending: true });
  }

  /** Policy values are explicit; omit value for generated digit policies. */
  async rotate({ policy, value, op = "rotate", admissionId }: RotateOptions): Promise<void> {
    if (this.#busy || (this.#state !== "idle" && this.#state !== "mismatch")) throw new E2EEError("rotation_pending", "PIN change is pending");
    let pin: string | null;
    if (policy?.kind === "digits") {
      if (value !== undefined) throw new E2EEError("invalid_argument", "generated policy takes no value");
      pin = generatePin(policy.length, this.#record.pin_state?.pin ?? null);
    } else pin = policy?.kind === "none" ? (value === undefined ? null : value) : (value ?? null);
    validatePinPolicy(policy, pin);
    if (this.#record.pin_state && samePin(pin, this.#record.pin_state.pin)) throw new E2EEError("invalid_argument", "new PIN equals current PIN");
    if (op === "kick" && !this.host.viewers().some((r) => r.admissionId === admissionId)) throw new E2EEError("unknown_admission", "unknown admission");
    if (this.#knockMismatch) throw new E2EEError("state_mismatch", "knock policy changed; reload before rotating");
    const repairing = this.#state === "mismatch";
    const previous = this.#record.pending_rotation;
    const version = Math.max(this.#record.pin_state?.version ?? -1, this.#libraryVersion, this.host.pinVersion, previous?.version ?? 0) + 1;
    this.#record.pending_rotation = { version, policy, pin: pin?.normalize("NFC") ?? null, op, ...(repairing ? { repair: true } : {}), ...(admissionId ? { admission_id: admissionId } : {}) };
    this.#busy = true;
    this.host.setReconciling(true);
    this.#setState("intent_persisted");
    try {
      await this.#persist();
      this.host.setReconciling(true);
      await this.#apply(false);
    } catch (e) {
      // An uncertain page write also counts as accepted: retry that exact intent.
      if (this.state === "intent_persisted") { this.host.setReconciling(true); throw this.#retry(e); }
      throw e;
    } finally { this.#busy = false; }
  }

  async #apply(retry: boolean): Promise<void> {
    const p = this.#record.pending_rotation!;
    this.#setState("applying");
    let before: Inspection | undefined;
    try {
      const state = before = await SenderHost.inspect(this.#storage, this.#o.streamId);
      this.#libraryVersion = state.pinVersion;
      // Completed library write + missing page commit: don't reset a budget twice.
      if (!(state.pinVersion === p.version && state.rotationComplete && this.host.rotationComplete && this.host.pinVersion === p.version)) {
        if (p.op === "kick" && this.host.viewers().some((r) => r.admissionId === p.admission_id)) await this.host.kickAndRotatePin(p.admission_id!, p.pin, { version: p.version });
        else {
          if (p.op === "kick" && !retry) throw new E2EEError("unknown_admission", "admission ended before rotation applied");
          await this.host.rotatePin(p.pin, { version: p.version });
        }
      }
      this.#setState("committing");
      await this.#finish();
    } catch (e) {
      let fence: Inspection;
      try { fence = await SenderHost.inspect(this.#storage, this.#o.streamId); }
      catch (inspectionError) { throw this.#retry(inspectionError); }
      this.#libraryVersion = fence.pinVersion;
      if (!isE2EEError(e) || isE2EEError(e, "storage")) throw this.#retry(e);
      if (fence.pinVersion === p.version) {
        if (!fence.rotationComplete || !this.host.rotationComplete || this.host.pinVersion !== p.version) throw this.#retry(e);
        this.#setState("committing");
        try { await this.#finish(); return; } catch (finishError) { throw this.#retry(finishError); }
      }
      if (!before || fence.pinVersion !== before.pinVersion || fence.rotationComplete !== before.rotationComplete) {
        this.#mismatch(); await this.#persist();
        throw new E2EEError("state_mismatch", "rotation disagrees with library fence");
      }
      if (retry) { this.#mismatch(); await this.#persist(); throw new E2EEError("state_mismatch", "rotation retry disagrees with library"); }
      this.#terminalError = e;
      this.#terminalRepair = p.repair ?? false;
      try { await this.#clearTerminal(); } catch (storageError) { throw this.#retry(storageError); }
      if (p.repair) this.#mismatch();
      else { this.host.setReconciling(false); this.#setState("idle"); }
      throw e;
    }
  }

  async #clearTerminal(): Promise<void> {
    await this.#persist({ ...this.#record, pending_rotation: null });
    this.#record.pending_rotation = null;
    this.#terminalError = null;
  }

  async #finish(): Promise<void> {
    const p = this.#record.pending_rotation!;
    const next: RecoveryRecord = { ...this.#record, latches: { ...this.#record.latches, frozen: p.op === "unlock" ? false : this.#record.latches.frozen }, pin_state: { version: p.version, policy: p.policy, pin: p.pin }, pin_mismatch: false, pending_rotation: null };
    // Leave the intent in memory until its final write is durable. Latch
    // changes may run concurrently, so don't replace their in-memory object.
    const previousPinState = this.#record.pin_state;
    const previousFrozen = this.#record.latches.frozen;
    const finalRevision = this.#revision + 1;
    await this.#persist(next);
    this.#record.pin_state = next.pin_state;
    this.#record.pending_rotation = null;
    this.#record.pin_mismatch = false;
    if (p.op === "unlock") this.#record.latches.frozen = false;
    if (this.#revision !== finalRevision) {
      try { await this.#persist(); }
      catch (e) {
        this.#record.pin_state = previousPinState;
        this.#record.pending_rotation = p;
        this.#record.pin_mismatch = p.repair ?? false;
        this.#record.latches.frozen ||= previousFrozen;
        throw e;
      }
    }
    this.#attempt = 0;
    this.host.setReconciling(false);
    this.#applyLatches();
    this.#setState("idle");
  }

  /** Local unfreeze: rotate by default; retaining the PIN must be explicit. */
  async unfreeze(o: { keepPin: true } | { policy: PinPolicy; value?: string | null }): Promise<void> {
    if ("keepPin" in o) {
      if (this.#busy || this.#state !== "idle" || this.#knockMismatch) throw new E2EEError("rotation_pending", "rotation or reconciliation is pending");
      await this.host.unlock({ keepPin: true });
      await this.#clearLatch("frozen");
    } else await this.rotate({ ...o, op: "unlock" });
  }

  /** Reconnect OR-merges policy. A running host's knock mode cannot change. */
  async mergeInit(init: { knock: boolean; latches: Latches }): Promise<void> {
    this.#record.latches = mergeLatches(this.#record.latches, init.latches);
    const addedKnock = init.knock && !this.#record.knock;
    if (addedKnock) {
      this.#record.knock = true;
      this.#knockMismatch = true;
      this.#record.latches.locked = true;
    }
    this.#applyLatches();
    if (addedKnock) { this.#notify(); this.#o.onMismatch?.(); }
    try { await this.#persist(); } catch (e) { this.#latchesDirty = true; throw new E2EEError("storage", String(e), { applied: true }); }
  }

  async setLatch(name: keyof Latches, value: boolean): Promise<void> {
    if (value) {
      this.#record.latches[name] = true;
      this.#applyLatches();
      try { await this.#persist(); } catch (e) { this.#latchesDirty = true; throw new E2EEError("storage", String(e), { applied: true }); }
    } else {
      if (name === "frozen") throw new E2EEError("invalid_argument", "use unfreeze to clear the frozen latch");
      await this.#clearLatch(name);
    }
  }

  async #clearLatch(name: keyof Latches): Promise<void> {
    if (this.#state !== "idle") throw new E2EEError("rotation_pending", "rotation or reconciliation is pending");
    if (name === "locked" && this.#knockMismatch) throw new E2EEError("state_mismatch", "reload to apply knock policy before unlocking");
    this.#record.latches[name] = false;
    try { await this.#persist(); }
    catch (e) {
      // Other latches may have been set while this write was in flight.
      this.#record.latches[name] = true;
      this.#applyLatches();
      this.#latchesDirty = true;
      throw new E2EEError("storage", String(e));
    }
    this.#applyLatches();
  }

  async tick(): Promise<void> {
    this.#storage.check();
    await this.host.tick();
    if (this.#busy || this.#now() < this.#retryAt) return;
    this.#busy = true;
    try {
      if (this.#latchesDirty) {
        try { await this.#persist(); this.#latchesDirty = false; } catch { this.#retryAt = this.#now() + retryDelayMs(this.#attempt++); return; }
      }
      if (this.#state === "retrying") {
        if (this.#terminalError) {
          try {
            await this.#clearTerminal();
            if (this.#terminalRepair) this.#mismatch();
            else { this.host.setReconciling(false); this.#setState("idle"); }
            this.#terminalRepair = false;
          }
          catch (e) { this.#retry(e); }
          return;
        }
        // Re-persist the intent first: even its initial write may have timed out.
        try { await this.#persist(); } catch (e) { this.#retry(e); return; }
        await this.#apply(true).catch(() => undefined);
      }
    } finally { this.#busy = false; }
  }
}
