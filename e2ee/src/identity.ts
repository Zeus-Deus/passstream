/**
 * Sender identity: an Ed25519 keypair generated on the sender. The public key
 * travels in the share link's #fragment, next to the capability, so a viewer
 * can authenticate the *sender* and not just the server. Signatures use
 * RFC 8032 Ed25519 with strict (non-ZIP-215) verification.
 */
import { ed25519 } from "@noble/curves/ed25519.js";
import { type Bytes, copy, fromBase64Url, randomBytes, toBase64Url } from "./bytes.ts";
import { E2EEError } from "./errors.ts";

export const SENDER_KEY_LENGTH = 32;

export class SenderIdentity {
  readonly publicKey: Bytes;
  readonly #secretKey: Bytes;

  private constructor(secretKey: Bytes) {
    this.#secretKey = secretKey;
    this.publicKey = copy(ed25519.getPublicKey(secretKey));
  }

  static generate(): SenderIdentity {
    return new SenderIdentity(copy(ed25519.utils.randomSecretKey()));
  }

  /**
   * Restore from the 32-byte seed. The sender must keep the seed for the
   * lifetime of the link (the link pins the public key), in storage that only
   * the sender can read.
   */
  static fromSecretKey(seed: Uint8Array): SenderIdentity {
    if (seed.length !== 32) throw new E2EEError("invalid_argument", "Ed25519 seed must be 32 bytes");
    return new SenderIdentity(copy(seed));
  }

  exportSecretKey(): Bytes {
    return copy(this.#secretKey);
  }

  sign(message: Uint8Array): Bytes {
    return copy(ed25519.sign(message, this.#secretKey));
  }
}

export function verifySignature(publicKey: Uint8Array, message: Uint8Array, sig: Uint8Array): boolean {
  if (publicKey.length !== 32 || sig.length !== 64) return false;
  try {
    return ed25519.verify(sig, message, publicKey, { zip215: false });
  } catch {
    return false;
  }
}

/** base64url (43 chars) for the URL fragment. */
export function encodeSenderKey(publicKey: Uint8Array): string {
  if (publicKey.length !== SENDER_KEY_LENGTH) throw new E2EEError("invalid_argument", "bad sender key");
  return toBase64Url(publicKey);
}

/** Parse and validate (must be a canonical point encoding). */
export function decodeSenderKey(s: string): Bytes {
  const b = fromBase64Url(s);
  if (b.length !== SENDER_KEY_LENGTH) throw new E2EEError("malformed", "sender key must be 32 bytes");
  try {
    ed25519.Point.fromBytes(b, false);
  } catch {
    throw new E2EEError("malformed", "sender key is not a valid Ed25519 point");
  }
  return b;
}

export const LINK_SECRET_LENGTH = 32;

/** 32 CSPRNG bytes for the link fragment. Every stream has one. */
export function generateLinkSecret(): Bytes {
  return randomBytes(LINK_SECRET_LENGTH);
}

/** Throws unless `s` is exactly 32 bytes. Both ends call this. */
export function assertLinkSecret(s: unknown): asserts s is Uint8Array {
  if (!(s instanceof Uint8Array) || s.length !== LINK_SECRET_LENGTH) {
    throw new E2EEError("invalid_argument", "link secret must be exactly 32 bytes");
  }
}

export interface LinkFragment {
  capability: string;
  senderKey: Bytes;
  linkSecret: Bytes;
}

const CAPABILITY = /^[A-Za-z0-9_-]+$/;

/**
 * Fragment layout: `c=<capability>&k=<senderKey>&s=<linkSecret>`, all
 * base64url without padding, in that order. The capability is opaque here
 * (the server defines it). The browser never sends the fragment to the
 * server, so `k` and `s` stay between sender and viewers.
 */
export function buildFragment(capability: string, publicKey: Uint8Array, linkSecret: Uint8Array): string {
  if (!CAPABILITY.test(capability)) throw new E2EEError("invalid_argument", "capability must be base64url");
  assertLinkSecret(linkSecret);
  return `c=${capability}&k=${encodeSenderKey(publicKey)}&s=${toBase64Url(linkSecret)}`;
}

/** Strict: only the canonical form `buildFragment` produces is accepted. */
export function parseFragment(fragment: string): LinkFragment {
  const f = fragment.startsWith("#") ? fragment.slice(1) : fragment;
  const m = /^c=([A-Za-z0-9_-]+)&k=([A-Za-z0-9_-]+)&s=([A-Za-z0-9_-]+)$/.exec(f);
  if (!m) throw new E2EEError("malformed", "fragment must be c=…&k=…&s=…");
  const senderKey = decodeSenderKey(m[2]!);
  const linkSecret = fromBase64Url(m[3]!);
  if (linkSecret.length !== LINK_SECRET_LENGTH) throw new E2EEError("malformed", "link secret must be 32 bytes");
  return { capability: m[1]!, senderKey, linkSecret };
}
