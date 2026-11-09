/**
 * SFrame, RFC 9605 (https://www.rfc-editor.org/rfc/rfc9605).
 *
 * - §4.3 header encoding (KID, CTR as compact big-endian integers)
 * - §4.4.2 key schedule (HKDF-Extract/Expand with the "SFrame 1.0" labels)
 * - §4.4.3/§4.4.4 encrypt/decrypt with AAD = header || metadata
 * - §4.5 cipher suites 0x0001–0x0005; PassStream media uses 0x0004
 *   (AES_128_GCM_SHA256_128). The AES-CTR+HMAC suites are implemented so the
 *   Appendix C vectors can be checked in full.
 *
 * HKDF runs in @noble/hashes (once per key, synchronous); the AEADs run in
 * WebCrypto so per-frame work stays in native code.
 */
import { expand as hkdfExpandRaw, extract as hkdfExtractRaw } from "@noble/hashes/hkdf.js";
import { sha256, sha512 } from "@noble/hashes/sha2.js";
import { type Bytes, concat, copy, u16be, u64be, utf8 } from "./bytes.ts";
import { E2EEError } from "./errors.ts";

export const CipherSuite = {
  AES_128_CTR_HMAC_SHA256_80: 0x0001,
  AES_128_CTR_HMAC_SHA256_64: 0x0002,
  AES_128_CTR_HMAC_SHA256_32: 0x0003,
  AES_128_GCM_SHA256_128: 0x0004,
  AES_256_GCM_SHA512_128: 0x0005,
} as const;
export type CipherSuiteId = (typeof CipherSuite)[keyof typeof CipherSuite];

/** The suite PassStream uses for media. */
export const MEDIA_SUITE: CipherSuiteId = CipherSuite.AES_128_GCM_SHA256_128;

interface SuiteParams {
  hash: typeof sha256 | typeof sha512;
  nh: number;
  nka: number; // 0 for GCM
  nk: number;
  nn: number;
  nt: number;
  gcm: boolean;
}

const SUITES: Record<CipherSuiteId, SuiteParams> = {
  1: { hash: sha256, nh: 32, nka: 16, nk: 48, nn: 12, nt: 10, gcm: false },
  2: { hash: sha256, nh: 32, nka: 16, nk: 48, nn: 12, nt: 8, gcm: false },
  3: { hash: sha256, nh: 32, nka: 16, nk: 48, nn: 12, nt: 4, gcm: false },
  4: { hash: sha256, nh: 32, nka: 0, nk: 16, nn: 12, nt: 16, gcm: true },
  5: { hash: sha512, nh: 64, nka: 0, nk: 32, nn: 12, nt: 16, gcm: true },
};

export function suiteParams(suite: CipherSuiteId): Readonly<SuiteParams> {
  const p = SUITES[suite];
  if (!p) throw new E2EEError("invalid_argument", `unknown cipher suite ${suite}`);
  return p;
}

export const MAX_U64 = (1n << 64n) - 1n;

// ---------------------------------------------------------------- header §4.3

export interface SFrameHeader {
  kid: bigint;
  ctr: bigint;
  /** Encoded header bytes (a view into the parsed buffer when decoded). */
  bytes: Uint8Array;
}

function minBytes(v: bigint): number {
  let n = 1;
  while (v >> BigInt(8 * n) !== 0n) n++;
  return n;
}

function writeBE(out: Uint8Array, off: number, v: bigint, len: number): void {
  for (let i = len - 1; i >= 0; i--) {
    out[off + i] = Number(v & 0xffn);
    v >>= 8n;
  }
}

export function encodeHeader(kid: bigint, ctr: bigint): Bytes {
  if (kid < 0n || kid > MAX_U64 || ctr < 0n || ctr > MAX_U64) {
    throw new E2EEError("invalid_argument", "KID/CTR out of range");
  }
  const kidLen = kid < 8n ? 0 : minBytes(kid);
  const ctrLen = ctr < 8n ? 0 : minBytes(ctr);
  const out = new Uint8Array(1 + kidLen + ctrLen);
  let cfg = 0;
  cfg |= kidLen === 0 ? Number(kid) << 4 : 0x80 | ((kidLen - 1) << 4);
  cfg |= ctrLen === 0 ? Number(ctr) : 0x08 | (ctrLen - 1);
  out[0] = cfg;
  if (kidLen) writeBE(out, 1, kid, kidLen);
  if (ctrLen) writeBE(out, 1 + kidLen, ctr, ctrLen);
  return out;
}

