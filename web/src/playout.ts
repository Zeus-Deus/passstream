// Playout delay trades latency for continuity. Reordering can produce NACKs
// without loss, so NACKs alone keep the low target. Repeated repair freezes
// raise it with RTT; measured TWCC loss plus active NACK repair raises it to
// an RTT-scaled target before the first long freeze. Quiet paths return to the low target.
export class PlayoutPolicy {
  static readonly low = 50;
  static readonly quietNack = 30_000;
  static readonly quietFreeze = 120_000;
  static readonly repairWindow = 5_000;
  static readonly freezeWindow = 30_000;
  target = PlayoutPolicy.low;
  private nacks = 0;
  private freezes = 0;
  private lastNack = -Infinity;
  private lastFreeze = -Infinity;
  private raised = false;
  private repairRTT?: number;
  private lastRTT = -Infinity;
 private lossSeen=-Infinity;
  noteLoss(now: number) { this.lossSeen = now; }
  observe(nackCount: number, freezeCount: number, rttMs: number, now: number, rawLoss=false): number {
 if(rawLoss)this.noteLoss(now);
    if (nackCount > this.nacks) this.lastNack = now;
    this.nacks = nackCount;
    if (freezeCount > this.freezes) {
      const repeated = freezeCount - this.freezes > 1 || now - this.lastFreeze < PlayoutPolicy.freezeWindow;
      if (repeated && now - this.lastNack < PlayoutPolicy.repairWindow) this.raised = true;
      this.lastFreeze = now;
    }
    this.freezes = freezeCount;
    if(now-this.lossSeen<5000 && now-this.lastNack<PlayoutPolicy.repairWindow)this.raised=true;
    if ((now - this.lastNack >= PlayoutPolicy.quietNack || now - this.lastFreeze >= PlayoutPolicy.quietFreeze) && now-this.lossSeen>=PlayoutPolicy.quietNack)
      this.raised = false;
    // A brief low RTT sample must not drain the buffer before late repair.
    if (this.raised && rttMs > 0) {
      this.repairRTT = this.repairRTT === undefined || now - this.lastRTT > 5000
        ? rttMs : this.repairRTT * .9 + rttMs * .1;
      this.lastRTT = now;
    } else if (!this.raised) this.repairRTT = undefined;
    // The low target already covers local scheduling; fast 30ms repair
    // retries need RTT headroom plus a short scheduling allowance.
    const desired = this.raised
      ? Math.min(300, Math.max(150, 65 + 1.5 * (this.repairRTT ?? rttMs)))
      : PlayoutPolicy.low;
    // Filling the entire repair window at once can itself create a freeze.
    // Stats arrive every 500 ms; spread increases across advancing pictures.
    this.target = Math.min(desired, this.target + 100);
    return this.target;
  }
}

export function lossEvidence(repair:unknown):boolean {
 if(typeof repair!=="object" || repair===null || !("estimate" in repair))return false;
 const estimate=repair.estimate;
 return typeof estimate==="object" && estimate!==null && "averageLoss" in estimate && typeof estimate.averageLoss==="number" && estimate.averageLoss>.01;
}
