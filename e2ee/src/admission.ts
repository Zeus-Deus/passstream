/**
 * PIN admission and media-key delivery over an untrusted relay.
 *
 *   Viewer (B, responder)                     Sender (A, initiator)
 *   HELLO  {streamId, viewerSid, nV, mode, grantId} ─────────────▶
 *                                              reserve a guess (PIN mode)
 *          ◀───────────── CHALLENGE {nS, ADa, Ya, sig_S(...)}
 *   verify sig_S with the key pinned in the link; only then use the PIN
 *   RESPONSE {Yb, ADb, Tb} ───────────────────────────────────────▶
 *                                              verify Tb, settle the guess
 *          ◀───────────── ADMIT {Ta, AEAD_box(epoch key, grant)}
 *                    or ◀ REJECT {reason, retryAfter, triesLeft, sig_S}
 *
 * CPace is CPACE-RISTR255-SHA512 from draft-irtf-cfrg-cpace-21 in
 * initiator-responder mode with explicit key confirmation per §10.4.
 * Exact transcripts and labels are in docs/PROTOCOL.md.
 */
import { hmac } from "@noble/hashes/hmac.js";
import { sha512 } from "@noble/hashes/sha2.js";
import type { GuessBudget } from "./budget.ts";
import { type Bytes, concat, copy, ctEqual, lvCat, randomBytes, toBase64Url, u16be, u32be, u64be, u8, utf8 } from "./bytes.ts";
import { deriveSessionKeys, type EpochKey, EPOCH_SECRET_BYTES, open, seal, type SenderSession, type SessionKeys, ViewerSession } from "./channel.ts";
import { calculateGenerator, deriveIsk, deriveMacKey, isIdentity, sampleScalar, scalarMult, scalarMultVfy, transcriptIr } from "./cpace.ts";
import { E2EEError } from "./errors.ts";
import type { Grant, GrantStore } from "./grants.ts";
import { assertLinkSecret, type SenderIdentity, verifySignature } from "./identity.ts";
import { rowCodeFromIndex, rowCodeIndex } from "./row-code.ts";
import { MEDIA_SUITE } from "./sframe.ts";
import {
  AdmissionMode,
  type AdmissionModeId,
  type Challenge,
  decodeMessage,
  encodeMessage,
  type Hello,
  MsgType,
  type Rekey,
  type Reject,
  RejectReason,
  type RejectReasonId,
  rejectReasonName,
  type Response,
} from "./wire.ts";

export const PROTOCOL_VERSION = 1;
const FLAG_PIN_REQUIRED = 0x01;
const MAX_PIN_BYTES = 256;
const ZERO_NONCE = new Uint8Array(12);
const PENDING_AAD = utf8("PassStream v1 pending");
const ADMIT_AAD = utf8("PassStream v1 admit");

// ------------------------------------------------------------ transcripts

/** PIN normalisation: NFC, as RFC 8265 OpaqueString would for digits and passphrases. */
export function normalizePin(pin: string): Bytes {
  const b = utf8(pin.normalize("NFC"));
  if (b.length === 0 || b.length > MAX_PIN_BYTES) throw new E2EEError("invalid_argument", "PIN must be 1–256 bytes");
  // Control characters are almost certainly input bugs; refuse rather than guess.
  if (/\p{Cc}/u.test(pin)) throw new E2EEError("invalid_argument", "PIN contains control characters");
  return b;
}

/**
 * PRS for PIN mode: lv_cat(label, linkSecret, pin). The 32-byte link secret
 * from the fragment is mandatory; the PIN part is empty for a no-PIN stream.
 */
export function pinPrs(pin: string | null, linkSecret: Uint8Array): Bytes {
  assertLinkSecret(linkSecret);
  return lvCat(utf8("PassStream v1 PRS pin"), linkSecret, pin === null ? new Uint8Array(0) : normalizePin(pin));
}

export function grantPrs(secret: Uint8Array): Bytes {
  return lvCat(utf8("PassStream v1 PRS grant"), secret);
}

export interface HandshakeIds {
  streamId: Bytes;
  senderKey: Bytes;
  viewerSid: Bytes;
  mode: AdmissionModeId;
  grantId: Bytes;
}

/** CI: role-tagged identities, initiator first (draft §10.1.1). Never sent. */
export function channelIdentifier(ids: HandshakeIds): Bytes {
  return lvCat(
    utf8("PassStream v1 CPace CI"),
    ids.streamId,
    utf8("initiator:sender"),
    ids.senderKey,
    utf8("responder:viewer"),
    ids.viewerSid,
    u8(ids.mode),
    ids.grantId,
  );
}

