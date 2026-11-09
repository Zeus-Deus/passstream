/**
 * Encrypt/decrypt WebRTC encoded frames (RTCEncodedVideoFrame /
 * RTCEncodedAudioFrame) in place. Fail closed: any error drops the frame;
 * a frame is never forwarded unencrypted (sender) or undecrypted (receiver).
 *
 * Protected frame layout:
 *   clear_prefix || SFrame(header || AEAD(rest, aad = header || clear_prefix))
 * Only VP9, VP8 and audio are accepted (see codec.ts).
 */
import { concat } from "./bytes.ts";
import { type Codec, codecFromMime, frameLayout } from "./codec.ts";
import { E2EEError, type E2EEErrorCode, isE2EEError } from "./errors.ts";
import { type ReceiverKeyRing, type SenderKeyRing, splitKid } from "./keyring.ts";
import { decodeHeader } from "./sframe.ts";

/** The subset of RTCEncodedVideoFrame / RTCEncodedAudioFrame we use. */
export interface EncodedFrameLike {
  data: ArrayBuffer;
  readonly type?: "key" | "delta" | "empty";
  getMetadata?(): { mimeType?: string };
}

/**
 * `key_changed`: the sender ring was suspended or rekeyed while the frame was being encrypted.
 * `cutoff`: a revocation's cutoff passed without a fresh epoch (worker-enforced).
 */
export type DropReason = E2EEErrorCode | "empty" | "no_codec" | "key_changed" | "cutoff" | "fence";

export interface CryptorStats {
  ok: number;
  dropped: Partial<Record<DropReason, number>>;
}

interface CryptorOptions {
  track: number;
  tracks?: readonly number[];
  kind: "video" | "audio";
  /** Fallback when the frame metadata carries no mimeType (e.g. "video/vp9"). */
  codec?: string;
}

function resolveCodec(o: CryptorOptions, frame: EncodedFrameLike): Codec | null {
  if (o.kind === "audio") return "audio";
  let mime: string | undefined;
  try {
    mime = frame.getMetadata?.().mimeType;
  } catch {
    mime = undefined;
  }
  return codecFromMime(mime) ?? codecFromMime(o.codec);
}

abstract class Cryptor {
  readonly stats: CryptorStats = { ok: 0, dropped: {} };
  protected readonly o: CryptorOptions;
  /** Called on every drop, for worker → main-thread reporting. */
  onDrop: ((reason: DropReason, err?: E2EEError) => void) | null = null;

  constructor(o: CryptorOptions) {
    this.o = o;
  }

  protected drop(reason: DropReason, err?: E2EEError): false {
    this.stats.dropped[reason] = (this.stats.dropped[reason] ?? 0) + 1;
    this.onDrop?.(reason, err);
    return false;
  }

  protected fail(e: unknown): false {
    if (isE2EEError(e)) return this.drop(e.code, e);
    return this.drop("malformed", new E2EEError("malformed", String(e)));
  }
}

export class FrameEncryptor extends Cryptor {
  readonly #ring: SenderKeyRing;

  constructor(ring: SenderKeyRing, o: CryptorOptions) {
    super(o);
    this.#ring = ring;
  }

  /** Encrypts `frame.data` in place. Returns false if the frame must be dropped. */
  async encrypt(frame: EncodedFrameLike): Promise<boolean> {
    try {
      const data = new Uint8Array(frame.data);
      if (data.length === 0) return this.drop("empty");
      const codec = resolveCodec(this.o, frame);
      if (!codec) return this.drop("no_codec");
      const { clear } = frameLayout(codec, data);
      const prefix = data.subarray(0, clear);
      // Rechecked after every await: a suspend() or a new epoch while WebCrypto
      // ran means this frame's key may be one a kicked viewer holds.
      const version = this.#ring.version;
      // Hot path: the key is already derived, no await before the AEAD.
      const key = this.#ring.readyKey(this.o.track) ?? (await this.#ring.key(this.o.track));
      if (this.#ring.version !== version) return this.drop("key_changed");
      const sf = await key.encrypt(data.subarray(clear), prefix);
      if (this.#ring.version !== version) return this.drop("key_changed");
      // VP9 and audio have no clear prefix: the SFrame buffer is the frame (no second copy).
      frame.data = clear === 0 ? sf.buffer : concat(prefix, sf).buffer;
      this.stats.ok++;
      return true;
    } catch (e) {
      return this.fail(e);
    }
  }
}

export class FrameDecryptor extends Cryptor {
  readonly #ring: ReceiverKeyRing;
  /** Newest epoch for which a frame arrived before its key (keyframe request hook). */
  missingEpoch: number | null = null;
  /** Track of the latest authenticated frame, for receiver liveness reports. */
  decryptedTrack: number | null = null;

  constructor(ring: ReceiverKeyRing, o: CryptorOptions) {
    super(o);
    this.#ring = ring;
  }

  /** Decrypts `frame.data` in place. Returns false if the frame must be dropped. */
  async decrypt(frame: EncodedFrameLike): Promise<boolean> {
    try {
      const data = new Uint8Array(frame.data);
      if (data.length === 0) return this.drop("empty");
      const codec = resolveCodec(this.o, frame);
      if (!codec) return this.drop("no_codec");
      const { clear } = frameLayout(codec, data);
      const prefix = data.subarray(0, clear);
      const sf = data.subarray(clear);
      const header = decodeHeader(sf);
      const { track } = splitKid(header.kid);
      if (!(this.o.tracks ?? [this.o.track]).includes(track)) return this.drop("wrong_track");
      let k = this.#ring.readyKey(header.kid);
      if (!k) try {
        k = await this.#ring.key(header.kid);
      } catch (e) {
        if (isE2EEError(e, "unknown_key")) {
          const { epoch } = splitKid(header.kid);
          const newest = this.#ring.newest();
          if (newest === null || epoch > newest) this.missingEpoch = Math.max(this.missingEpoch ?? 0, epoch);
        }
        throw e;
      }
      if (!k.window.check(header.ctr)) return this.drop("replay");
      const pt = await k.key.decrypt(sf, prefix, header);
      // Another decrypt of the same CTR may have finished while we awaited the
      // AEAD: recheck and commit synchronously, before any plaintext leaves.
      if (!k.window.check(header.ctr)) return this.drop("replay");
      k.window.commit(header.ctr);
      this.#ring.noteDecrypted(k.epoch);
      this.decryptedTrack = track;
      // WebCrypto's output is a fresh buffer: with no clear prefix it is the frame.
      frame.data = clear === 0 && pt.byteOffset === 0 && pt.byteLength === pt.buffer.byteLength ? pt.buffer : concat(prefix, pt).buffer;
      this.stats.ok++;
      return true;
    } catch (e) {
      return this.fail(e);
    }
  }
}
