/**
 * Binary messages carried by the ordered (untrusted) signaling relay.
 *
 *   message = version:u8 (=1) || type:u8 || field*
 *   field   = LEB128(len) || bytes
 *
 * Each type has a fixed field list with exact or bounded lengths. Parsers
 * reject unknown versions/types, wrong field counts, bad lengths and trailing
 * bytes, and throw only E2EEError("malformed").
 */
import { type Bytes, concat, copy, leb128, u32be, u64be } from "./bytes.ts";
import { E2EEError } from "./errors.ts";

export const WIRE_VERSION = 1;
export const MAX_MESSAGE_BYTES = 4096;

export const MsgType = {
  Hello: 0x01,
  Challenge: 0x02,
  Response: 0x03,
  Admit: 0x04,
  Reject: 0x05,
  Rekey: 0x06,
  RekeyAck: 0x07,
  Pending: 0x08,
  Annotation: 0x09,
  Annotations: 0x0a,
} as const;
export type MsgTypeId = (typeof MsgType)[keyof typeof MsgType];

export const AdmissionMode = { Pin: 0, Grant: 1 } as const;
export type AdmissionModeId = (typeof AdmissionMode)[keyof typeof AdmissionMode];

export const RejectReason = {
  WrongPin: 1,
  Backoff: 2,
  Frozen: 3,
  Locked: 4,
  GrantInvalid: 5,
  Expired: 6,
  Invalid: 7,
  Denied: 8,
} as const;
export type RejectReasonId = (typeof RejectReason)[keyof typeof RejectReason];
const REJECT_NAMES: Record<number, string> = {
  1: "wrong_pin",
  2: "backoff",
  3: "frozen",
  4: "locked",
  5: "grant_invalid",
  6: "expired",
  7: "invalid",
  8: "denied",
};
export function rejectReasonName(r: number): string {
  return REJECT_NAMES[r] ?? "unknown";
}

export interface Hello {
  type: typeof MsgType.Hello;
  streamId: Bytes; // 1..128
  viewerSid: Bytes; // 16
  nonce: Bytes; // nV, 32
  mode: AdmissionModeId;
  grantId: Bytes; // 0 (pin) or 16 (grant)
}
export interface Challenge {
  type: typeof MsgType.Challenge;
  nonce: Bytes; // nS, 32
  ada: Bytes; // 8: version u8 || epoch u32 || suite u16 || flags u8
  ya: Bytes; // 32
  sig: Bytes; // 64
}
export interface Response {
  type: typeof MsgType.Response;
  yb: Bytes; // 32
  adb: Bytes; // 1: version
  tb: Bytes; // 64
}
export interface Admit {
  type: typeof MsgType.Admit;
  ta: Bytes; // 64
  box: Bytes; // AEAD(admit bundle)
}
export interface Reject {
  type: typeof MsgType.Reject;
  reason: RejectReasonId;
  retryAfterMs: number; // u32
  triesLeft: number; // u8
  sig: Bytes; // 64
}
export interface Rekey {
  type: typeof MsgType.Rekey;
  seq: bigint; // u64
  box: Bytes;
}
export interface RekeyAck {
  type: typeof MsgType.RekeyAck;
  seq: bigint;
  tag: Bytes; // 16
}
export interface Pending {
  type: typeof MsgType.Pending;
  box: Bytes; // 22: code index u16, timeout u32, GCM tag
}
/** V→S: one encrypted annotation body (PROTOCOL §20). */
export interface Annotation {
  type: typeof MsgType.Annotation;
  epoch: number; // u32
  ctr: bigint; // u64
  box: Bytes; // AEAD(body ≤ 512)
}
/** S→V: a batch of origin-stamped annotation records, sealed for one viewer (PROTOCOL §20). */
export interface Annotations {
  type: typeof MsgType.Annotations;
  epoch: number;
  ctr: bigint;
  box: Bytes; // AEAD(records ≤ 2048)
}
export type Message = Hello | Challenge | Response | Admit | Reject | Rekey | RekeyAck | Pending | Annotation | Annotations;

type Len = number | [min: number, max: number];
const SCHEMA: Record<MsgTypeId, Len[]> = {
  [MsgType.Hello]: [[1, 128], 16, 32, 1, [0, 16]],
  [MsgType.Challenge]: [32, 8, 32, 64],
  [MsgType.Response]: [32, 1, 64],
  [MsgType.Admit]: [64, 113],
  [MsgType.Reject]: [1, 4, 1, 64],
  [MsgType.Rekey]: [8, [16, 512]],
  [MsgType.RekeyAck]: [8, 16],
  [MsgType.Pending]: [22],
  // Bodies 1–512 bytes + 16-byte tag; batches 6–2048 bytes + tag.
  [MsgType.Annotation]: [4, 8, [17, 528]],
  [MsgType.Annotations]: [4, 8, [22, 2064]],
};

function fieldOk(len: number, spec: Len): boolean {
  return typeof spec === "number" ? len === spec : len >= spec[0] && len <= spec[1];
}

function encodeFields(type: MsgTypeId, fields: Uint8Array[]): Bytes {
  const spec = SCHEMA[type];
  if (fields.length !== spec.length) throw new E2EEError("invalid_argument", "field count");
  const parts: Uint8Array[] = [Uint8Array.of(WIRE_VERSION, type)];
  fields.forEach((f, i) => {
    if (!fieldOk(f.length, spec[i]!)) throw new E2EEError("invalid_argument", `field ${i} length ${f.length}`);
    parts.push(leb128(f.length), f);
  });
  const out = concat(...parts);
  if (out.length > MAX_MESSAGE_BYTES) throw new E2EEError("invalid_argument", "message too large");
  return out;
}