export function encodeAda(epoch: number, pinRequired: boolean): Bytes {
  return concat(u8(PROTOCOL_VERSION), u32be(epoch), u16be(MEDIA_SUITE), u8(pinRequired ? FLAG_PIN_REQUIRED : 0));
}

export function decodeAda(ada: Uint8Array): { epoch: number; pinRequired: boolean } {
  const dv = new DataView(ada.buffer, ada.byteOffset, ada.byteLength);
  if (ada.length !== 8 || ada[0] !== PROTOCOL_VERSION || dv.getUint16(5) !== MEDIA_SUITE || ((ada[7] ?? 0) & ~FLAG_PIN_REQUIRED) !== 0) {
    throw new E2EEError("malformed", "unsupported ADa");
  }
  return { epoch: dv.getUint32(1), pinRequired: ((ada[7] ?? 0) & FLAG_PIN_REQUIRED) !== 0 };
}

const ADB = u8(PROTOCOL_VERSION);

export function challengeSigInput(ids: HandshakeIds, nV: Uint8Array, c: Pick<Challenge, "nonce" | "ada" | "ya">): Bytes {
  return lvCat(utf8("PassStream v1 challenge"), ids.streamId, ids.senderKey, ids.viewerSid, nV, u8(ids.mode), ids.grantId, c.nonce, c.ada, c.ya);
}

export function rejectSigInput(
  ids: HandshakeIds,
  nV: Uint8Array,
  nS: Uint8Array,
  r: Pick<Reject, "reason" | "retryAfterMs" | "triesLeft">,
): Bytes {
  return lvCat(
    utf8("PassStream v1 reject"),
    ids.streamId,
    ids.senderKey,
    ids.viewerSid,
    nV,
    nS,
    u8(r.reason),
    u32be(r.retryAfterMs),
    u8(r.triesLeft),
  );
}

interface Confirmed {
  isk: Bytes;
  ta: Bytes;
  tb: Bytes;
  keys: SessionKeys;
}

async function confirmKeys(sid: Bytes, k: Bytes, ya: Bytes, ada: Bytes, yb: Bytes, adb: Bytes): Promise<Confirmed> {
  const isk = deriveIsk(sid, k, transcriptIr(ya, ada, yb, adb));
  const macKey = deriveMacKey(sid, isk);
  const ta = copy(hmac(sha512, macKey, lvCat(ya, ada)));
  const tb = copy(hmac(sha512, macKey, lvCat(yb, adb)));
  macKey.fill(0);
  return { isk, ta, tb, keys: await deriveSessionKeys(isk, sid) };
}

/**
 * Best-effort erasure of handshake secrets. JavaScript gives no guarantee:
 * the engine may have copied a buffer, strings (the PIN) are immutable and
 * can only be dropped, and non-extractable CryptoKeys are freed by the GC.
 * Zeroing what we own still shortens the window in which a heap dump or a
 * later bug can read them.
 */
function wipe(...bufs: Array<Uint8Array | null | undefined>): void {
  for (const b of bufs) b?.fill(0);
}

// ------------------------------------------------------------ admit bundle

export interface AdmitBundle {
  rowCode: string;
  epoch: EpochKey;
  grant: { id: Bytes; secret: Bytes; expiresAt: number };
}

/** version:u8 || epoch:u32 || suite:u16 || secret:32 || grantId:16 || grantSecret:32 || expiresAt:u64 || code_index:u16 */
function encodeBundle(b: AdmitBundle): Bytes {
  return concat(u8(PROTOCOL_VERSION), u32be(b.epoch.epoch), u16be(MEDIA_SUITE), b.epoch.secret, b.grant.id, b.grant.secret, u64be(b.grant.expiresAt), u16be(rowCodeIndex(b.rowCode)));
}

const BUNDLE_LEN = 1 + 4 + 2 + EPOCH_SECRET_BYTES + 16 + 32 + 8 + 2;

