// A lost application heartbeat alone must not tear down a receiving transport.
//
// Recovery replaces the PC instead of restarting ICE. Measured with Chrome
// (controlling) and Pion (controlled): after an ICE restart Chrome keeps the
// dead selected pair until its receiving timeout (about 3.6 s after a UDP
// blackhole, whenever the restart was sent), and Pion drops media on the old
// pair as soon as a delayed restart arrives, even when that pair has already
// resumed (8.4 s stalls after a total outage). A new PC selects the first
// working pair in about 200 ms.
//
// - Media stalls while signaling answers pings throughout: the network works
//   but the media path does not. Replace at once.
// - Signaling stalls too: the outage is total. Wait; the old pair usually
//   resumes by itself. If media is still stalled reconnectGrace after
//   signaling returns, replace.
// - Without progress for 6 s, replace regardless.
export class RecoveryPolicy {
  static readonly stall = 1000;
  static readonly reconnectGrace = 1000;
  lastAlive = 0;
  suspect = 0;
  /** Start of the current run of responsive signaling, or Infinity if unresponsive. */
  signalingSince = Infinity;
  lastPong = -Infinity;
  private counters = new Map<string, number>();
  private videoAt: number | null = null;
  private sourceFrames = 0;
  private sourceAt = -Infinity;
  private receivedVideoAt = -Infinity;
  private replaceAfter = -Infinity;
  private lastReplacement = -Infinity;
  private replacementBackoff = 6000;
  private healthySince: number | null = null;
  replaced(now: number) {
    this.replacementBackoff = now-this.lastReplacement<30000 ? Math.min(30000,this.replacementBackoff*2) : 6000;
    this.lastReplacement=now;this.replaceAfter=now+this.replacementBackoff;this.healthySince=null;
  }
  source(frames: number, now: number) {
    if (!Number.isFinite(frames)) return;
    if (frames > this.sourceFrames) { this.sourceAt = now; this.sourceFrames = frames; }
  }
  deferVideo(now: number) { if (this.videoAt !== null) this.videoAt = now; }
  video(now: number, fresh = true) {
    if (this.videoAt === null || fresh) this.videoAt = now;
    if (fresh) {
      this.alive(now);
      this.healthySince ??= now;
      if (now-this.healthySince>=3000) {this.replaceAfter=-Infinity;this.lastReplacement=-Infinity;this.replacementBackoff=6000}
    } else this.healthySince=null;
  }
  reset(now: number) {
    this.lastAlive = now; this.suspect = 0; this.counters.clear();
 this.videoAt = null; this.sourceFrames = 0; this.sourceAt = -Infinity; this.receivedVideoAt = -Infinity; this.healthySince=null;
  }
  alive(now: number) { this.lastAlive = now; }
  pong(now: number) {
    if (this.signalingSince === Infinity || now - this.lastPong > RecoveryPolicy.stall) this.signalingSince = now;
    this.lastPong = now;
  }
  signalingLost() { this.signalingSince = Infinity; }
  observe(stats: RTCStatsReport, now: number) {
    const pair=selectedPair(stats);
    for (const s of stats.values()) {
      // Incoming video proves activity even while SCTP source counters are
      // delayed. Require a delta: a late first snapshot cannot prove activity
      // on an idle screen. Consent and audio do not prove video progress.
      if (s.type === "inbound-rtp" && (s.kind === "video" || s.mediaType === "video") && typeof s.packetsReceived === "number") {
        const key = `${s.id}:video`, before = this.counters.get(key);
        if (before !== undefined && s.packetsReceived > before) this.receivedVideoAt = now;
        this.counters.set(key, s.packetsReceived);
      }
      const n = s.type === "transport" || s.type === "inbound-rtp" ? s.bytesReceived
        : s.type === "candidate-pair" && s.id===pair?.id ? s.responsesReceived : undefined;
      if (typeof n !== "number") continue;
      const before = this.counters.get(s.id) ?? 0;
      if (n > before) this.alive(now);
      this.counters.set(s.id, n);
    }
  }
  action(now: number, rtt: number, signaling: boolean, baselineRTT = rtt): "none" | "replace" {
    const videoStall = Math.max(RecoveryPolicy.stall, 4 * baselineRTT);
    // Queue-inflated RTT must not postpone recovery of a stopped decoder.
    // Only the pre-media baseline sets its physical-delay allowance.
    const decodedAt = this.videoAt !== null && Math.max(this.sourceAt,this.receivedVideoAt)-this.videoAt >= videoStall ? this.videoAt : undefined;
    const stall = decodedAt === undefined ? Math.max(RecoveryPolicy.stall,4*rtt) : videoStall;
    const alive = decodedAt === undefined ? this.lastAlive : Math.min(this.lastAlive,decodedAt);
    if (now - alive <= stall) { this.suspect = 0; return "none"; }
    if (now<this.replaceAfter) return "none";
    this.suspect ||= now;
    if (now - this.suspect >= 6000) return "replace";
    if (!signaling || now - this.lastPong > RecoveryPolicy.stall) return "none";
    if (this.signalingSince <= alive) return "replace";
    return now - this.signalingSince >= RecoveryPolicy.reconnectGrace ? "replace" : "none";
  }
}
export function selectedPair(stats: RTCStatsReport): RTCStats | undefined {
  for (const s of stats.values()) if (s.type === "transport" && s.selectedCandidatePairId)
    return stats.get(s.selectedCandidatePairId);
  return undefined;
}

// A genuinely high-RTT path needs its physical delay allowance. Congestion
// added after admission must not make stale moving pictures count as fresh.
export function transportBaselineRTT(repair: unknown): number | undefined {
 if (typeof repair !== "object" || repair === null || !("initialRTTMs" in repair)) return undefined;
 const rtt = repair.initialRTTMs;
 return typeof rtt === "number" && Number.isFinite(rtt) && rtt >= 0 ? rtt : undefined;
}
export function videoFreshnessLimit(repair: unknown): number {
 return Math.max(1000,(transportBaselineRTT(repair) ?? 0)+500);
}
