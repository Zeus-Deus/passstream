/**
 * End-to-end encrypted annotations (laser pointer, pen strokes), PROTOCOL §20.
 *
 *   session_annot  = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 annotation session", 32)
 *   epoch_annot(e) = HKDF-SHA-256(epoch_secret_e, salt="", info="PassStream v1 annotation epoch" || u32(e), 32)
 *   key(dir, e)    = HKDF-SHA-256(ikm=session_annot, salt=epoch_annot(e),
 *                                 info="PassStream v1 annotation " || dir || u32(e))      AES-256-GCM
 *   nonce          = u32(e) || u64(ctr)        ctr from 1, strictly increasing per (session, epoch, dir)
 *   aad            = lv_cat("PassStream v1 annotation", dir, u32(e), u64(ctr))
 *
 * The pairwise session secret authenticates the row: only that viewer and the
 * sender hold it, so no viewer can speak for another viewer or the host. The
 * epoch secret ties every message to the current E2EE epoch, so a rekey moves
 * everyone to fresh keys and counters. Keys are distinct from every SFrame key
 * (other labels, other hash, never used for media), so nonces can't collide.
 *
 * Upstream (viewer → sender) carries one opaque app body with no origin field.
 * Downstream (sender → viewer) carries records stamped by the sender with the
 * origin it authenticated: a row-code index, or 0xFFFF for the host.
 */
import { hkdf } from "@noble/hashes/hkdf.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { type Bytes, concat, copy, lvCat, u16be, u32be, u64be, utf8 } from "./bytes.ts";
import type { EpochKey } from "./channel.ts";
import { E2EEError } from "./errors.ts";
import { rowCodeFromIndex, rowCodeIndex } from "./row-code.ts";

/** Largest upstream body (one app message). */
export const ANNOTATION_MAX_BODY = 512;
/** Largest downstream plaintext (a batch of stamped records). */
export const ANNOTATION_MAX_BATCH = 2048;
/** Records per downstream batch. */
export const ANNOTATION_MAX_RECORDS = 64;
/** Epochs each side keeps annotation keys and counters for. */
export const ANNOTATION_MAX_EPOCHS = 4;
/** The origin of the sender's own annotations. */
export const ANNOTATION_HOST = "host";
const HOST_INDEX = 0xffff;

export type AnnotationDirection = "v2s" | "s2v";

/** One downstream record: who drew it (as authenticated by the sender), and the app body. */
export interface AnnotationRecord {
  /** A canonical row code, or "host". */
  origin: string;
  body: Uint8Array;
}

/** The 32-byte per-epoch input, derived from the epoch secret. Never used for media. */
export function annotationEpochSecret(k: EpochKey): Bytes {
  return copy(hkdf(sha256, k.secret, new Uint8Array(0), concat(utf8("PassStream v1 annotation epoch"), u32be(k.epoch)), 32));
}

/** Import the per-session secret as a non-extractable HKDF base key. */
export function importAnnotationBase(raw: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", copy(raw), "HKDF", false, ["deriveKey"]);
}

function aad(dir: AnnotationDirection, epoch: number, ctr: bigint): Bytes {
  return lvCat(utf8("PassStream v1 annotation"), utf8(dir), u32be(epoch), u64be(ctr));
}
function nonce(epoch: number, ctr: bigint): Bytes {
  return concat(u32be(epoch), u64be(ctr));
}

/** `count:u8 || (origin:u16 || len:u16 || body)*`, origin 0xFFFF = host. */
export function encodeAnnotationRecords(records: readonly AnnotationRecord[]): Bytes {
  if (records.length < 1 || records.length > ANNOTATION_MAX_RECORDS) throw new E2EEError("invalid_argument", "annotation record count");
  const parts: Uint8Array[] = [Uint8Array.of(records.length)];
  for (const r of records) {
    if (!(r.body instanceof Uint8Array) || r.body.length < 1 || r.body.length > ANNOTATION_MAX_BODY) throw new E2EEError("invalid_argument", "annotation body size");
    parts.push(u16be(r.origin === ANNOTATION_HOST ? HOST_INDEX : rowCodeIndex(r.origin)), u16be(r.body.length), r.body);
  }
  const out = concat(...parts);
  if (out.length > ANNOTATION_MAX_BATCH) throw new E2EEError("invalid_argument", "annotation batch too large");
  return out;
}

export function decodeAnnotationRecords(b: Uint8Array): AnnotationRecord[] {
  if (b.length < 1 || b.length > ANNOTATION_MAX_BATCH) throw new E2EEError("malformed", "annotation batch size");
  const count = b[0]!;
  if (count < 1 || count > ANNOTATION_MAX_RECORDS) throw new E2EEError("malformed", "annotation record count");
  const out: AnnotationRecord[] = [];
  let off = 1;
  for (let i = 0; i < count; i++) {
    if (off + 4 > b.length) throw new E2EEError("malformed", "truncated annotation record");
    const idx = b[off]! * 256 + b[off + 1]!;
    const len = b[off + 2]! * 256 + b[off + 3]!;
    off += 4;
    if (len < 1 || len > ANNOTATION_MAX_BODY || off + len > b.length) throw new E2EEError("malformed", "annotation record length");
    out.push({ origin: idx === HOST_INDEX ? ANNOTATION_HOST : rowCodeFromIndex(idx), body: copy(b.subarray(off, off + len)) });
    off += len;
  }
  if (off !== b.length) throw new E2EEError("malformed", "trailing annotation bytes");
  return out;
}