function readLeb(buf: Uint8Array, off: number): [value: number, next: number] {
  let v = 0;
  let shift = 0;
  for (let i = 0; i < 3; i++) {
    const b = buf[off + i];
    if (b === undefined) throw new E2EEError("malformed", "truncated length");
    v += (b & 0x7f) * 2 ** shift;
    if ((b & 0x80) === 0) {
      // Minimal encoding only: no trailing zero continuation bytes.
      if (i > 0 && b === 0) throw new E2EEError("malformed", "non-minimal length");
      return [v, off + i + 1];
    }
    shift += 7;
  }
  throw new E2EEError("malformed", "length too large");
}

/** The type byte, for routing, after checking the version. */
export function peekType(buf: Uint8Array): MsgTypeId {
  if (!(buf instanceof Uint8Array) || buf.length < 2) throw new E2EEError("malformed", "short message");
  if (buf[0] !== WIRE_VERSION) throw new E2EEError("malformed", `unsupported version ${buf[0]}`);
  const t = buf[1]!;
  if (!(t in SCHEMA)) throw new E2EEError("malformed", `unknown type ${t}`);
  return t as MsgTypeId;
}

function decodeFields(buf: Uint8Array): [MsgTypeId, Bytes[]] {
  if (buf.length > MAX_MESSAGE_BYTES) throw new E2EEError("malformed", "message too large");
  const type = peekType(buf);
  const spec = SCHEMA[type];
  const fields: Bytes[] = [];
  let off = 2;
  for (const s of spec) {
    const [len, next] = readLeb(buf, off);
    if (!fieldOk(len, s)) throw new E2EEError("malformed", `bad field length ${len}`);
    if (next + len > buf.length) throw new E2EEError("malformed", "truncated field");
    fields.push(copy(buf.subarray(next, next + len)));
    off = next + len;
  }
  if (off !== buf.length) throw new E2EEError("malformed", "trailing bytes");
  return [type, fields];
}

function u32(b: Uint8Array): number {
  return new DataView(b.buffer, b.byteOffset, b.byteLength).getUint32(0);
}
function u64(b: Uint8Array): bigint {
  return new DataView(b.buffer, b.byteOffset, b.byteLength).getBigUint64(0);
}

export function encodeMessage(m: Message): Bytes {
  switch (m.type) {
    case MsgType.Hello:
      return encodeFields(m.type, [m.streamId, m.viewerSid, m.nonce, Uint8Array.of(m.mode), m.grantId]);
    case MsgType.Challenge:
      return encodeFields(m.type, [m.nonce, m.ada, m.ya, m.sig]);
    case MsgType.Response:
      return encodeFields(m.type, [m.yb, m.adb, m.tb]);
    case MsgType.Pending:
      return encodeFields(m.type, [m.box]);
    case MsgType.Admit:
      return encodeFields(m.type, [m.ta, m.box]);
    case MsgType.Reject:
      return encodeFields(m.type, [Uint8Array.of(m.reason), u32be(m.retryAfterMs), Uint8Array.of(m.triesLeft), m.sig]);
    case MsgType.Rekey:
      return encodeFields(m.type, [u64be(m.seq), m.box]);
    case MsgType.RekeyAck:
      return encodeFields(m.type, [u64be(m.seq), m.tag]);
    case MsgType.Annotation:
    case MsgType.Annotations:
      return encodeFields(m.type, [u32be(m.epoch), u64be(m.ctr), m.box]);
  }
}

export function decodeMessage(buf: Uint8Array): Message {
  const [type, f] = decodeFields(buf);
  const at = (i: number): Bytes => f[i]!;
  switch (type) {
    case MsgType.Hello: {
      const mode = at(3)[0]!;
      if (mode !== AdmissionMode.Pin && mode !== AdmissionMode.Grant) throw new E2EEError("malformed", "bad mode");
      const want = mode === AdmissionMode.Grant ? 16 : 0;
      if (at(4).length !== want) throw new E2EEError("malformed", "grant id length does not match mode");
      return { type, streamId: at(0), viewerSid: at(1), nonce: at(2), mode, grantId: at(4) };
    }
    case MsgType.Challenge:
      return { type, nonce: at(0), ada: at(1), ya: at(2), sig: at(3) };
    case MsgType.Response:
      return { type, yb: at(0), adb: at(1), tb: at(2) };
    case MsgType.Pending:
      return { type, box: at(0) };
    case MsgType.Admit:
      return { type, ta: at(0), box: at(1) };
    case MsgType.Reject: {
      const reason = at(0)[0]!;
      if (!(reason in REJECT_NAMES)) throw new E2EEError("malformed", "bad reject reason");
      return { type, reason: reason as RejectReasonId, retryAfterMs: u32(at(1)), triesLeft: at(2)[0]!, sig: at(3) };
    }
    case MsgType.Rekey:
      return { type, seq: u64(at(0)), box: at(1) };
    case MsgType.RekeyAck:
      return { type, seq: u64(at(0)), tag: at(1) };
    case MsgType.Annotation:
    case MsgType.Annotations:
      return { type, epoch: u32(at(0)), ctr: u64(at(1)), box: at(2) };
  }
}

/** Decode and require a specific type. */
export function expectMessage<T extends Message["type"]>(buf: Uint8Array, type: T): Extract<Message, { type: T }> {
  const m = decodeMessage(buf);
  if (m.type !== type) throw new E2EEError("unexpected_message", `expected type ${type}, got ${m.type}`);
  return m as Extract<Message, { type: T }>;
}