function readBE(buf: Uint8Array, off: number, len: number): bigint {
  let v = 0n;
  for (let i = 0; i < len; i++) v = (v << 8n) | BigInt(buf[off + i] ?? 0);
  return v;
}

/**
 * Parse the header at the start of `buf`. Values must use the minimal
 * encoding the RFC mandates ("MUST be encoded with the minimum number of
 * bytes"), so each (KID, CTR) has exactly one header and therefore one AAD.
 */
export function decodeHeader(buf: Uint8Array): SFrameHeader {
  if (buf.length < 1) throw new E2EEError("malformed", "empty SFrame");
  const cfg = buf[0]!;
  const x = (cfg & 0x80) !== 0;
  const k = (cfg >> 4) & 0x07;
  const y = (cfg & 0x08) !== 0;
  const c = cfg & 0x07;
  const kidLen = x ? k + 1 : 0;
  const ctrLen = y ? c + 1 : 0;
  const total = 1 + kidLen + ctrLen;
  if (buf.length < total) throw new E2EEError("malformed", "truncated SFrame header");
  const kid = x ? readBE(buf, 1, kidLen) : BigInt(k);
  const ctr = y ? readBE(buf, 1 + kidLen, ctrLen) : BigInt(c);
  if ((x && (kid < 8n || minBytes(kid) !== kidLen)) || (y && (ctr < 8n || minBytes(ctr) !== ctrLen))) {
    throw new E2EEError("malformed", "non-minimal SFrame header");
  }
  return { kid, ctr, bytes: buf.subarray(0, total) };
}

// -------------------------------------------------------- key schedule §4.4.2

export interface DerivedKeySalt {
  key: Bytes;
  salt: Bytes;
  secret: Bytes;
  keyLabel: Bytes;
  saltLabel: Bytes;
}

export function deriveKeySalt(suite: CipherSuiteId, kid: bigint, baseKey: Uint8Array): DerivedKeySalt {
  const p = suiteParams(suite);
  // HKDF-Extract("", base_key) = HMAC(salt = "", base_key).
  const secret = copy(hkdfExtract(p.hash, baseKey));
  const keyLabel = concat(utf8("SFrame 1.0 Secret key "), u64be(kid), u16be(suite));
  const saltLabel = concat(utf8("SFrame 1.0 Secret salt "), u64be(kid), u16be(suite));
  const key = copy(hkdfExpand(p.hash, secret, keyLabel, p.nk));
  const salt = copy(hkdfExpand(p.hash, secret, saltLabel, p.nn));
  return { key, salt, secret, keyLabel, saltLabel };
}

function hkdfExtract(hash: SuiteParams["hash"], ikm: Uint8Array): Uint8Array {
  return hkdfExtractRaw(hash, ikm, new Uint8Array(0));
}
function hkdfExpand(hash: SuiteParams["hash"], prk: Uint8Array, info: Uint8Array, len: number): Uint8Array {
  return hkdfExpandRaw(hash, prk, info, len);
}

// ---------------------------------------------------------------- AEAD §4.5

/** An AEAD bound to one sframe_key. Prepared once per KID. */
export interface Aead {
  readonly nt: number;
  encrypt(nonce: Bytes, aad: Uint8Array, pt: Uint8Array): Promise<Bytes>;
  decrypt(nonce: Bytes, aad: Uint8Array, ct: Uint8Array): Promise<Bytes>;
}

async function gcmAead(key: Bytes): Promise<Aead> {
  const k = await crypto.subtle.importKey("raw", key, "AES-GCM", false, ["encrypt", "decrypt"]);
  return {
    nt: 16,
    async encrypt(nonce, aad, pt) {
      return new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: aad as Bytes, tagLength: 128 }, k, pt as Bytes));
    },
    async decrypt(nonce, aad, ct) {
      try {
        return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce, additionalData: aad as Bytes, tagLength: 128 }, k, ct as Bytes));
      } catch {
        throw new E2EEError("decrypt_failed", "AEAD authentication failed");
      }
    },
  };
}

