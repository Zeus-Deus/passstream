import { randomBytes } from "../src/bytes.ts";
import type { EncodedFrameLike } from "../src/frame-cryptor.ts";

export class FakeFrame implements EncodedFrameLike {
  data: ArrayBuffer;
  readonly type: "key" | "delta" | undefined;
  readonly #mime: string | undefined;
  constructor(data: Uint8Array, mime?: string, type?: "key" | "delta") {
    this.data = Uint8Array.from(data).buffer;
    this.#mime = mime;
    this.type = type;
  }
  getMetadata() {
    return this.#mime ? { mimeType: this.#mime } : {};
  }
  bytes(): Uint8Array {
    return new Uint8Array(this.data);
  }
  clone(): FakeFrame {
    return new FakeFrame(this.bytes(), this.#mime, this.type);
  }
}

/** VP8 frame: byte 0 bit 0 = 0 for keyframes. */
export function vp8Frame(key: boolean, size = 2000): Uint8Array {
  const d = randomBytes(size);
  d[0] = key ? (d[0]! & 0xfe) : d[0]! | 0x01;
  return d;
}
