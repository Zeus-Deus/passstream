/**
 * Main-thread feature detection and transform attachment. Fail closed: if
 * RTCRtpScriptTransform is missing or not honoured, these throw
 * E2EEError("not_supported"); the app shows "browser not supported" and must
 * never fall back to plaintext.
 */
import type { EpochKey } from "./channel.ts";
import { E2EEError } from "./errors.ts";
import { monotonicNow, type TransformOptions, type WorkerInMessage } from "./worker-runtime.ts";

export type SupportProblem = "insecure_context" | "no_webcrypto" | "no_worker" | "no_script_transform";
export type E2EESupport = { supported: true } | { supported: false; reason: SupportProblem };

interface GlobalLike {
  isSecureContext?: boolean;
  crypto?: { subtle?: unknown };
  Worker?: unknown;
  RTCRtpScriptTransform?: unknown;
}

export function detectE2EESupport(g: GlobalLike = globalThis as GlobalLike): E2EESupport {
  if (g.isSecureContext === false) return { supported: false, reason: "insecure_context" };
  if (!g.crypto?.subtle) return { supported: false, reason: "no_webcrypto" };
  if (typeof g.Worker !== "function") return { supported: false, reason: "no_worker" };
  if (typeof g.RTCRtpScriptTransform !== "function") return { supported: false, reason: "no_script_transform" };
  return { supported: true };
}

export function assertE2EESupported(g?: GlobalLike): void {
  const s = detectE2EESupport(g);
  if (!s.supported) throw new E2EEError("not_supported", `end-to-end encryption unavailable: ${s.reason}`, { reason: s.reason });
}

type ScriptTransformCtor = new (worker: Worker, options?: unknown) => object;

/**
 * Attach an encrypt/decrypt transform to an RTCRtpSender or RTCRtpReceiver
 * and verify the browser kept it.
 */
export function attachTransform(target: { transform: unknown }, worker: Worker, options: TransformOptions, g: GlobalLike = globalThis as GlobalLike): void {
  assertE2EESupported(g);
  const Ctor = g.RTCRtpScriptTransform as ScriptTransformCtor;
  const t = new Ctor(worker, options);
  target.transform = t;
  if (target.transform !== t) throw new E2EEError("not_supported", "browser ignored the RTCRtpScriptTransform");
}

/**
 * Add a track so that no frame can leave unencrypted: the transceiver is
 * created without a track, the transform attached, and only then the track
 * set. If attaching fails the transceiver is stopped and the error thrown.
 */
export async function addEncryptedTrack(
  pc: RTCPeerConnection,
  track: MediaStreamTrack,
  worker: Worker,
  options: Omit<TransformOptions, "role" | "kind">,
  init: RTCRtpTransceiverInit = {},
): Promise<RTCRtpTransceiver> {
  assertE2EESupported();
  const kind = track.kind === "audio" ? "audio" : "video";
  const tr = pc.addTransceiver(kind, { ...init, direction: init.direction ?? "sendonly" });
  try {
    attachTransform(tr.sender, worker, { ...options, role: "encrypt", kind });
  } catch (e) {
    tr.stop();
    throw e;
  }
  await tr.sender.replaceTrack(track);
  return tr;
}

/**
 * Helpers to post epoch keys to the worker (the secret is copied, then
 * transferred). Call postSenderEpoch only from SenderHost's `onEpoch`: an
 * epoch key must reach an encryptor once. After recreating the worker, call
 * `host.replaceEncryptor()` instead of re-posting a key (nonce reuse).
 */
export function postSenderEpoch(worker: Worker, k: EpochKey): void {
  const secret = k.secret.slice().buffer;
  const m: WorkerInMessage = { type: "sender-epoch", epoch: k.epoch, secret };
  worker.postMessage(m, [secret]);
}

/**
 * Call from SenderHost's `onCutoff`, synchronously: the worker enforces the
 * revocation cutoff on its own monotonic clock (see worker-runtime.ts).
 */
export function postSenderCutoff(worker: Worker, c: { within: number; stale: number }): void {
  const m: WorkerInMessage = { type: "sender-cutoff", at: monotonicNow() + c.within, within: c.within, stale: c.stale };
  worker.postMessage(m);
}

/** The longest the curtain fence drops every encoded frame (see worker-runtime.ts "sender-fence"). */
export const CURTAIN_FENCE_MS = 1000;

/**
 * Call synchronously when the privacy curtain (pause) goes down: the worker
 * arms the no-enqueue fence when it receives this message, until
 * postSenderFenceOpen() (outgoing tracks switched to the curtain picture), at
 * most `ms`, then passes video only from a keyframe. Capture/audio disable at
 * the accepted page command; pre-command frames can finish encryption before
 * worker receipt. Once armed, the fence also rejects encryption already in
 * flight, including an arm-and-open while WebCrypto is pending. Previously
 * emitted output and downstream packets cannot be recalled.
 */
export function postSenderFence(worker: Worker, ms = CURTAIN_FENCE_MS): void {
  const m: WorkerInMessage = { type: "sender-fence", at: monotonicNow(), ms };
  worker.postMessage(m);
}

/** The outgoing tracks now carry the curtain picture: end the fence (its first frame is the keyframe viewers need). */
export function postSenderFenceOpen(worker: Worker): void {
  const m: WorkerInMessage = { type: "sender-fence-open" };
  worker.postMessage(m);
}

/** Call from SenderHost's `onSuspend`: the worker drops every frame until the next epoch. */
export function postSenderSuspend(worker: Worker): void {
  const m: WorkerInMessage = { type: "sender-suspend" };
  worker.postMessage(m);
}

export function postReceiverEpoch(worker: Worker, k: EpochKey): void {
  const secret = k.secret.slice().buffer;
  const m: WorkerInMessage = { type: "receiver-epoch", epoch: k.epoch, secret };
  worker.postMessage(m, [secret]);
}