/**
 * One side of one admitted session's annotation channel: key derivation per
 * epoch, the send counter, and the receive replay check. The caller supplies
 * the epoch input (`annotationEpochSecret`) it holds for that epoch.
 */
export class AnnotationChannel {
  #base: CryptoKey | null;
  readonly #send: AnnotationDirection;
  readonly #recv: AnnotationDirection;
  readonly #keys = new Map<string, Promise<CryptoKey>>();
  readonly #sendCtr = new Map<number, bigint>();
  readonly #recvCtr = new Map<number, bigint>();
  #recvEpoch = 0;
  #sendEpoch = 0;
  #chain: Promise<unknown> = Promise.resolve();

  constructor(base: CryptoKey, role: "sender" | "viewer") {
    this.#base = base;
    this.#send = role === "sender" ? "s2v" : "v2s";
    this.#recv = role === "sender" ? "v2s" : "s2v";
  }

  #key(dir: AnnotationDirection, epoch: number, epochSecret: Uint8Array): Promise<CryptoKey> {
    const base = this.#base;
    if (!base) return Promise.reject(new E2EEError("state", "annotation channel disposed"));
    const id = `${dir}:${epoch}`;
    let k = this.#keys.get(id);
    if (!k) {
      k = crypto.subtle.deriveKey(
        { name: "HKDF", hash: "SHA-256", salt: copy(epochSecret), info: concat(utf8(`PassStream v1 annotation ${dir}`), u32be(epoch)) },
        base,
        { name: "AES-GCM", length: 256 },
        false,
        ["encrypt", "decrypt"],
      );
      this.#keys.set(id, k);
      this.#prune();
    }
    return k;
  }

  /** Bound cached epoch keys; keep the independent monotonic direction watermarks. */
  #prune() {
    const epochs = new Set<number>([...this.#sendCtr.keys(), ...this.#recvCtr.keys(), ...[...this.#keys.keys()].map((k) => Number(k.split(":")[1]))]);
    const keep = new Set([...epochs].sort((a, b) => b - a).slice(0, ANNOTATION_MAX_EPOCHS));
    for (const k of [...this.#keys.keys()]) if (!keep.has(Number(k.split(":")[1]))) this.#keys.delete(k);
    // Counter watermarks must not be evicted by unauthenticated receive keys.
    for (const e of this.#sendCtr.keys()) if (e < this.#sendEpoch) this.#sendCtr.delete(e);
    for (const e of this.#recvCtr.keys()) if (e < this.#recvEpoch) this.#recvCtr.delete(e);
  }

  /**
   * Encrypt one plaintext under (this direction, epoch). The counter is taken
   * synchronously and seals are chained, so messages come out in counter order.
   */
  seal(epoch: number, epochSecret: Uint8Array, plaintext: Uint8Array): Promise<{ ctr: bigint; box: Bytes }> {
    if (!this.#base) return Promise.reject(new E2EEError("state", "annotation channel disposed"));
    if (epoch < this.#sendEpoch) return Promise.reject(new E2EEError("replay", "obsolete annotation epoch"));
    const ctr = (this.#sendCtr.get(epoch) ?? 0n) + 1n;
    if (ctr > 0xffff_ffff_ffff_ffffn) return Promise.reject(new E2EEError("state", "annotation counter exhausted"));
    this.#sendEpoch = epoch;
    this.#sendCtr.set(epoch, ctr);
    const run = this.#chain.then(async () => {
      const key = await this.#key(this.#send, epoch, epochSecret);
      const box = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce(epoch, ctr), additionalData: aad(this.#send, epoch, ctr) }, key, copy(plaintext)));
      if (!this.#base) throw new E2EEError("state", "annotation channel disposed");
      return { ctr, box };
    });
    this.#chain = run.catch(() => undefined);
    return run;
  }

  /**
   * Annotation output is ordered by (authenticated epoch, counter), unlike
   * media's old-key playout buffer. Check before and after every decrypt await:
   * an older in-flight authentication cannot cross a newer accepted clear.
   */
  async open(epoch: number, epochSecret: Uint8Array, ctr: bigint, box: Uint8Array): Promise<Bytes> {
    this.#checkReceive(epoch, ctr);
    const key = await this.#key(this.#recv, epoch, epochSecret);
    let pt: Bytes;
    try {
      pt = new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce(epoch, ctr), additionalData: aad(this.#recv, epoch, ctr) }, key, copy(box)));
    } catch {
      throw new E2EEError("decrypt_failed", "annotation failed authentication");
    }
    this.#checkReceive(epoch, ctr);
    this.#recvEpoch = epoch;
    this.#recvCtr.set(epoch, ctr);
    this.#prune();
    return pt;
  }

  #checkReceive(epoch: number, ctr: bigint) {
    if (!this.#base) throw new E2EEError("state", "annotation channel disposed");
    if (epoch < this.#recvEpoch || ctr <= (this.#recvCtr.get(epoch) ?? 0n)) throw new E2EEError("replay", "stale annotation epoch or counter");
  }

  dispose(): void {
    this.#base = null;
    this.#keys.clear();
  }
}
