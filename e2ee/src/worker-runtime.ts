/**
 * The logic behind transform-worker.ts, written against a structural worker
 * scope so it runs (and is tested) in Node as well as in a dedicated worker.
 *
 * Main thread → worker messages (postMessage):
 *   { type: "sender-epoch",   epoch, secret }  switch encryption to this epoch, request a keyframe
 *   { type: "sender-suspend" }                 stop encrypting (drop every frame) until the next sender-epoch
 *   { type: "sender-cutoff", at, within, stale } a revocation began: by min(at, receipt + within)
 *                                               (monotonic ms, see monotonicNow) the epoch must be
 *                                               newer than `stale`, or the worker suspends by itself
 *   { type: "receiver-epoch", epoch, secret }  add a decryption epoch
 *   { type: "receiver-drop-epoch", epoch }     forget an epoch now (kick of self, teardown)
 *   { type: "sender-fence", at, ms }           the privacy curtain went down at `at` (monotonic ms): drop
 *                                               every frame reaching an encrypt transform until at + ms,
 *                                               whenever it was captured; then pass video only from a
 *                                               keyframe (requested), so nothing encoded before the
 *                                               curtain leaves after it
 *   { type: "sender-fence-open" }              the outgoing track now is the curtain picture: end the
 *                                               fence now (its `ms` is only the upper bound)
 *   { type: "keyframe" }                      ask every video encoder for a keyframe
 *   { type: "trace", on }                     record per-frame crypto timings (diagnostics, off by default)
 *   { type: "trace-dump" }                    reply { type: "trace", frames } and clear
 * Worker → main thread:
 *   { type: "drop", role, track, reason, count } throttled per (track, reason)
 *   { type: "decrypted", track, count }        receiver: frames decrypted, at most every `okReportMs` (200) per track
 *   { type: "fence", at }                      sender: the curtain fence was armed here at `at` (monotonic ms);
 *                                               frames already handed on before this can't be recalled
 *   { type: "error", message }                 bad options or messages; affected transform drops all frames
 */
import { E2EEError } from "./errors.ts";
import { type DropReason, type EncodedFrameLike, FrameDecryptor, FrameEncryptor } from "./frame-cryptor.ts";
import { ReceiverKeyRing, type ReceiverKeyRingOptions, SenderKeyRing, TRACKS_PER_EPOCH } from "./keyring.ts";

export interface TransformOptions {
  role: "encrypt" | "decrypt";
  kind: "video" | "audio";
  /** 0..15, unique per sending track; the receiver checks it. */
  track: number;
  /** Decrypt-only allowlist when one receiver switches independent encodings. */
  tracks?: readonly number[];
  /** Fallback codec mime type when frame metadata lacks one, e.g. "video/vp9". */
  codec?: string;
}

export interface ScriptTransformerLike {
  readonly readable: ReadableStream<EncodedFrameLike>;
  readonly writable: WritableStream<EncodedFrameLike>;
  readonly options?: unknown;
  generateKeyFrame?(rid?: string): Promise<unknown>;
  sendKeyFrameRequest?(): Promise<unknown>;
}

