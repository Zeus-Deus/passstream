/**
 * Every failure this library raises on purpose is an E2EEError with a stable
 * code. Parsers and state machines never throw anything else for bad input,
 * which the property tests check.
 */
export type E2EEErrorCode =
  | "malformed" // a message or frame failed to parse
  | "unexpected_message" // valid message, wrong state or wrong type
  | "bad_signature" // sender signature did not verify against the pinned key
  | "bad_mac" // key confirmation failed (wrong PIN or tampering)
  | "invalid_point" // CPace share decoded to an invalid or neutral element
  | "rejected" // the sender refused admission (see `reason`)
  | "decrypt_failed" // AEAD authentication failed
  | "replay" // SFrame counter already seen or too old
  | "unknown_key" // no key for this KID / epoch
  | "wrong_track" // frame KID belongs to another track
  | "counter_exhausted" // encryption counter limit reached, rekey required
  | "unsupported_codec" // codec cannot be carried under E2EE
  | "not_supported" // browser lacks RTCRtpScriptTransform or WebCrypto
  | "conflict" // owner action refused by the current admission gates
  | "invalid_argument"
  | "unknown_knock"
  | "unknown_admission"
  | "code_mismatch"
  | "rotation_pending"
  | "state_mismatch"
  | "passphrase_required"
  | "storage" // persistent storage failed or timed out
  | "state"; // API misuse (e.g. calling start twice)

export class E2EEError extends Error {
  readonly code: E2EEErrorCode;
  readonly details: Readonly<Record<string, unknown>> | undefined;

  constructor(code: E2EEErrorCode, message: string, details?: Record<string, unknown>) {
    super(`${code}: ${message}`);
    this.name = "E2EEError";
    this.code = code;
    this.details = details;
  }
}

export function isE2EEError(e: unknown, code?: E2EEErrorCode): e is E2EEError {
  return e instanceof E2EEError && (code === undefined || e.code === code);
}
