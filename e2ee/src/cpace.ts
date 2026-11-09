/**
 * CPace, cipher suite CPACE-RISTR255-SHA512, from draft-irtf-cfrg-cpace-21
 * (https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/21/), sections 6–8.3
 * and Appendix A. Only the group primitives and the ISK computation live here;
 * the message flow, party roles and key confirmation are in admission.ts.
 *
 * Group operations come from @noble/curves (ristretto255, RFC 9496) and hashing
 * from @noble/hashes (SHA-512). Nothing in this file is PassStream-specific, so
 * it is checked directly against the draft's Appendix B.3 test vectors.
 */
import { ristretto255, ristretto255_hasher } from "@noble/curves/ed25519.js";
import { sha512 } from "@noble/hashes/sha2.js";
import { type Bytes, concat, copy, lvCat, prependLen, randomBytes, utf8 } from "./bytes.ts";
import { E2EEError } from "./errors.ts";

const Point = ristretto255.Point;
type RPoint = InstanceType<typeof Point>;

/** G_Ristretto255.DSI (§8.3). */
export const DSI = utf8("CPaceRistretto255");
/** H.s_in_bytes for SHA-512 (§6.2). */
export const SHA512_BLOCK = 128;
/** Encoded neutral element G.I. */
export const IDENTITY: Bytes = new Uint8Array(32);
const GROUP_ORDER = Point.Fn.ORDER;

/** `generator_string` (§8.1, Appendix A.2). */
export function generatorString(dsi: Uint8Array, prs: Uint8Array, ci: Uint8Array, sid: Uint8Array, sInBytes: number): Bytes {
  const zpad = Math.max(0, sInBytes - 1 - prependLen(prs).length - prependLen(dsi).length);
  return lvCat(dsi, prs, new Uint8Array(zpad), ci, sid);
}

/** `G.calculate_generator` (§8.3): returns the internal (decoded) generator. */
export function calculateGenerator(prs: Uint8Array, ci: Uint8Array, sid: Uint8Array): RPoint {
  const genStr = generatorString(DSI, prs, ci, sid, SHA512_BLOCK);
  const hash = sha512(genStr); // 64 bytes = 2 * field_size_bytes
  return ristretto255_hasher.deriveToCurve!(hash);
}

function scalarFromLE(b: Uint8Array): bigint {
  let n = 0n;
  for (let i = b.length - 1; i >= 0; i--) n = (n << 8n) | BigInt(b[i] ?? 0);
  return n;
}

/**
 * `G.sample_scalar` (§8.3, recommended form): 32 random bytes with the bits
 * above group_size_bits (252) cleared, read little-endian. Zero is rejected
 * (probability 2^-252) because it would produce the neutral element.
 */
export function sampleScalar(): Bytes {
  for (;;) {
    const b = randomBytes(32);
    b[31] = (b[31] ?? 0) & 0x0f;
    const n = scalarFromLE(b);
    if (n !== 0n && n < GROUP_ORDER) return b;
  }
}

function scalarToBigint(y: Uint8Array): bigint {
  if (y.length !== 32) throw new E2EEError("invalid_argument", "scalar must be 32 bytes");
  const n = scalarFromLE(y) % GROUP_ORDER;
  if (n === 0n) throw new E2EEError("invalid_argument", "zero scalar");
  return n;
}

/** `G.scalar_mult(y, _g)`: returns the public encoding of y*g. */
export function scalarMult(y: Uint8Array, g: RPoint): Bytes {
  return copy(g.multiply(scalarToBigint(y)).toBytes());
}

/**
 * `G.scalar_mult_vfy(y, X)`: decode X, returning G.I if decoding fails or if
 * the product is the neutral element. Callers MUST abort on G.I (§7.2).
 */
export function scalarMultVfy(y: Uint8Array, x: Uint8Array): Bytes {
  let p: RPoint;
  try {
    if (x.length !== 32) return copy(IDENTITY);
    p = Point.fromBytes(x);
  } catch {
    return copy(IDENTITY);
  }
  if (p.is0()) return copy(IDENTITY);
  const k = p.multiply(scalarToBigint(y));
  if (k.is0()) return copy(IDENTITY);
  return copy(k.toBytes());
}

export function isIdentity(k: Uint8Array): boolean {
  let d = 0;
  for (const x of k) d |= x;
  return d === 0;
}

/** `transcript_ir` (§6.3, Appendix A.3.4). */
export function transcriptIr(ya: Uint8Array, ada: Uint8Array, yb: Uint8Array, adb: Uint8Array): Bytes {
  return concat(lvCat(ya, ada), lvCat(yb, adb));
}

function lexLarger(a: Uint8Array, b: Uint8Array): boolean {
  const m = Math.min(a.length, b.length);
  for (let i = 0; i < m; i++) {
    const x = a[i] ?? 0;
    const y = b[i] ?? 0;
    if (x > y) return true;
    if (x < y) return false;
  }
  return a.length > b.length;
}

/** `o_cat` (Appendix A.3.2). Only used to check the symmetric-mode vectors. */
export function oCat(a: Uint8Array, b: Uint8Array): Bytes {
  return lexLarger(a, b) ? concat(utf8("oc"), a, b) : concat(utf8("oc"), b, a);
}

/** `transcript_oc` (Appendix A.3.6). PassStream uses initiator-responder mode. */
export function transcriptOc(ya: Uint8Array, ada: Uint8Array, yb: Uint8Array, adb: Uint8Array): Bytes {
  return oCat(lvCat(ya, ada), lvCat(yb, adb));
}

/** ISK = H.hash(lv_cat(G.DSI || "_ISK", sid, K) || transcript) (§7.2). */
export function deriveIsk(sid: Uint8Array, k: Uint8Array, transcript: Uint8Array): Bytes {
  return copy(sha512(concat(lvCat(concat(DSI, utf8("_ISK")), sid, k), transcript)));
}

/** mac_key = H.hash("CPaceMac" || sid || ISK) (§10.4). */
export function deriveMacKey(sid: Uint8Array, isk: Uint8Array): Bytes {
  return copy(sha512(concat(utf8("CPaceMac"), sid, isk)));
}

/** sid_output (§10.6); exposed only for the test vectors. */
export function sidOutput(transcript: Uint8Array): Bytes {
  return copy(sha512(concat(utf8("CPaceSidOutput"), transcript)));
}
