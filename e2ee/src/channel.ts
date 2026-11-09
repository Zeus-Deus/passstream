/**
 * Keys derived from a CPace ISK after mutual key confirmation, and the
 * per-session channel that carries rekeys from sender to viewer.
 *
 *   box_key = HKDF-SHA-512(ikm=ISK, salt=sid, info="PassStream v1 admit box", 32)
 *   s2v_key = HKDF-SHA-512(ikm=ISK, salt=sid, info="PassStream v1 channel s2v", 32)
 *   v2s_key = HKDF-SHA-512(ikm=ISK, salt=sid, info="PassStream v1 channel v2s", 32)
 *
 * All three are AES-256-GCM keys. box_key encrypts exactly one message (the
 * admit bundle) so it uses an all-zero nonce. Channel messages use
 * nonce = 0x00000000 || seq:u64 with a strictly increasing seq per direction.
 */
import { hkdf } from "@noble/hashes/hkdf.js";
import { sha512 } from "@noble/hashes/sha2.js";
import { type Bytes, concat, copy, lvCat, u16be, u32be, u64be, utf8 } from "./bytes.ts";
import { E2EEError } from "./errors.ts";
import { MEDIA_SUITE } from "./sframe.ts";
import { encodeMessage, expectMessage, MsgType, type Annotation, type Rekey, type RekeyAck } from "./wire.ts";
import {
  ANNOTATION_MAX_EPOCHS,
  AnnotationChannel,
  type AnnotationRecord,
  annotationEpochSecret,
  decodeAnnotationRecords,
  encodeAnnotationRecords,
  importAnnotationBase,
} from "./annotation.ts";

export interface EpochKey {
  /** Monotonic per stream, u32. */
  epoch: number;
  /** 32 random bytes; per-track SFrame base keys are derived from it. */
  secret: Bytes;
}

export const EPOCH_SECRET_BYTES = 32;

async function aesKey(raw: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", copy(raw), "AES-GCM", false, ["encrypt", "decrypt"]);
}

function derive(isk: Uint8Array, sid: Uint8Array, label: string): Bytes {
  return copy(hkdf(sha512, isk, sid, utf8(label), 32));
}

export interface SessionKeys {
  box: CryptoKey;
  pending: CryptoKey;
  s2v: CryptoKey;
  v2s: CryptoKey;
  /** HKDF base key for annotations (PROTOCOL §20), non-extractable. */
  annot: CryptoKey;
}

export async function deriveSessionKeys(isk: Uint8Array, sid: Uint8Array): Promise<SessionKeys> {
  return {
    pending: await aesKey(derive(isk, sid, "PassStream v1 pending")),
    box: await aesKey(derive(isk, sid, "PassStream v1 admit box")),
    s2v: await aesKey(derive(isk, sid, "PassStream v1 channel s2v")),
    v2s: await aesKey(derive(isk, sid, "PassStream v1 channel v2s")),
    annot: await importAnnotationBase(derive(isk, sid, "PassStream v1 annotation session")),
  };
}

export async function seal(key: CryptoKey, nonce: Bytes, aad: Bytes, pt: Bytes): Promise<Bytes> {
  return new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: aad }, key, pt));
}

export async function open(key: CryptoKey, nonce: Bytes, aad: Bytes, ct: Uint8Array): Promise<Bytes> {
  try {
    return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce, additionalData: aad }, key, copy(ct)));
  } catch {
    throw new E2EEError("decrypt_failed", "channel message failed authentication");
  }
}

export function seqNonce(seq: bigint): Bytes {
  return concat(new Uint8Array(4), u64be(seq));
}

const REKEY_AAD = "PassStream v1 rekey";
const ACK_AAD = "PassStream v1 rekey ack";

/** epoch:u32 || suite:u16 || secret:32 */
export function encodeEpochKey(k: EpochKey): Bytes {
  return concat(u32be(k.epoch), u16be(MEDIA_SUITE), k.secret);
}

export function decodeEpochKey(b: Uint8Array): EpochKey {
  if (b.length !== 6 + EPOCH_SECRET_BYTES) throw new E2EEError("malformed", "bad epoch key");
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  if (dv.getUint16(4) !== MEDIA_SUITE) throw new E2EEError("malformed", "unsupported media suite");
  return { epoch: dv.getUint32(0), secret: copy(b.subarray(6)) };
}

/** Sender's end of an admitted session. */
export class SenderSession {
  readonly id: string;
  readonly viewerSid: Bytes;
  readonly grantId: Bytes;
  readonly #keys: SessionKeys;
  #seq = 0n;
  readonly #sent = new Map<bigint, number>(); // seq -> epoch, awaiting ack
  readonly #annot: AnnotationChannel;

  constructor(opts: { id: string; viewerSid: Uint8Array; grantId: Uint8Array; keys: SessionKeys }) {
    this.id = opts.id;
    this.viewerSid = copy(opts.viewerSid);
    this.grantId = copy(opts.grantId);
    this.#keys = opts.keys;
    this.#annot = new AnnotationChannel(opts.keys.annot, "sender");
  }

  /** Seal a downstream batch (S→V) under `epoch`, whose annotation input the key manager holds. */
  async sealAnnotations(epoch: number, epochSecret: Uint8Array, records: readonly AnnotationRecord[]): Promise<Bytes> {
    const { ctr, box } = await this.#annot.seal(epoch, epochSecret, encodeAnnotationRecords(records));
    return encodeMessage({ type: MsgType.Annotations, epoch, ctr, box });
  }