/** AES-CTR + HMAC compound AEAD (§4.5.1). */
async function ctrHmacAead(p: SuiteParams, key: Bytes): Promise<Aead> {
  const encKey = await crypto.subtle.importKey("raw", key.slice(0, p.nka), "AES-CTR", false, ["encrypt", "decrypt"]);
  const authKey = await crypto.subtle.importKey("raw", key.slice(p.nka), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const tag = async (nonce: Bytes, aad: Uint8Array, ct: Uint8Array): Promise<Bytes> => {
    const authData = concat(u64be(aad.length), u64be(ct.length), u64be(p.nt), nonce, aad, ct);
    const full = new Uint8Array(await crypto.subtle.sign("HMAC", authKey, authData));
    return full.slice(0, p.nt);
  };
  const ctr = (nonce: Bytes) => ({ name: "AES-CTR", counter: concat(nonce, new Uint8Array(4)), length: 32 });
  return {
    nt: p.nt,
    async encrypt(nonce, aad, pt) {
      const ct = new Uint8Array(await crypto.subtle.encrypt(ctr(nonce), encKey, pt as Bytes));
      return concat(ct, await tag(nonce, aad, ct));
    },
    async decrypt(nonce, aad, ct) {
      if (ct.length < p.nt) throw new E2EEError("decrypt_failed", "ciphertext shorter than tag");
      const inner = ct.subarray(0, ct.length - p.nt);
      const t = ct.subarray(ct.length - p.nt);
      const want = await tag(nonce, aad, inner);
      let d = 0;
      for (let i = 0; i < p.nt; i++) d |= (want[i] ?? 0) ^ (t[i] ?? 0);
      if (d !== 0) throw new E2EEError("decrypt_failed", "AEAD authentication failed");
      return new Uint8Array(await crypto.subtle.decrypt(ctr(nonce), encKey, inner as Bytes));
    },
  };
}

export async function makeAead(suite: CipherSuiteId, key: Bytes): Promise<Aead> {
  const p = suiteParams(suite);
  if (key.length !== p.nk) throw new E2EEError("invalid_argument", "bad AEAD key length");
  return p.gcm ? gcmAead(key) : ctrHmacAead(p, key);
}

// -------------------------------------------------------------- context §4.4

/** nonce = sframe_salt XOR encode_big_endian(CTR, Nn). */
export function makeNonce(salt: Uint8Array, ctr: bigint): Bytes {
  const nonce = copy(salt);
  let v = ctr;
  for (let i = nonce.length - 1; i >= 0 && v > 0n; i--) {
    nonce[i] = (nonce[i] ?? 0) ^ Number(v & 0xffn);
    v >>= 8n;
  }
  return nonce;
}

/**
 * One SFrame key (one KID). Implementations MUST mark each base_key as usable
 * for encryption or decryption, never both (§4.4.1), so the direction is fixed
 * at construction.
 */
export class SFrameKey {
  readonly kid: bigint;
  readonly suite: CipherSuiteId;
  readonly direction: "encrypt" | "decrypt";
  private readonly salt: Bytes;
  private readonly aead: Aead;
  private nextCtr = 0n;
  private readonly maxCtr: bigint;

  private constructor(kid: bigint, suite: CipherSuiteId, direction: "encrypt" | "decrypt", salt: Bytes, aead: Aead, maxCtr: bigint) {
    this.kid = kid;
    this.suite = suite;
    this.direction = direction;
    this.salt = salt;
    this.aead = aead;
    this.maxCtr = maxCtr;
  }

  /**
   * @param maxCtr encryption refuses CTR values above this (default 2^32 - 1);
   * the key manager rekeys long before that.
   */
  static async create(opts: {
    kid: bigint;
    baseKey: Uint8Array;
    direction: "encrypt" | "decrypt";
    suite?: CipherSuiteId;
    maxCtr?: bigint;
  }): Promise<SFrameKey> {
    const suite = opts.suite ?? MEDIA_SUITE;
    const { key, salt } = deriveKeySalt(suite, opts.kid, opts.baseKey);
    return new SFrameKey(opts.kid, suite, opts.direction, salt, await makeAead(suite, key), opts.maxCtr ?? 0xffff_ffffn);
  }

  get counter(): bigint {
    return this.nextCtr;
  }

  /** Tag + header overhead for a given CTR. */
  overhead(ctr: bigint = this.nextCtr): number {
    return encodeHeader(this.kid, ctr).length + this.aead.nt;
  }

  /**
   * encrypt(CTR, KID, metadata, plaintext) with an internal monotonic CTR.
   * The counter is consumed before the AEAD call, so a failed or concurrent
   * call can never reuse a nonce.
   */
  async encrypt(plaintext: Uint8Array, metadata: Uint8Array = new Uint8Array(0)): Promise<Bytes> {
    if (this.direction !== "encrypt") throw new E2EEError("state", "decrypt-only key");
    const ctr = this.nextCtr;
    if (ctr > this.maxCtr) throw new E2EEError("counter_exhausted", "rekey required");
    this.nextCtr = ctr + 1n;
    return this.encryptWithCtr(ctr, plaintext, metadata);
  }

  /** Explicit-CTR form, used by the RFC test vectors only. */
  async encryptWithCtr(ctr: bigint, plaintext: Uint8Array, metadata: Uint8Array): Promise<Bytes> {
    const header = encodeHeader(this.kid, ctr);
    const ct = await this.aead.encrypt(makeNonce(this.salt, ctr), concat(header, metadata), plaintext);
    return concat(header, ct);
  }

  /** decrypt(metadata, sframe_ciphertext). The caller handles replay. */
  async decrypt(ciphertext: Uint8Array, metadata: Uint8Array = new Uint8Array(0), header?: SFrameHeader): Promise<Bytes> {
    if (this.direction !== "decrypt") throw new E2EEError("state", "encrypt-only key");
    const h = header ?? decodeHeader(ciphertext);
    if (h.kid !== this.kid) throw new E2EEError("unknown_key", "KID mismatch");
    const body = ciphertext.subarray(h.bytes.length);
    if (body.length < this.aead.nt) throw new E2EEError("malformed", "SFrame shorter than tag");
    return this.aead.decrypt(makeNonce(this.salt, h.ctr), concat(h.bytes, metadata), body);
  }
}

// ------------------------------------------------------------ replay window

/**
 * Counter-based anti-replay (RFC 9605 §9.3, adapted from RFC 3711 §3.3.2).
 * `check` is called before decryption and again right after the AEAD
 * verified, and `commit` follows the second check synchronously, so forged
 * frames cannot advance the window and two concurrent decryptions of the
 * same CTR cannot both pass.
 */
export class ReplayWindow {
  private top = -1n;
  private readonly bits: Uint32Array;
  readonly size: number;

  constructor(size = 1024) {
    if (size < 32 || size % 32 !== 0) throw new E2EEError("invalid_argument", "window must be a multiple of 32");
    this.size = size;
    this.bits = new Uint32Array(size / 32);
  }

  private bit(ctr: bigint): [number, number] {
    const i = Number(ctr % BigInt(this.size));
    return [i >>> 5, 1 << (i & 31)];
  }

  /** True if `ctr` is new and inside the window. */
  check(ctr: bigint): boolean {
    if (ctr > this.top) return true;
    if (this.top - ctr >= BigInt(this.size)) return false;
    const [w, m] = this.bit(ctr);
    return ((this.bits[w] ?? 0) & m) === 0;
  }

  commit(ctr: bigint): void {
    if (ctr > this.top) {
      const shift = ctr - this.top;
      if (shift >= BigInt(this.size)) {
        this.bits.fill(0);
      } else {
        for (let c = this.top + 1n; c < ctr; c++) {
          const [w, m] = this.bit(c);
          this.bits[w] = (this.bits[w] ?? 0) & ~m;
        }
      }
      this.top = ctr;
    }
    const [w, m] = this.bit(ctr);
    this.bits[w] = (this.bits[w] ?? 0) | m;
  }
}
