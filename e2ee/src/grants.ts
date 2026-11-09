/**
 * Short-lived viewer session grants, held by the sender. A grant lets an
 * admitted viewer re-admit after a reload or reconnect without the PIN: the
 * grant secret (256-bit) replaces the PIN as the CPace password, so the
 * re-admission still has forward secrecy and still proves knowledge to the
 * sender only. Kicks revoke a viewer's grant; PIN rotation revokes all.
 *
 * Revocation is write-ahead. A separate record, the revocation fence
 * (`revocations:<streamId>`), holds a grant generation and tombstones:
 * - every grant is stamped with the generation current when it was issued,
 *   and a grant from an older generation is refused. A PIN rotation bumps
 *   the generation: one durable counter revokes every earlier grant;
 * - a kick writes a tombstone for the kicked grant.
 * SenderHost writes the fence before it evicts anyone in memory, and
 * lookup() checks it, so a revocation is in force from the moment its fence
 * write lands, even if the grant table itself was never cleaned (a failed
 * write, a crash). Cleaning the table (revoke, revokeAll, recover) is
 * housekeeping only.
 */
import { type Bytes, fromBase64Url, randomBytes, toBase64Url } from "./bytes.ts";
import { E2EEError } from "./errors.ts";
import { assignRowCode, rowCodeIndex } from "./row-code.ts";
import type { AtomicStorage, Json } from "./storage.ts";

export const DEFAULT_GRANT_TTL_MS = 8 * 3600_000;

export interface Grant {
  id: Bytes; // 16
  secret: Bytes; // 32
  viewerSid: Bytes; // 16
  viewerId: Bytes; // 16, grant-scoped
  rowCode: string;
  expiresAt: number; // ms since epoch
}

// Type aliases (not interfaces) so they are assignable to Json.
type Entry = {
  sid: string;
  secret: string;
  exp: number;
  /** Fence generation at issue time; entries written before generations existed count as 0. */
  gen: number;
  viewer: string;
  code: string;
};
type Fence = {
  gen: number;
  pin_version: number;
  rotation_complete: boolean;
  /** Tombstones: grant id (b64url) → expiry of the grant, after which the tombstone is dropped. */
  revoked: Record<string, number>;
};

export class GrantStore {
  readonly #storage: AtomicStorage;
  readonly #key: string;
  readonly #fenceKey: string;
  readonly #now: () => number;
  readonly ttlMs: number;

  constructor(opts: { storage: AtomicStorage; streamId: string; now?: () => number; ttlMs?: number }) {
    this.#storage = opts.storage;
    this.#key = `grants:${opts.streamId}`;
    this.#fenceKey = `revocations:${opts.streamId}`;
    this.#now = opts.now ?? Date.now;
    this.ttlMs = opts.ttlMs ?? DEFAULT_GRANT_TTL_MS;
  }