function decodeBundle(b: Uint8Array): AdmitBundle {
  if (b.length !== BUNDLE_LEN || b[0] !== PROTOCOL_VERSION) throw new E2EEError("malformed", "bad admit bundle");
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  if (dv.getUint16(5) !== MEDIA_SUITE) throw new E2EEError("malformed", "unsupported media suite");
  let o = 7;
  const take = (n: number) => copy(b.subarray(o, (o += n)));
  const secret = take(EPOCH_SECRET_BYTES);
  const id = take(16);
  const gs = take(32);
  return { rowCode: rowCodeFromIndex(dv.getUint16(BUNDLE_LEN - 2)), epoch: { epoch: dv.getUint32(1), secret }, grant: { id, secret: gs, expiresAt: Number(dv.getBigUint64(o)) } };
}

// ------------------------------------------------------------ sender

/** What a verified handshake asks the sender to register. */
export interface AdmissionCommit {
  peerId: string;
  rowCode?: string;
  mode: AdmissionModeId;
  viewerSid: Bytes;
  /** Grant mode: the grant the viewer proved knowledge of. */
  grant: Grant | null;
  /** The PIN generation the CHALLENGE was computed under (PIN mode). */
  pinGeneration: number;
  keys: SessionKeys;
  /**
   * Call synchronously, immediately before registering the session. Returns
   * false if the handshake was aborted (then refuse); after true, abort() is
   * a no-op because the admission has happened.
   */
  claim(): boolean;
}

export type CommitResult =
  | { ok: true; admissionId: string; session: SenderSession; epoch: EpochKey; grant: Grant }
  | { ok: false; reason: RejectReasonId };

export type KnockPreparation =
  | { ok: true; rowCode: string; timeoutMs: number }
  | { ok: false; reason: RejectReasonId; retryAfterMs?: number };

/** What the per-handshake state machine needs from the sender. */
export interface SenderAdmissionContext {
  identity: SenderIdentity;
  streamId: string;
  /** Current PIN, or null for an explicit "no PIN" stream. */
  pin(): string | null;
  /** Bumped by every PIN rotation; read together with pin(). */
  pinGeneration(): number;
  /** The mandatory 32-byte fragment secret. */
  linkSecret: Uint8Array;
  budget: GuessBudget;
  grants: GrantStore;
  /** Active epoch number, advertised (and signed) in ADa. */
  currentEpoch(): number;
  /**
   * Re-validate and register a verified handshake. Must be serialised with
   * kick and PIN rotation, and must check revocation (PIN generation, grant
   * validity, abort) immediately before registering the session.
   */
  commit(adm: SenderAdmission, c: AdmissionCommit): Promise<CommitResult>;
  /** Locked streams take no new PIN admissions (grants still work). */
  locked?(): boolean;
  blocked?(): boolean;
  prepareKnock?(adm: SenderAdmission, c: AdmissionCommit): Promise<KnockPreparation>;
  now?(): number;
}

/** hello: HELLO being processed; committing: key confirmed, waiting for commit(). */
type SenderState = "idle" | "hello" | "challenged" | "pending" | "committing" | "done";

export type SenderOutcome =
  | { kind: "admitted"; admissionId: string; session: SenderSession; mode: AdmissionModeId }
  | { kind: "rejected"; reason: string }
  | { kind: "aborted"; reason: string };

export class SenderAdmission {
  readonly peerId: string;
  readonly #ctx: SenderAdmissionContext;
  #state: SenderState = "idle";
  #outcome: SenderOutcome | null = null;
  #aborted = false;
  #claimed = false;
  // Handshake material, set on HELLO and wiped when the handshake ends.
  #ids!: HandshakeIds;
  #nV!: Bytes;
  #nS!: Bytes;
  #ya: Bytes | null = null;
  #Ya!: Bytes;
  #ada!: Bytes;
  #ticket: string | null = null;
  #grant: Grant | null = null;
  #grantId: string | null = null;
  #pinGeneration = 0;
  #startedAt = 0;
  #verified: { conf: Confirmed; commit: AdmissionCommit } | null = null;

  constructor(ctx: SenderAdmissionContext, peerId: string) {
    this.#ctx = ctx;
    this.peerId = peerId;
  }

  get state(): SenderState {
    return this.#state;
  }
  get outcome(): SenderOutcome | null {
    return this.#outcome;
  }
  get aborted(): boolean {
    return this.#aborted;
  }
  /** Viewer session id (base64url) once HELLO arrived. */
  get viewerSid(): string | null {
    return this.#ids ? toBase64Url(this.#ids.viewerSid) : null;
  }
  /** Grant mode: the grant id (base64url) this handshake claims, so a kick can abort it. */
  get grantId(): string | null {
    return this.#grantId;
  }
  /** When the CHALLENGE went out (0 before). */
  get startedAt(): number {
    return this.#startedAt;
  }