export interface WorkerScopeLike {
  onrtctransform: ((ev: { transformer: ScriptTransformerLike }) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  postMessage(msg: unknown): void;
}

export type WorkerInMessage =
  | { type: "sender-epoch"; epoch: number; secret: ArrayBuffer | Uint8Array }
  | { type: "sender-suspend" }
  | { type: "sender-cutoff"; at: number; within: number; stale: number }
  | { type: "receiver-epoch"; epoch: number; secret: ArrayBuffer | Uint8Array }
  | { type: "receiver-drop-epoch"; epoch: number }
  | { type: "sender-fence"; at: number; ms: number }
  | { type: "sender-fence-open" }
  | { type: "keyframe" }
  | { type: "trace"; on: boolean }
  | { type: "trace-dump" };

export type WorkerOutMessage =
  | { type: "drop"; role: "encrypt" | "decrypt"; track: number; reason: DropReason; count: number }
  | { type: "decrypted"; track: number; count: number }
  | { type: "fence"; at: number }
  | { type: "trace"; frames: TraceRow[] }
  | { type: "error"; message: string };

/**
 * One traced frame: [monotonic start ms, role (0 encrypt, 1 decrypt), track,
 * queue residence ms (receiver: now − the frame's receiveTime; null if the
 * browser doesn't report it), crypto ms, size in, size out, keyframe,
 * true or the drop reason].
 */
export type TraceRow = [number, 0 | 1, number, number | null, number, number, number, boolean, true | string];
const TRACE_MAX = 20_000;

export function parseTransformOptions(o: unknown): TransformOptions {
  if (typeof o !== "object" || o === null) throw new E2EEError("invalid_argument", "transform options missing");
  const { role, kind, track, tracks, codec } = o as Record<string, unknown>;
  if (role !== "encrypt" && role !== "decrypt") throw new E2EEError("invalid_argument", "role must be encrypt|decrypt");
  if (kind !== "video" && kind !== "audio") throw new E2EEError("invalid_argument", "kind must be video|audio");
  if (typeof track !== "number" || !Number.isInteger(track) || track < 0 || track >= TRACKS_PER_EPOCH) {
    throw new E2EEError("invalid_argument", "track must be 0..15");
  }
  if (tracks !== undefined && (role !== "decrypt" || kind !== "video" || !Array.isArray(tracks) || tracks.length === 0 || tracks.length > TRACKS_PER_EPOCH || new Set(tracks).size !== tracks.length || !tracks.includes(track) || tracks.some(t => typeof t !== "number" || !Number.isInteger(t) || t < 0 || t >= TRACKS_PER_EPOCH))) {
    throw new E2EEError("invalid_argument", "tracks must be a video decrypt allowlist containing track");
  }
  if (codec !== undefined && typeof codec !== "string") throw new E2EEError("invalid_argument", "codec must be a string");
  return { role, kind, track, ...(tracks ? { tracks: [...tracks] } : {}), ...(codec ? { codec } : {}) };
}

function secretBytes(s: unknown): Uint8Array {
  if (s instanceof Uint8Array && s.length === 32) return s;
  if (s instanceof ArrayBuffer && s.byteLength === 32) return new Uint8Array(s);
  throw new E2EEError("invalid_argument", "epoch secret must be 32 bytes");
}

function epochNumber(e: unknown): number {
  if (typeof e !== "number" || !Number.isInteger(e) || e < 0 || e > 0xffff_ffff) throw new E2EEError("invalid_argument", "bad epoch");
  return e;
}

interface Active {
  opts: TransformOptions;
  transformer: ScriptTransformerLike;
  decryptor?: FrameDecryptor;
}

/**
 * performance.timeOrigin + performance.now(): monotonic within a context and
 * comparable across the page and its workers (same time origin base).
 */
/** While a video transform waits for its post-fence keyframe, it re-requests one this often. */
export const FENCE_KEY_RETRY_MS = 250;

export function monotonicNow(): number {
  return performance.timeOrigin + performance.now();
}

export interface TransformWorker {
  senderRing: SenderKeyRing;
  receiverRing: ReceiverKeyRing;
  /** Pipes still running (tests); a finished pipe is removed. */
  piped: Promise<void>[];
  /** Transforms still attached; a finished pipe's entry is removed (PC replacement). */
  active: readonly Active[];
  /** The armed revocation cutoff, null when none is open. */
  readonly cutoff: { at: number; stale: number } | null;
  /** The newest curtain fence (it stays set; frames are dropped only before `until`). */
  readonly fence: { until: number; epoch: number } | null;
}

export function installTransformWorker(
  scope: WorkerScopeLike,
  o: { receiver?: ReceiverKeyRingOptions; dropReportMs?: number; okReportMs?: number; keyframeRequestMs?: number; now?: () => number; monotonic?: () => number } = {},
): TransformWorker {
  const now = o.now ?? Date.now;
  const mono = o.monotonic ?? monotonicNow;
  /**
   * Revocation cutoff, enforced here and not only by the main thread's
   * SenderKeyManager.tick(): a stalled UI thread must not let frames under an
   * epoch a kicked viewer holds leave after the cutoff. Checked before and
   * after every encryption; cleared by an epoch newer than `stale`.
   */
  let cutoff: { at: number; stale: number } | null = null;
  /** True when the cutoff (not the main thread) stopped encryption. */
  let cutoffHit = false;
  const cutoffPassed = (epoch: number | null) => cutoff !== null && mono() >= cutoff.at && (epoch === null || epoch <= cutoff.stale);
  const enforceCutoff = (): boolean => {
    if (!cutoffPassed(senderRing.epoch)) return true;
    if (senderRing.epoch !== null) senderRing.suspend();
    cutoffHit = true;
    return false;
  };
  /**
   * Curtain fence: frames reaching an encrypt transform before `until` are
   * dropped (they may have been captured before the curtain and sat in the
   * encoder); afterwards each video transform waits for a keyframe.
   */
  let fence: { until: number; epoch: number } | null = null;
  let fenceEpoch = 0;
  const senderRing = new SenderKeyRing();
  const receiverRing = new ReceiverKeyRing(o.receiver ?? {});
  const active: Active[] = [];
  const piped: Promise<void>[] = [];
  const dropReportMs = o.dropReportMs ?? 1000;
  const lastReport = new Map<string, { at: number; count: number }>();
  let lastKeyframeRequest = -Infinity;

  const post = (m: WorkerOutMessage) => scope.postMessage(m);
  let trace: TraceRow[] | null = null;
  let lastDrop = "";
  const traced = async (role: 0 | 1, track: number, frame: EncodedFrameLike, run: () => Promise<boolean>): Promise<boolean> => {
    if (!trace) return run();
    const t0 = mono();
    let residence: number | null = null;
    try {
      const rt = (frame.getMetadata?.() as { receiveTime?: unknown } | undefined)?.receiveTime;
      if (typeof rt === "number" && rt > 0) residence = performance.now() - rt;
    } catch {
      /* no metadata */
    }
    const size = frame.data.byteLength;
    lastDrop = "dropped";
    const ok = await run();
    if (trace.length < TRACE_MAX) trace.push([t0, role, track, residence, mono() - t0, size, frame.data.byteLength, frame.type === "key", ok || lastDrop]);
    return ok;
  };

  const reportDrop = (role: "encrypt" | "decrypt", track: number, reason: DropReason) => {
    const k = `${role}:${track}:${reason}`;
    lastDrop = reason;
    const r = lastReport.get(k) ?? { at: -Infinity, count: 0 };
    r.count++;
    if (now() - r.at >= dropReportMs) {
      post({ type: "drop", role, track, reason, count: r.count });
      r.at = now();
      r.count = 0;
    }
    lastReport.set(k, r);
  };

  // Liveness of each received track (the viewer shows "Tap to unmute" only while audio decrypts).
  const okReports = new Map<number, { at: number; count: number }>();
  const reportOk = (track: number) => {
    const r = okReports.get(track) ?? { at: -Infinity, count: 0 };
    r.count++;
    if (now() - r.at >= (o.okReportMs ?? 200)) {
      post({ type: "decrypted", track, count: r.count });
      r.at = now();
      r.count = 0;
    }
    okReports.set(track, r);
  };

  const requestKeyframes = () => {
    for (const a of active) {
      if (a.opts.role === "encrypt" && a.opts.kind === "video") a.transformer.generateKeyFrame?.().catch(() => undefined);
    }
  };

  const maybeRequestFromSender = () => {
    // A receiver saw frames of an epoch before its key: once the key arrives,
    // ask for a keyframe so decoding restarts immediately (rate-limited).
    const newest = receiverRing.newest();
    for (const a of active) {
      const d = a.decryptor;
      if (!d || d.missingEpoch === null || newest === null || d.missingEpoch > newest) continue;
      d.missingEpoch = null;
      if (a.opts.kind === "video" && now() - lastKeyframeRequest >= (o.keyframeRequestMs ?? 1000)) {
        lastKeyframeRequest = now();
        a.transformer.sendKeyFrameRequest?.().catch(() => undefined);
      }
    }
  };

  scope.onmessage = (ev) => {
    try {
      const m = ev.data as Partial<WorkerInMessage> | null;
      switch (m?.type) {
        case "sender-epoch": {
          const epoch = epochNumber(m.epoch);
          // Past the cutoff, an epoch a revoked viewer may hold is never installed.
          if (cutoff && epoch <= cutoff.stale && cutoffHit) throw new E2EEError("state", "stale epoch after a revocation cutoff");
          senderRing.setEpoch({ epoch, secret: Uint8Array.from(secretBytes(m.secret)) });
          if (cutoff && epoch > cutoff.stale) {
            cutoff = null;
            cutoffHit = false;
          }
          requestKeyframes();
          break;
        }
        case "sender-cutoff": {
          const at = Number(m.at), within = Number(m.within), stale = epochNumber(m.stale);
          if (!Number.isFinite(at) || !Number.isFinite(within)) throw new E2EEError("invalid_argument", "bad cutoff");
          // The earlier of the sender's absolute time and receipt + within: never later than either.
          const deadline = Math.min(at, mono() + Math.max(0, within));
          cutoff = { at: Math.min(cutoff?.at ?? Infinity, deadline), stale: Math.max(cutoff?.stale ?? 0, stale) };
          if (senderRing.epoch !== null && senderRing.epoch > cutoff.stale) {
            cutoff = null;
            cutoffHit = false;
          }
          break;
        }
        case "sender-suspend":
          senderRing.suspend();
          break;
        case "receiver-epoch":
          receiverRing.addEpoch({ epoch: epochNumber(m.epoch), secret: Uint8Array.from(secretBytes(m.secret)) });
          maybeRequestFromSender();
          break;
        case "receiver-drop-epoch":
          receiverRing.dropEpoch(epochNumber(m.epoch));
          break;
        case "sender-fence": {
          const at = Number(m.at), ms = Number(m.ms);
          if (!Number.isFinite(at) || !Number.isFinite(ms) || ms < 0 || ms > 5000) throw new E2EEError("invalid_argument", "bad fence");
          fence = { until: Math.max(fence?.until ?? 0, Math.min(at, mono()) + ms), epoch: ++fenceEpoch };
          post({ type: "fence", at: mono() });
          break;
        }
        case "sender-fence-open":
          if (fence) fence = { ...fence, until: Math.min(fence.until, mono()) };
          break;
        case "keyframe":
          requestKeyframes();
          break;
        case "trace":
          trace = m.on ? (trace ?? []) : null;
          break;
        case "trace-dump":
          post({ type: "trace", frames: trace ?? [] });
          if (trace) trace = [];
          break;
        default:
          throw new E2EEError("invalid_argument", "unknown worker message");
      }
    } catch (e) {
      post({ type: "error", message: String(e) });
    }
  };

  scope.onrtctransform = (ev) => {
    const t = ev.transformer;
    let stream: TransformStream<EncodedFrameLike, EncodedFrameLike>;
    let entry: Active | undefined;
    try {
      const opts = parseTransformOptions(t.options);
      const a: Active = { opts, transformer: t };
      if (opts.role === "encrypt") {
        senderRing.claimTrack(opts.track);
        const enc = new FrameEncryptor(senderRing, opts);
        enc.onDrop = (r) => reportDrop("encrypt", opts.track, r);
        /** The fence this video transform still owes a keyframe for (0: none). */
        let awaitingKey = 0;
        let keyAskedAt = -Infinity;
        stream = new TransformStream({
          async transform(frame, c) {
            // The fence generation this frame started under: a curtain armed while
            // WebCrypto runs must still stop it (checked again after the await).
            const fenceGen = fence?.epoch ?? 0;
            if (fence && mono() < fence.until) {
              if (opts.kind === "video") awaitingKey = fence.epoch;
              return reportDrop("encrypt", opts.track, "fence");
            }
            if (awaitingKey) {
              // Past the fence: decoders resume from a fresh keyframe, never a delta on a dropped frame.
              if (frame.type !== "key") {
                // Ask, and ask again every 250 ms: a request made while the encoder
                // switches tracks can be lost, and viewers wait on this keyframe.
                if (mono() - keyAskedAt >= FENCE_KEY_RETRY_MS) {
                  keyAskedAt = mono();
                  t.generateKeyFrame?.().catch(() => undefined);
                }
                return reportDrop("encrypt", opts.track, "fence");
              }
              keyAskedAt = -Infinity;
              awaitingKey = 0;
            }
            if (!enforceCutoff()) return reportDrop("encrypt", opts.track, "cutoff");
            const epoch = senderRing.epoch;
            if (!(await traced(0, opts.track, frame, () => enc.encrypt(frame)))) return;
            // The cutoff may have passed while WebCrypto ran.
            if (cutoffPassed(epoch)) {
              enforceCutoff();
              return reportDrop("encrypt", opts.track, "cutoff");
            }
            // So may a curtain: a fence armed during the encryption drops this frame,
            // even if it has opened again since, and video then waits for a keyframe.
            if (fence && (fence.epoch !== fenceGen || mono() < fence.until)) {
              if (opts.kind === "video") awaitingKey = fence.epoch;
              return reportDrop("encrypt", opts.track, "fence");
            }
            c.enqueue(frame);
          },
        });
      } else {
        const dec = new FrameDecryptor(receiverRing, opts);
        dec.onDrop = (r) => reportDrop("decrypt", opts.track, r);
        a.decryptor = dec;
        stream = new TransformStream({
          async transform(frame, c) {
            if (await traced(1, opts.track, frame, () => dec.decrypt(frame))) {
              reportOk(dec.decryptedTrack!);
              c.enqueue(frame);
            }
          },
        });
      }
      active.push(a);
      entry = a;
    } catch (e) {
      post({ type: "error", message: String(e) });
      // Fail closed: consume and discard everything.
      stream = new TransformStream({ transform() {} });
    }
    // A replaced PC ends its pipes: forget them, so a long session's
    // replacements don't accumulate transformers and decryptors.
    const pipe: Promise<void> = t.readable
      .pipeThrough(stream)
      .pipeTo(t.writable)
      .catch(() => undefined)
      .finally(() => {
        const i = piped.indexOf(pipe);
        if (i >= 0) piped.splice(i, 1);
        const j = entry ? active.indexOf(entry) : -1;
        if (j >= 0) active.splice(j, 1);
      });
    piped.push(pipe);
  };

  return {
    senderRing,
    receiverRing,
    piped,
    active,
    get cutoff() {
      return cutoff;
    },
    get fence() {
      return fence;
    },
  };
}