  #entries(j: Json | undefined, now: number): Record<string, Entry> {
    const out: Record<string, Entry> = {};
    if (typeof j !== "object" || j === null || Array.isArray(j)) return out;
    for (const [id, e] of Object.entries(j)) {
      if (typeof e !== "object" || e === null || Array.isArray(e)) continue;
      const { sid, secret, exp, gen, viewer, code } = e;
      if (typeof sid === "string" && typeof secret === "string" && typeof exp === "number" && exp > now && typeof viewer === "string" && typeof code === "string") {
        try {
          rowCodeIndex(code);
          if (fromBase64Url(viewer).length !== 16) continue;
          out[id] = { sid, secret, exp, gen: typeof gen === "number" ? gen : 0, viewer, code };
        } catch { /* Pre-release or corrupt grants fail closed. */ }
      }
    }
    return out;
  }

  /** The stored fence, or null if it is corrupt (then every grant is refused: fail closed). */
  #fence(j: Json | undefined, now: number): Fence | null {
    if (j === undefined) return { gen: 0, revoked: {}, pin_version: 0, rotation_complete: true };
    if (typeof j !== "object" || j === null || Array.isArray(j)) return null;
    const { gen, revoked } = j;
    if (typeof gen !== "number" || !Number.isSafeInteger(gen) || gen < 0) return null;
    if (typeof revoked !== "object" || revoked === null || Array.isArray(revoked)) return null;
    const out: Record<string, number> = {};
    for (const [id, exp] of Object.entries(revoked)) if (typeof exp === "number" && exp > now) out[id] = exp;
    const version = j.pin_version ?? 0;
    const complete = j.rotation_complete ?? true;
    if (typeof version !== "number" || !Number.isSafeInteger(version) || version < 0 || typeof complete !== "boolean") return null;
    return { gen, revoked: out, pin_version: version, rotation_complete: complete };
  }

  /**
   * The fence to build a write on. A corrupt one is replaced by a generation
   * above any counter value (the clock in ms), which refuses every earlier grant.
   */
  #fenceForWrite(j: Json | undefined, now: number): Fence {
    return this.#fence(j, now) ?? { gen: Math.max(now, 1), revoked: {}, pin_version: 0, rotation_complete: false };
  }

  async issue(viewerSid: Uint8Array, rowCode?: string): Promise<Grant> {
    const fence = this.#fence(await this.#storage.get(this.#fenceKey), this.#now());
    if (!fence) throw new E2EEError("storage", "revocation state is corrupt; rotate the PIN to reset it");
    const id = randomBytes(16);
    const secret = randomBytes(32);
    const viewerId = randomBytes(16);
    if (rowCode !== undefined) rowCodeIndex(rowCode);
    let code!: string;
    const expiresAt = this.#now() + this.ttlMs;
    await this.#storage.update(this.#key, (cur) => {
      const e = this.#entries(cur, this.#now());
      const used = new Set(Object.values(e).map((x) => x.code));
      code = rowCode ?? assignRowCode(used);
      if (used.has(code)) throw new E2EEError("state", "row code already in use");
      e[toBase64Url(id)] = { viewer: toBase64Url(viewerId), code, sid: toBase64Url(viewerSid), secret: toBase64Url(secret), exp: expiresAt, gen: fence.gen };
      return { ...e };
    });
    return { id, secret, viewerSid: Uint8Array.from(viewerSid), viewerId, rowCode: code, expiresAt };
  }

  /** Valid, unexpired, unrevoked grant of the current generation, bound to this viewer session, else null. */
  async lookup(id: Uint8Array, viewerSid: Uint8Array): Promise<Grant | null> {
    const now = this.#now();
    const [fenceJ, tableJ] = await Promise.all([this.#storage.get(this.#fenceKey), this.#storage.get(this.#key)]);
    const fence = this.#fence(fenceJ, now);
    const key = toBase64Url(id);
    const e = this.#entries(tableJ, now)[key];
    if (!fence || !e || e.sid !== toBase64Url(viewerSid)) return null;
    if (e.gen < fence.gen || fence.revoked[key] !== undefined) return null;
    return { id: Uint8Array.from(id), secret: fromBase64Url(e.secret), viewerSid: Uint8Array.from(viewerSid), viewerId: fromBase64Url(e.viewer), rowCode: e.code, expiresAt: e.exp };
  }

  /**
   * Write-ahead revocation of specific grants: once this resolves, they are
   * refused by every lookup, including after a restart. The storage write
   * starts synchronously, before this returns.
   */
  tombstone(grants: readonly { id: Uint8Array; expiresAt: number }[]): Promise<Json> {
    return this.#storage.update(this.#fenceKey, (cur) => {
      const now = this.#now();
      const f = this.#fenceForWrite(cur, now);
      for (const g of grants) f.revoked[toBase64Url(g.id)] = Math.max(f.revoked[toBase64Url(g.id)] ?? 0, g.expiresAt);
      return { ...f, revoked: { ...f.revoked } };
    });
  }

  /**
   * Write-ahead revocation of every grant issued so far (PIN rotation): the
   * generation moves on, so all earlier grants are refused. Older tombstones
   * are covered by the new generation and dropped. Starts synchronously.
   */
  bumpGeneration(version?: number): Promise<Json> {
    return this.#storage.update(this.#fenceKey, (cur) => {
      const f = this.#fenceForWrite(cur, this.#now());
      const next = version ?? f.pin_version + 1;
      if (!Number.isSafeInteger(next) || next < 1) throw new E2EEError("invalid_argument", "invalid PIN version");
      // Idempotent even if a timed-out write lands after its retry or completion.
      if (next <= f.pin_version) return { ...f };
      return { gen: f.gen + 1, revoked: {}, pin_version: next, rotation_complete: false };
    });
  }

  async inspect(): Promise<{ pinVersion: number; rotationComplete: boolean }> {
    const f = this.#fence(await this.#storage.get(this.#fenceKey), this.#now());
    return f ? { pinVersion: f.pin_version, rotationComplete: f.rotation_complete } : { pinVersion: -1, rotationComplete: false };
  }

  async completeRotation(version: number): Promise<void> {
    await this.#storage.update(this.#fenceKey, (cur) => {
      const f = this.#fence(cur, this.#now());
      if (!f || f.pin_version !== version) throw new E2EEError("state_mismatch", "PIN version changed during rotation");
      return { ...f, rotation_complete: true };
    });
  }

  async rowCodes(): Promise<Set<string>> {
    const entries = this.#entries(await this.#storage.get(this.#key), this.#now());
    return new Set(Object.values(entries).map((e) => e.code));
  }

  /** Housekeeping: remove a (revoked) grant from the table. */
  async revoke(id: Uint8Array): Promise<void> {
    await this.#storage.update(this.#key, (cur) => {
      const e = this.#entries(cur, this.#now());
      delete e[toBase64Url(id)];
      return { ...e };
    });
  }

  /** Housekeeping after a generation bump: empty the table. */
  async revokeAll(): Promise<void> {
    await this.#storage.update(this.#key, () => ({}));
  }

  /**
   * At startup: apply every revocation in the fence to the grant table (a
   * crash may have left it written ahead but not applied). lookup() already
   * enforces the fence, so this is cleanup and may fail without harm.
   */
  async recover(): Promise<void> {
    const fence = this.#fence(await this.#storage.get(this.#fenceKey), this.#now());
    await this.#storage.update(this.#key, (cur) => {
      if (!fence) return {};
      const e = this.#entries(cur, this.#now());
      for (const [id, entry] of Object.entries(e)) if (entry.gen < fence.gen || fence.revoked[id] !== undefined) delete e[id];
      return { ...e };
    });
  }
}
