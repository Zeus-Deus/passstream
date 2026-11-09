// Decoder progress is independent of the captured pixels. WebRTC timestamps
// wrap at 32 bits; receiveTime and the callback clock are monotonic milliseconds.
export class VideoFreshness {
  count = 0;
  private presented = 0;
  private timestamp?: number;
  observe(now: number, metadata: Pick<VideoFrameCallbackMetadata, "presentedFrames" | "rtpTimestamp" | "receiveTime" | "captureTime">) {
    const { presentedFrames, rtpTimestamp, receiveTime } = metadata;
    const delta = rtpTimestamp === undefined || this.timestamp === undefined
      ? 1 : (rtpTimestamp - this.timestamp) >>> 0;
    const advancing = presentedFrames > this.presented && delta > 0 && delta < 0x80000000;
    const presentedDelta = this.presented === 0 ? 1 : presentedFrames - this.presented;
    this.presented = presentedFrames;
    this.timestamp = rtpTimestamp;
    if (advancing && receiveTime !== undefined && now - receiveTime >= -100 && now - receiveTime <= 1000)
      this.count += presentedDelta <= 240 ? presentedDelta : 1;
    return this.count;
  }
}

// SCTP setup can lag behind working media on a lossy path. Replacing that
// transport discards healthy video and starts another lossy handshake.
export function heartbeatStartupExpired(now: number, started: number, lastDecoded?: number, connected: number | null = started) {
  // ICE/DTLS can consume the initial deadline under heavy loss. Keep a bounded
  // connection attempt, then give newly connected media its own SCTP window.
  const since = connected ?? started, timeout = connected === null ? 45_000 : 15_000;
  return now - since > timeout && (lastDecoded === undefined || now - lastDecoded > 1_000);
}

// Replacing a PC cannot accelerate a keyframe already inside its wire budget.
// Keep this grace bounded even if repeated room keyframes keep the queue busy.
export class CatchupGrace {
  private until?: number;
  defer(now: number, repair: unknown): boolean {
    const active = typeof repair === "object" && repair !== null && "catchupMs" in repair
      && typeof repair.catchupMs === "number" && repair.catchupMs > 0;
    if (!active) { this.until = undefined; return false; }
    this.until ??= now + 6000;
    return now < this.until;
  }
}

// An advancing decoder survives ordinary FIFO residence. The optional harness
// clock still detects grossly stale moving pictures without turning a brief
// one-second queue excursion into another expensive PC handshake.
export function decoderLiveness(advancing: boolean, pixelDelay?: number, freshnessLimit = 1000) {
  return advancing && (pixelDelay === undefined || pixelDelay <= Math.max(3000, 2 * freshnessLimit));
}
