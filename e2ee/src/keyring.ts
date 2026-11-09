/**
 * Per-track SFrame keys derived from epoch secrets.
 *
 *   KID      = epoch * 16 + track          (track 0..15, epoch u32)
 *   base_key = HKDF-SHA-256(ikm=epoch_secret, salt="",
 *                           info="PassStream v1 SFrame base_key" || KID:u64 || suite:u16, 32)
 *
 * so every (epoch, track) has its own base_key and its own CTR starting at 0.
 * Only the publisher encrypts; a base_key is never shared by two senders.
 */
import { hkdf } from "@noble/hashes/hkdf.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { type Bytes, concat, copy, u16be, u64be, utf8 } from "./bytes.ts";
import type { EpochKey } from "./channel.ts";
import { E2EEError } from "./errors.ts";
import { MEDIA_SUITE, ReplayWindow, SFrameKey } from "./sframe.ts";

export const TRACKS_PER_EPOCH = 16;

export function kidFor(epoch: number, track: number): bigint {
  if (!Number.isInteger(track) || track < 0 || track >= TRACKS_PER_EPOCH) throw new E2EEError("invalid_argument", "track must be 0..15");
  if (!Number.isInteger(epoch) || epoch < 0 || epoch > 0xffff_ffff) throw new E2EEError("invalid_argument", "bad epoch");
  return BigInt(epoch) * BigInt(TRACKS_PER_EPOCH) + BigInt(track);
}

export function splitKid(kid: bigint): { epoch: number; track: number } {
  const epoch = kid / BigInt(TRACKS_PER_EPOCH);
  if (epoch > 0xffff_ffffn) throw new E2EEError("unknown_key", "KID outside PassStream range");
  return { epoch: Number(epoch), track: Number(kid % BigInt(TRACKS_PER_EPOCH)) };
}

export function deriveBaseKey(epochSecret: Uint8Array, kid: bigint): Bytes {
  return copy(hkdf(sha256, epochSecret, new Uint8Array(0), concat(utf8("PassStream v1 SFrame base_key"), u64be(kid), u16be(MEDIA_SUITE)), 32));
}

/** Sender side: one active epoch, lazily one encrypt-only SFrameKey per track. */
export class SenderKeyRing {
  #active: EpochKey | null = null;
  /** Last epoch installed, kept across suspend(): epochs only move forward. */
  #last: number | null = null;
  #keys = new Map<number, Promise<SFrameKey>>();
  /** Resolved keys of the active epoch: the per-frame lookup needs no await. */
  #ready = new Map<number, SFrameKey>();
  #version = 0;
  readonly #claimed = new Set<number>();
  readonly #maxCtr: bigint | undefined;

  constructor(opts: { maxCtr?: bigint } = {}) {
    this.#maxCtr = opts.maxCtr;
  }

  get epoch(): number | null {
    return this.#active?.epoch ?? null;
  }

  /**
   * Changes on every setEpoch() and suspend(). An encryption that started
   * under one version must not emit its frame under another: the key it used
   * may be one a kicked viewer holds.
   */
  get version(): number {
    return this.#version;
  }

