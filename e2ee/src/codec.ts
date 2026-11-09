/**
 * What stays in the clear in an encrypted WebRTC encoded frame, per codec.
 *
 * The RTP packetizer and depacketizer run *after* the sender transform and
 * *before* the receiver transform, and some of them parse the frame bytes.
 * PassStream negotiates VP9 only. The standalone frame helpers also retain
 * VP8 support for vectors and interoperability tests:
 *
 * - VP9: the RTP payload descriptor is built from encoder metadata, not by
 *   parsing the bitstream, so nothing is left clear.
 * - VP8: the packetizer/depacketizer read the uncompressed data chunk at the
 *   start of the frame (RFC 6386 §9.1): 3-byte frame tag (key/inter flag,
 *   version, show_frame, first partition size) and, on keyframes, the 7-byte
 *   start code + width/height. We leave 10 bytes clear on keyframes and 3 on
 *   delta frames (the keyframe bit is bit 0 of byte 0, so the receiver derives
 *   the same length from the clear bytes).
 * - H.264: **refused** (fail closed). libwebrtc parses first_mb_in_slice,
 *   slice_type and pps_id from the slice header before the receive transform
 *   runs, and may rewrite SPS on the send path, so no frame-level clear prefix
 *   is both sufficient and reliably preserved.
 * - AV1: **refused**. The packetizer parses OBU headers and sizes and drops
 *   some OBU types, so a frame-level SFrame ciphertext breaks packetization.
 * - Audio (Opus and others): nothing is left clear.
 *
 * The clear prefix is authenticated: it is the SFrame `metadata`, so it goes
 * into the AAD (RFC 9605 §4.4.3).
 */
import { E2EEError } from "./errors.ts";

export type Codec = "vp8" | "vp9" | "h264" | "av1" | "audio";

/** Video codecs usable under E2EE, in preference order (for setCodecPreferences). */
export const E2EE_VIDEO_CODECS = ["video/VP9"] as const;

export function codecFromMime(mime: string | undefined): Codec | null {
  if (!mime) return null;
  const m = mime.toLowerCase();
  if (m.startsWith("audio/")) return "audio";
  if (m === "video/vp8") return "vp8";
  if (m === "video/vp9") return "vp9";
  if (m === "video/h264") return "h264";
  if (m === "video/av1") return "av1";
  return null;
}

export interface Layout {
  /** Bytes at the start of the frame left unencrypted (and authenticated). */
  clear: number;
}

const VP8_KEY = 10;
const VP8_DELTA = 3;

/**
 * Layout for a frame. On the sender `data` is the plaintext frame; on the
 * receiver it is the protected frame, and the result is the same because only
 * clear bytes are inspected.
 */
export function frameLayout(codec: Codec, data: Uint8Array): Layout {
  switch (codec) {
    case "vp8": {
      if (data.length === 0) throw new E2EEError("malformed", "empty VP8 frame");
      const key = ((data[0] ?? 0) & 0x01) === 0;
      return { clear: Math.min(data.length, key ? VP8_KEY : VP8_DELTA) };
    }
    case "vp9":
    case "audio":
      return { clear: 0 };
    case "h264":
      throw new E2EEError("unsupported_codec", "H.264 is not supported under E2EE; negotiate VP9");
    case "av1":
      throw new E2EEError("unsupported_codec", "AV1 is not supported under E2EE; negotiate VP9");
  }
}