  /** Test hook: names of handshake secrets still held (empty once finished). */
  _retainedSecrets(): string[] {
    const out: string[] = [];
    if (this.#ya) out.push("ya");
    if (this.#grant) out.push("grant");
    return out;
  }

  #now(): number {
    return (this.#ctx.now ?? Date.now)();
  }

  #rejection(reason: RejectReasonId, retryAfterMs: number, triesLeft: number): Bytes {
    const r = { reason, retryAfterMs: Math.min(0xffff_ffff, Math.max(0, Math.ceil(retryAfterMs))), triesLeft: Math.min(255, triesLeft) };
    const nS = this.#nS ?? new Uint8Array(0);
    const sig = this.#ctx.identity.sign(rejectSigInput(this.#ids, this.#nV, nS, r));
    return encodeMessage({ type: MsgType.Reject, ...r, sig });
  }

  #reject(reason: RejectReasonId, retryAfterMs: number, triesLeft: number): Bytes {
    const reply = this.#rejection(reason, retryAfterMs, triesLeft);
    this.#finish(this.#aborted ? null : { kind: "rejected", reason: rejectReasonName(reason) });
    return reply;
  }

  #finish(outcome: SenderOutcome | null): void {
    this.#state = "done";
    if (outcome && !this.#aborted) this.#outcome = outcome;
    wipe(this.#ya, this.#grant?.secret);
    this.#ya = null;
    this.#grant = null;
    this.#verified = null;
  }

  /**
   * Consume one message from the viewer, return the messages to send back.
   * Throws E2EEError on malformed or out-of-order input; the caller should
   * then call abort() (which settles a reserved guess as abandoned).
   */
  async handle(bytes: Uint8Array): Promise<Bytes[]> {
    const m = decodeMessage(bytes);
    if (this.#state === "idle" && m.type === MsgType.Hello) return [await this.#onHello(m)];
    if (this.#state === "challenged" && m.type === MsgType.Response) {
      try {
        return [await this.#onResponse(m)];
      } catch (e) {
        this.#finish(null); // never left stuck in "committing"; idempotent
        throw e;
      }
    }
    throw new E2EEError("unexpected_message", `type ${m.type} in state ${this.#state}`);
  }

  #checkAborted(where: string): void {
    if (!this.#aborted) return;
    this.#finish(null);
    throw new E2EEError("state", `aborted during ${where}`);
  }

  async #onHello(h: Hello): Promise<Bytes> {
    const ctx = this.#ctx;
    if (toBase64Url(h.streamId) !== toBase64Url(utf8(ctx.streamId))) {
      throw new E2EEError("unexpected_message", "HELLO for another stream");
    }
    this.#ids = { streamId: h.streamId, senderKey: ctx.identity.publicKey, viewerSid: h.viewerSid, mode: h.mode, grantId: h.grantId };
    this.#nV = h.nonce;
    this.#state = "hello"; // claim the state before awaiting
    // Read the PIN and its generation together: commit() refuses if a rotation came in between.
    const pin = ctx.pin();
    this.#pinGeneration = ctx.pinGeneration();
    let prs: Bytes;
    if (ctx.blocked?.()) return this.#reject(RejectReason.Locked, 0, 0);
    if (h.mode === AdmissionMode.Grant) {
      this.#grantId = toBase64Url(h.grantId);
      this.#grant = await ctx.grants.lookup(h.grantId, h.viewerSid);
      this.#checkAborted("HELLO");
      if (!this.#grant) return this.#reject(RejectReason.GrantInvalid, 0, 0);
      prs = grantPrs(this.#grant.secret);
    } else {
      if (ctx.locked?.()) return this.#reject(RejectReason.Locked, 0, 0);
      prs = pinPrs(pin, ctx.linkSecret);
      if (pin !== null) {
        // Persist the reservation *before* any PIN-dependent value leaves.
        const r = await ctx.budget.reserve(toBase64Url(h.viewerSid));
        if (!r.ok) {
          wipe(prs);
          return this.#reject(r.reason === "frozen" ? RejectReason.Frozen : RejectReason.Backoff, r.retryAfterMs, r.triesLeft);
        }
        this.#ticket = r.ticket;
        if (this.#aborted) {
          wipe(prs);
          await this.#settle("abandoned");
          this.#checkAborted("HELLO");
        }
      }
    }
    this.#startedAt = this.#now();
    this.#nS = randomBytes(32);
    this.#ada = encodeAda(ctx.currentEpoch(), h.mode === AdmissionMode.Pin && pin !== null);
    const g = calculateGenerator(prs, channelIdentifier(this.#ids), concat(this.#nV, this.#nS));
    wipe(prs);
    this.#ya = sampleScalar();
    this.#Ya = scalarMult(this.#ya, g);
    const c = { nonce: this.#nS, ada: this.#ada, ya: this.#Ya };
    const sig = ctx.identity.sign(challengeSigInput(this.#ids, this.#nV, c));
    this.#state = "challenged";
    return encodeMessage({ type: MsgType.Challenge, ...c, sig });
  }

  async #onResponse(r: Response): Promise<Bytes> {
    const ctx = this.#ctx;
    this.#state = "committing";
    if (this.#now() - this.#startedAt > ctx.budget.limits.handshakeTimeoutMs) {
      await this.#settle("timeout");
      return this.#reject(RejectReason.Expired, 0, 0);
    }
    const sid = concat(this.#nV, this.#nS);
    const k = scalarMultVfy(this.#ya!, r.yb);
    if (isIdentity(k) || r.adb.length !== 1 || r.adb[0] !== PROTOCOL_VERSION) {
      const s = await this.#settle("invalid");
      return this.#reject(RejectReason.Invalid, s.retryAfterMs, s.triesLeft);
    }
    const conf = await confirmKeys(sid, k, this.#Ya, this.#ada, r.yb, r.adb);
    wipe(k, conf.isk);
    if (!ctEqual(conf.tb, r.tb)) {
      const s = await this.#settle("wrong_pin");
      if (this.#ids.mode === AdmissionMode.Grant) return this.#reject(RejectReason.GrantInvalid, 0, 0);
      return this.#reject(s.frozen ? RejectReason.Frozen : RejectReason.WrongPin, s.retryAfterMs, s.triesLeft);
    }
    await this.#settle("success");
    const commit: AdmissionCommit = {
      peerId: this.peerId,
      mode: this.#ids.mode,
      viewerSid: this.#ids.viewerSid,
      grant: this.#grant,
      pinGeneration: this.#pinGeneration,
      keys: conf.keys,
      claim: () => {
        if (this.#aborted) return false;
        this.#claimed = true;
        return true;
      },
    };
    // Rotation can finish while budget settlement is awaiting storage.
    if (commit.mode === AdmissionMode.Pin && commit.pinGeneration !== ctx.pinGeneration()) return this.#reject(RejectReason.Expired, 0, 0);
    if (ctx.blocked?.()) return this.#reject(RejectReason.Locked, 0, 0);
    this.#checkAborted("confirmation");
    this.#verified = { conf, commit };
    wipe(this.#ya);
    this.#ya = null;
    if (ctx.prepareKnock && commit.mode === AdmissionMode.Pin) {
      this.#state = "pending";
      const pending = await ctx.prepareKnock(this, commit);
      if (!pending.ok) return this.#reject(pending.reason, pending.retryAfterMs ?? 0, 0);
      this.#checkAborted("pending approval");
      commit.rowCode = pending.rowCode;
      const box = await seal(conf.keys.pending, ZERO_NONCE, PENDING_AAD, concat(u16be(rowCodeIndex(pending.rowCode)), u32be(pending.timeoutMs)));
      this.#checkAborted("PENDING encryption");
      return encodeMessage({ type: MsgType.Pending, box });
    }
    return this.#admit();
  }

  /** Only the host holding this exact pending handshake can approve it. */
  async approve(): Promise<Bytes> {
    if (this.#state !== "pending" || !this.#verified || this.#aborted) throw new E2EEError("unknown_knock", "handshake no longer pending");
    return this.#admit();
  }

  rejectVerified(): Bytes { return this.#reject(RejectReason.Denied, 0, 0); }

  rejectPending(reason: RejectReasonId = RejectReason.Denied): Bytes {
    if (this.#state !== "pending") throw new E2EEError("unknown_knock", "handshake no longer pending");
    return this.#reject(reason, 0, 0);
  }

  async #admit(): Promise<Bytes> {
    this.#state = "committing";
    const { conf, commit } = this.#verified!;
    try {
      const res = await this.#ctx.commit(this, commit);
      if (!res.ok) return this.#reject(res.reason, 0, 0);
      const bundle = encodeBundle({ epoch: res.epoch, grant: res.grant, rowCode: res.grant.rowCode });
      const box = await seal(conf.keys.box, ZERO_NONCE, ADMIT_AAD, bundle);
      wipe(bundle, res.grant.secret);
      this.#finish({ kind: "admitted", admissionId: res.admissionId, session: res.session, mode: this.#ids.mode });
      return encodeMessage({ type: MsgType.Admit, ta: conf.ta, box });
    } catch (e) {
      this.#finish(null);
      throw e;
    }
  }

  async #settle(outcome: "success" | "wrong_pin" | "invalid" | "abandoned" | "timeout") {
    const t = this.#ticket;
    this.#ticket = null;
    if (t === null) return { triesLeft: 0, retryAfterMs: 0, frozen: false };
    return this.#ctx.budget.settle(t, outcome);
  }

  /**
   * Transport closed, handshake given up, or revoked by an owner action: a
   * reserved guess counts as used. A handshake that is committing is marked
   * aborted and commit() refuses it.
   */
  async abort(reason = "abandoned", rejection?: RejectReasonId): Promise<Bytes | null> {
    if (this.#state === "done" || this.#aborted || this.#claimed) return null;
    // Sign while the transcript is intact. Owner cancellation is not a new
    // protocol message and must not refund a reserved PIN guess.
    const reply = rejection !== undefined && this.#state !== "idle" ? this.#rejection(rejection, 0, 0) : null;
    this.#aborted = true;
    this.#outcome = { kind: "aborted", reason };
    if (this.#state === "committing") return reply; // #onResponse finishes and rejects
    const inHello = this.#state === "hello";
    this.#state = "done";
    await this.#settle("abandoned");
    // #onHello still owns the material while its awaits run; it wipes on its way out.
    if (!inHello) this.#finish(null);
    return reply;
  }
}

// ------------------------------------------------------------ viewer

export type ViewerTickStatus = { state: ViewerState; reason?: "knock_timeout" };

export interface ViewerAdmissionOptions {
  now?(): number;
  streamId: string;
  /** Ed25519 public key from the link fragment. */
  senderKey: Uint8Array;
  /** 16 random bytes identifying this viewer session (kept across reloads). */
  viewerSid: Uint8Array;
  /** The mandatory 32-byte link secret from the fragment, never sent to the server. */
  linkSecret: Uint8Array;
  /** PIN admission: the PIN typed by the viewer, or null for a no-PIN stream. */
  pin?: string | null;
  /** Grant re-admission instead of a PIN. */
  grant?: { id: Uint8Array; secret: Uint8Array };
}

export interface ViewerAdmissionResult {
  rowCode: string;
  session: ViewerSession;
  /** Epoch keys in order: the bundle's, then any rekeys that raced the ADMIT. */
  epochs: EpochKey[];
  grant: { id: Bytes; secret: Bytes; expiresAt: number };
  /** Acks for rekeys that raced the ADMIT; send them. */
  acks: Bytes[];
}

type ViewerState = "idle" | "hello_sent" | "response_sent" | "pending" | "admitted" | "failed";

const MAX_EARLY_REKEYS = 8;

export class ViewerAdmission {
  readonly #ids: HandshakeIds;
  readonly #now: () => number;
  #pending: { rowCode: string; timeoutMs: number; expiresAt: number } | null = null;
  #state: ViewerState = "idle";
  // Secrets, copied from the options and wiped (or dropped) when the handshake ends.
  #pin: string | null | undefined;
  #linkSecret: Bytes | null;
  #grantSecret: Bytes | null;
  #nV: Bytes = new Uint8Array(0);
  #nS: Bytes = new Uint8Array(0);
  #conf: Omit<Confirmed, "isk"> | null = null;
  #adaEpoch = 0;
  #early: Rekey[] = [];
  #result: ViewerAdmissionResult | null = null;
  #aborted = false;

  constructor(o: ViewerAdmissionOptions) {
    this.#now = o.now ?? Date.now;
    if (o.senderKey.length !== 32) throw new E2EEError("invalid_argument", "sender key must be 32 bytes");
    if (o.viewerSid.length !== 16) throw new E2EEError("invalid_argument", "viewerSid must be 16 bytes");
    assertLinkSecret(o.linkSecret);
    if (o.grant && (o.grant.id.length !== 16 || o.grant.secret.length !== 32)) throw new E2EEError("invalid_argument", "bad grant");
    if (!o.grant && o.pin === undefined) throw new E2EEError("invalid_argument", "need a pin (or null) or a grant");
    if (!o.grant && o.pin !== null && o.pin !== undefined) normalizePin(o.pin); // validate early
    this.#pin = o.grant ? undefined : o.pin;
    this.#linkSecret = copy(o.linkSecret);
    this.#grantSecret = o.grant ? copy(o.grant.secret) : null;
    this.#ids = {
      streamId: utf8(o.streamId),
      senderKey: copy(o.senderKey),
      viewerSid: copy(o.viewerSid),
      mode: o.grant ? AdmissionMode.Grant : AdmissionMode.Pin,
      grantId: o.grant ? copy(o.grant.id) : new Uint8Array(0),
    };
  }

  get state(): ViewerState {
    return this.#state;
  }
  get result(): ViewerAdmissionResult | null {
    return this.#result;
  }
  get pending(): Readonly<{ rowCode: string; timeoutMs: number; expiresAt: number }> | null {
    return this.#pending ? { ...this.#pending } : null;
  }

  /** Drop an unfinished check. Late WebCrypto completions cannot release its result. */
  abort(): void {
    if (this.#state === "admitted") return;
    this.#aborted = true;
    this.#state = "failed";
    this.#wipe();
  }

  /** Call periodically while waiting, including while signaling is disconnected. */
  tick(): ViewerTickStatus {
    if (this.#state === "pending" && this.#now() >= this.#pending!.expiresAt) {
      this.#state = "failed";
      this.#wipe();
      return { state: "failed", reason: "knock_timeout" };
    }
    return { state: this.#state };
  }


  /** Test hook: names of handshake secrets still held (empty once finished). */
  _retainedSecrets(): string[] {
    const out: string[] = [];
    if (this.#pin !== undefined) out.push("pin");
    if (this.#linkSecret) out.push("linkSecret");
    if (this.#grantSecret) out.push("grantSecret");
    if (this.#conf) out.push("sessionKeys");
    return out;
  }

  #wipe(): void {
    wipe(this.#linkSecret, this.#grantSecret);
    this.#pin = undefined; // strings can only be dropped
    this.#linkSecret = null;
    this.#grantSecret = null;
    this.#conf = null;
    this.#early = [];
    this.#pending = null;
  }

  start(): Bytes {
    if (this.#state !== "idle") throw new E2EEError("state", "already started");
    this.#nV = randomBytes(32);
    this.#state = "hello_sent";
    return encodeMessage({
      type: MsgType.Hello,
      streamId: this.#ids.streamId,
      viewerSid: this.#ids.viewerSid,
      nonce: this.#nV,
      mode: this.#ids.mode,
      grantId: this.#ids.grantId,
    });
  }

  /** Any error moves the handshake to "failed"; start a new ViewerAdmission to retry. */
  async handle(bytes: Uint8Array): Promise<Bytes[]> {
    try {
      if (this.#aborted) throw new E2EEError("state", "admission aborted");
      if (this.tick().reason === "knock_timeout") throw new E2EEError("rejected", "knock timeout", { reason: "denied" });
      const out = await this.#handle(bytes);
      if (this.#aborted) {
        this.#result?.session.dispose();
        this.#result?.epochs.forEach((k) => k.secret.fill(0));
        this.#result?.grant.secret.fill(0);
        this.#result = null;
        this.#state = "failed";
        throw new E2EEError("state", "admission aborted");
      }
      return out;
    } catch (e) {
      if (this.#state !== "admitted") {
        this.#state = "failed";
        this.#wipe();
      }
      throw e;
    }
  }

  async #handle(bytes: Uint8Array): Promise<Bytes[]> {
    const m = decodeMessage(bytes);
    if (m.type === MsgType.Reject && (this.#state === "hello_sent" || this.#state === "response_sent" || this.#state === "pending")) {
      if (!verifySignature(this.#ids.senderKey, rejectSigInput(this.#ids, this.#nV, this.#nS, m), m.sig)) {
        throw new E2EEError("bad_signature", "REJECT not signed by the pinned sender key");
      }
      throw new E2EEError("rejected", rejectReasonName(m.reason), {
        reason: rejectReasonName(m.reason),
        retryAfterMs: m.retryAfterMs,
        triesLeft: m.triesLeft,
      });
    }
    if (this.#state === "hello_sent" && m.type === MsgType.Challenge) return [await this.#onChallenge(m)];
    if (this.#state === "response_sent" && m.type === MsgType.Pending) {
      const plain = await open(this.#conf!.keys.pending, ZERO_NONCE, PENDING_AAD, m.box);
      const dv = new DataView(plain.buffer, plain.byteOffset, plain.byteLength);
      const rowCode = rowCodeFromIndex(dv.getUint16(0));
      const timeoutMs = dv.getUint32(2);
      wipe(plain);
      this.#pending = { rowCode, timeoutMs, expiresAt: this.#now() + timeoutMs + 5_000 };
      this.#state = "pending";
      return [];
    }
    if ((this.#state === "response_sent" || this.#state === "pending") && m.type === MsgType.Admit) return this.#onAdmit(m.ta, m.box);
    if ((this.#state === "response_sent" || this.#state === "pending") && m.type === MsgType.Rekey) {
      // A rekey can overtake the ADMIT; hold it until we have the session.
      if (this.#early.length >= MAX_EARLY_REKEYS) throw new E2EEError("unexpected_message", "too many early rekeys");
      this.#early.push(m);
      return [];
    }
    throw new E2EEError("unexpected_message", `type ${m.type} in state ${this.#state}`);
  }

  async #onChallenge(c: Challenge): Promise<Bytes> {
    // 1. Authenticate the sender before anything PIN-dependent is computed or sent.
    if (!verifySignature(this.#ids.senderKey, challengeSigInput(this.#ids, this.#nV, c), c.sig)) {
      throw new E2EEError("bad_signature", "CHALLENGE not signed by the pinned sender key");
    }
    const ada = decodeAda(c.ada);
    if (this.#ids.mode === AdmissionMode.Pin && ada.pinRequired && this.#pin === null) {
      throw new E2EEError("rejected", "pin_required", { reason: "pin_required", retryAfterMs: 0, triesLeft: 0 });
    }
    this.#nS = c.nonce;
    this.#adaEpoch = ada.epoch;
    const prs = this.#grantSecret ? grantPrs(this.#grantSecret) : pinPrs(ada.pinRequired ? (this.#pin ?? null) : null, this.#linkSecret!);
    const sid = concat(this.#nV, this.#nS);
    const g = calculateGenerator(prs, channelIdentifier(this.#ids), sid);
    wipe(prs);
    const yb = sampleScalar();
    const Yb = scalarMult(yb, g);
    const k = scalarMultVfy(yb, c.ya);
    wipe(yb);
    if (isIdentity(k)) throw new E2EEError("invalid_point", "sender share is invalid");
    const { isk, ...conf } = await confirmKeys(sid, k, c.ya, c.ada, Yb, ADB);
    wipe(k, isk);
    this.#conf = conf;
    // The password inputs are no longer needed: everything else derives from the session keys.
    this.#pin = undefined;
    wipe(this.#linkSecret, this.#grantSecret);
    this.#linkSecret = null;
    this.#grantSecret = null;
    this.#state = "response_sent";
    return encodeMessage({ type: MsgType.Response, yb: Yb, adb: ADB, tb: conf.tb });
  }

  async #onAdmit(ta: Bytes, box: Bytes): Promise<Bytes[]> {
    const conf = this.#conf!;
    if (!ctEqual(conf.ta, ta)) throw new E2EEError("bad_mac", "sender key confirmation failed");
    const plain = await open(conf.keys.box, ZERO_NONCE, ADMIT_AAD, box);
    const bundle = decodeBundle(plain);
    wipe(plain);
    if (this.#pending && this.#pending.rowCode !== bundle.rowCode) throw new E2EEError("malformed", "admit row code differs from pending");
    // A rekey may land between CHALLENGE and ADMIT, never a step back.
    if (bundle.epoch.epoch < this.#adaEpoch) throw new E2EEError("malformed", "admit epoch older than challenge epoch");
    const session = new ViewerSession(conf.keys, bundle.epoch.epoch);
    session.addEpoch(bundle.epoch);
    const epochs: EpochKey[] = [bundle.epoch];
    const acks: Bytes[] = [];
    for (const r of this.#early) {
      const { epoch, ack } = await session.handleRekey(r);
      epochs.push(epoch);
      acks.push(ack);
    }
    this.#state = "admitted";
    this.#wipe();
    this.#result = { session, epochs, grant: bundle.grant, rowCode: bundle.rowCode, acks };
    return acks;
  }
}

/** Exposed for the adversarial tests, which play a relay that computes everything it can. */
export const _internal = { confirmKeys, encodeBundle, decodeBundle };