  /**
   * Switch to a new epoch. Epochs only move forward: going back would
   * restart counters on a KID that was already used.
   */
  setEpoch(k: EpochKey): void {
    if (this.#last !== null && k.epoch <= this.#last) throw new E2EEError("state", "epoch must increase");
    this.#active = { epoch: k.epoch, secret: copy(k.secret) };
    this.#last = k.epoch;
    this.#version++;
    this.#keys = new Map(); // old encryption keys are dropped immediately
    this.#ready = new Map();
  }

  /**
   * Stop encrypting (fail closed): the active key is dropped and every frame
   * is refused until a newer epoch is set. A revocation could not get a
   * fresh epoch active in time (SenderKeyManager's `onSuspend`).
   */
  suspend(): void {
    this.#active = null;
    this.#version++;
    this.#keys = new Map();
    this.#ready = new Map();
  }

  /**
   * Each track may be encrypted by exactly one transform in this realm;
   * two would share a KID and a counter space.
   */
  claimTrack(track: number): () => void {
    kidFor(0, track);
    if (this.#claimed.has(track)) throw new E2EEError("state", `track ${track} already has an encryptor`);
    this.#claimed.add(track);
    return () => this.#claimed.delete(track);
  }

  /** The active epoch's key for `track` if already derived (the hot path), else null. */
  readyKey(track: number): SFrameKey | null {
    return this.#active ? (this.#ready.get(track) ?? null) : null;
  }

  key(track: number): Promise<SFrameKey> {
    const a = this.#active;
    if (!a) return Promise.reject(new E2EEError("unknown_key", "no active epoch"));
    let p = this.#keys.get(track);
    if (!p) {
      const kid = kidFor(a.epoch, track);
      const ready = this.#ready;
      p = SFrameKey.create({ kid, baseKey: deriveBaseKey(a.secret, kid), direction: "encrypt", suite: MEDIA_SUITE, ...(this.#maxCtr !== undefined ? { maxCtr: this.#maxCtr } : {}) }).then((k) => {
        ready.set(track, k); // a replaced map (new epoch, suspend) never sees it
        return k;
      });
      this.#keys.set(track, p);
    }
    return p;
  }
}

interface EpochEntry {
  secret: Bytes;
  addedAt: number;
  retireAt: number; // Infinity while current
  keys: Map<number, Promise<{ key: SFrameKey; window: ReplayWindow }>>;
  /** Resolved keys (the per-frame lookup needs no await). */
  ready: Map<number, { key: SFrameKey; window: ReplayWindow }>;
}

export interface ReceiverKeyRingOptions {
  now?: () => number;
  /** Keep an old epoch this long after the first frame of a newer one decrypts. */
  graceMs?: number;
  /** Drop an old epoch this long after a newer one arrives, even if no frame came. */
  fallbackMs?: number;
  maxEpochs?: number;
  replayWindow?: number;
}

/** Receiver side: a few epochs, per-KID decrypt keys and replay windows. */
export class ReceiverKeyRing {
  readonly #epochs = new Map<number, EpochEntry>();
  readonly #now: () => number;
  readonly graceMs: number;
  readonly fallbackMs: number;
  readonly #maxEpochs: number;
  readonly #window: number;

  constructor(o: ReceiverKeyRingOptions = {}) {
    this.#now = o.now ?? Date.now;
    this.graceMs = o.graceMs ?? 5_000;
    this.fallbackMs = o.fallbackMs ?? 30_000;
    this.#maxEpochs = o.maxEpochs ?? 3;
    this.#window = o.replayWindow ?? 1024;
  }

  epochs(): number[] {
    return [...this.#epochs.keys()].sort((a, b) => a - b);
  }

  newest(): number | null {
    const e = this.epochs();
    return e.length ? e[e.length - 1]! : null;
  }

  addEpoch(k: EpochKey): void {
    if (this.#epochs.has(k.epoch)) return;
    const now = this.#now();
    const newest = this.newest();
    // A late, older epoch is accepted for in-flight frames but retires on the fallback deadline.
    const retireAt = newest !== null && k.epoch < newest ? now + this.fallbackMs : Infinity;
    this.#epochs.set(k.epoch, { secret: copy(k.secret), addedAt: now, retireAt, keys: new Map(), ready: new Map() });
    if (newest !== null && k.epoch > newest) {
      for (const [e, entry] of this.#epochs) if (e < k.epoch) entry.retireAt = Math.min(entry.retireAt, now + this.fallbackMs);
    }
    while (this.#epochs.size > this.#maxEpochs) this.#epochs.delete(this.epochs()[0]!);
    this.prune();
  }

  dropEpoch(epoch: number): void {
    this.#epochs.delete(epoch);
  }

  /** Called after a frame of `epoch` authenticated: start the grace period for older epochs. */
  noteDecrypted(epoch: number): void {
    const now = this.#now();
    for (const [e, entry] of this.#epochs) if (e < epoch) entry.retireAt = Math.min(entry.retireAt, now + this.graceMs);
  }

  prune(): void {
    const now = this.#now();
    for (const [e, entry] of this.#epochs) if (entry.retireAt <= now) this.#epochs.delete(e);
  }

  /** The key for `kid` if already derived and its epoch still held (the hot path), else null. */
  readyKey(kid: bigint): { key: SFrameKey; window: ReplayWindow; epoch: number; track: number } | null {
    this.prune();
    const { epoch, track } = splitKid(kid);
    const k = this.#epochs.get(epoch)?.ready.get(track);
    return k ? { key: k.key, window: k.window, epoch, track } : null;
  }

  async key(kid: bigint): Promise<{ key: SFrameKey; window: ReplayWindow; epoch: number; track: number }> {
    const hot = this.readyKey(kid);
    if (hot) return hot;
    const { epoch, track } = splitKid(kid);
    const entry = this.#epochs.get(epoch);
    if (!entry) throw new E2EEError("unknown_key", `no key for epoch ${epoch}`, { epoch });
    let p = entry.keys.get(track);
    if (!p) {
      p = SFrameKey.create({ kid, baseKey: deriveBaseKey(entry.secret, kid), direction: "decrypt", suite: MEDIA_SUITE }).then((key) => {
        const k = { key, window: new ReplayWindow(this.#window) };
        entry.ready.set(track, k);
        return k;
      });
      entry.keys.set(track, p);
    }
    return { ...(await p), epoch, track };
  }
}