  /** Open an upstream message (V→S). `epochSecret` is undefined for an epoch the sender doesn't hold. */
  async openAnnotation(m: Annotation, epochSecret: Uint8Array | undefined): Promise<Bytes> {
    if (!epochSecret) throw new E2EEError("unknown_key", "annotation for an unknown epoch");
    return this.#annot.open(m.epoch, epochSecret, m.ctr, m.box);
  }

  /** Forget annotation keys (the session was removed). */
  disposeAnnotations(): void {
    this.#annot.dispose();
  }

  async sealRekey(k: EpochKey): Promise<Bytes> {
    const seq = ++this.#seq;
    this.#sent.set(seq, k.epoch);
    const box = await seal(this.#keys.s2v, seqNonce(seq), lvCat(utf8(REKEY_AAD), u64be(seq)), encodeEpochKey(k));
    const m: Rekey = { type: MsgType.Rekey, seq, box };
    return encodeMessage(m);
  }

  /** Verify an ack; returns the acknowledged epoch. */
  async openAck(ack: RekeyAck): Promise<number> {
    const epoch = this.#sent.get(ack.seq);
    if (epoch === undefined) throw new E2EEError("unexpected_message", "ack for unknown rekey");
    await open(this.#keys.v2s, seqNonce(ack.seq), lvCat(utf8(ACK_AAD), u64be(ack.seq), u32be(epoch)), ack.tag);
    this.#sent.delete(ack.seq);
    return epoch;
  }
}

/** Viewer's end of an admitted session. */
export class ViewerSession {
  #keys: SessionKeys | null;
  #lastSeq = 0n;
  #maxEpoch: number;
  readonly #annot: AnnotationChannel;
  /** Annotation inputs of the newest epochs this session received (PROTOCOL §20). */
  readonly #annotEpochs = new Map<number, Bytes>();

  constructor(keys: SessionKeys, initialEpoch: number) {
    this.#keys = keys;
    this.#maxEpoch = initialEpoch;
    this.#annot = new AnnotationChannel(keys.annot, "viewer");
  }

  /** Remember an epoch's annotation input. The admission calls it for the admit bundle's epoch; rekeys do it themselves. */
  addEpoch(k: EpochKey): void {
    if (!this.#keys) return;
    this.#annotEpochs.set(k.epoch, annotationEpochSecret(k));
    const keep = [...this.#annotEpochs.keys()].sort((a, b) => b - a).slice(0, ANNOTATION_MAX_EPOCHS);
    for (const [e, v] of this.#annotEpochs) if (!keep.includes(e)) (v.fill(0), this.#annotEpochs.delete(e));
  }

  /** The newest epoch with an annotation key, or null. */
  get annotationEpoch(): number | null {
    return this.#annotEpochs.size ? Math.max(...this.#annotEpochs.keys()) : null;
  }

  /** Seal one app body (≤ ANNOTATION_MAX_BODY bytes) for the sender, under the newest epoch. */
  async sealAnnotation(body: Uint8Array): Promise<Bytes> {
    const epoch = this.annotationEpoch;
    if (!this.#keys || epoch === null) throw new E2EEError("state", "session disposed or no epoch");
    const { ctr, box } = await this.#annot.seal(epoch, this.#annotEpochs.get(epoch)!, body);
    const m: Annotation = { type: MsgType.Annotation, epoch, ctr, box };
    return encodeMessage(m);
  }

  /** Open a downstream ANNOTATIONS message: the sender-stamped records. */
  async openAnnotations(bytes: Uint8Array): Promise<{ epoch: number; records: AnnotationRecord[] }> {
    const m = expectMessage(bytes, MsgType.Annotations);
    if (!this.#keys) throw new E2EEError("state", "session disposed");
    const secret = this.#annotEpochs.get(m.epoch);
    if (!secret) throw new E2EEError("unknown_key", "annotation for an unknown epoch");
    return { epoch: m.epoch, records: decodeAnnotationRecords(await this.#annot.open(m.epoch, secret, m.ctr, m.box)) };
  }

  /** Bytes-in convenience for the data channel: a REKEY message → new epoch + ack. */
  handle(bytes: Uint8Array): Promise<{ epoch: EpochKey; ack: Bytes }> {
    return this.handleRekey(expectMessage(bytes, MsgType.Rekey));
  }

  /**
   * Open a REKEY message. Returns the new epoch key and the ack to send back.
   * Rejects replays (seq must increase) and epochs that don't move forward.
   */
  async handleRekey(m: Rekey): Promise<{ epoch: EpochKey; ack: Bytes }> {
    const keys = this.#keys;
    if (!keys) throw new E2EEError("state", "session disposed");
    if (m.seq <= this.#lastSeq) throw new E2EEError("replay", "stale rekey sequence number");
    const pt = await open(keys.s2v, seqNonce(m.seq), lvCat(utf8(REKEY_AAD), u64be(m.seq)), m.box);
    const epoch = decodeEpochKey(pt);
    this.#lastSeq = m.seq;
    if (epoch.epoch <= this.#maxEpoch) throw new E2EEError("replay", "epoch does not advance");
    this.#maxEpoch = epoch.epoch;
    this.addEpoch(epoch);
    const tag = await seal(keys.v2s, seqNonce(m.seq), lvCat(utf8(ACK_AAD), u64be(m.seq), u32be(epoch.epoch)), new Uint8Array(0));
    const ack: RekeyAck = { type: MsgType.RekeyAck, seq: m.seq, tag };
    return { epoch, ack: encodeMessage(ack) };
  }

  /** Forget the channel keys (kicked, ended): every later rekey is refused. */
  dispose(): void {
    this.#keys = null;
    this.#annot.dispose();
    for (const v of this.#annotEpochs.values()) v.fill(0);
    this.#annotEpochs.clear();
  }
}
