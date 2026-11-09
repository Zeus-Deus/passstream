import { E2EEError } from "./errors.ts";

/** Uint8Array backed by a plain ArrayBuffer, as WebCrypto requires. */
export type Bytes = Uint8Array<ArrayBuffer>;

const enc = new TextEncoder();

export function utf8(s: string): Bytes {
  return enc.encode(s);
}

export function utf8Decode(b: Uint8Array): string {
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(b);
  } catch {
    throw new E2EEError("malformed", "invalid UTF-8");
  }
}

/** Copy into a fresh ArrayBuffer-backed array (also normalises noble's outputs). */
export function copy(b: Uint8Array): Bytes {
  const out = new Uint8Array(b.length);
  out.set(b);
  return out;
}

export function concat(...parts: Uint8Array[]): Bytes {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

export function randomBytes(n: number): Bytes {
  const out = new Uint8Array(n);
  // getRandomValues is capped at 65536 bytes per call.
  for (let o = 0; o < n; o += 65536) crypto.getRandomValues(out.subarray(o, Math.min(n, o + 65536)));
  return out;
}

/** Constant-time equality for equal-length secrets (MACs, tags). */
export function ctEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return d === 0;
}

/** LEB128 length prefix, as `prepend_len` in draft-irtf-cfrg-cpace Appendix A.1. */
export function leb128(n: number): Bytes {
  if (!Number.isSafeInteger(n) || n < 0) throw new E2EEError("invalid_argument", "bad length");
  const out: number[] = [];
  for (;;) {
    if (n < 128) {
      out.push(n);
    } else {
      out.push((n & 0x7f) | 0x80);
    }
    n = Math.floor(n / 128);
    if (n === 0) break;
  }
  return Uint8Array.from(out);
}

export function prependLen(data: Uint8Array): Bytes {
  return concat(leb128(data.length), data);
}

/** `lv_cat` from draft-irtf-cfrg-cpace Appendix A.1.3. */
export function lvCat(...args: Uint8Array[]): Bytes {
  return concat(...args.map(prependLen));
}

export function u8(n: number): Bytes {
  return Uint8Array.of(n & 0xff);
}

export function u16be(n: number): Bytes {
  return Uint8Array.of((n >>> 8) & 0xff, n & 0xff);
}

export function u32be(n: number): Bytes {
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, n >>> 0);
  return out;
}

export function u64be(n: bigint | number): Bytes {
  const out = new Uint8Array(8);
  new DataView(out.buffer).setBigUint64(0, BigInt(n));
  return out;
}

export function toHex(b: Uint8Array): string {
  let s = "";
  for (const x of b) s += x.toString(16).padStart(2, "0");
  return s;
}

export function fromHex(h: string): Bytes {
  if (h.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(h)) {
    throw new E2EEError("malformed", "bad hex");
  }
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = Number.parseInt(h.slice(2 * i, 2 * i + 2), 16);
  return out;
}

const B64URL = /^[A-Za-z0-9_-]*$/;

export function toBase64Url(b: Uint8Array): string {
  let bin = "";
  for (const x of b) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function fromBase64Url(s: string): Bytes {
  if (!B64URL.test(s) || s.length % 4 === 1) throw new E2EEError("malformed", "bad base64url");
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  // Reject non-canonical encodings (stray bits in the last character).
  if (toBase64Url(out) !== s) throw new E2EEError("malformed", "non-canonical base64url");
  return out;
}
