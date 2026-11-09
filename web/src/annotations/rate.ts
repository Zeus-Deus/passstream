/** A token bucket: `rate` tokens per second, at most `burst` saved up. */
export class TokenBucket {
  private tokens: number;
  private at: number;
  constructor(
    readonly rate: number,
    readonly burst: number,
    private readonly now: () => number = () => performance.now(),
  ) {
    this.tokens = burst;
    this.at = now();
  }
  private refill() {
    const t = this.now();
    this.tokens = Math.min(this.burst, this.tokens + ((t - this.at) / 1000) * this.rate);
    this.at = t;
  }
  /** Take one token if there is one. */
  take(): boolean {
    this.refill();
    if (this.tokens < 1) return false;
    this.tokens -= 1;
    return true;
  }
  /** Milliseconds until a token is available (0 if one is). */
  wait(): number {
    this.refill();
    return this.tokens >= 1 ? 0 : Math.ceil(((1 - this.tokens) / this.rate) * 1000);
  }
}

/**
 * Viewer → host: 8 messages per second, burst 8. The relay allows a viewer
 * 16 per second (burst 32), shared with rekey acks and handshakes.
 */
export const VIEWER_SEND = { rate: 8, burst: 8 } as const;
/** Host accepts per admission: 10 per second, burst 20. The excess is dropped and counted. */
export const HOST_ACCEPT = { rate: 10, burst: 20 } as const;
/** Host fan-out: one batch per recipient per tick, ticks at least this far apart (≤ 8 per second per recipient). */
export const FANOUT_MS = 125;
/**
 * Host fan-out across all recipients: at most this many envelopes per second.
 * The server gives annotation envelopes their own budget (publisher 256/s),
 * apart from admission, rekey and ACK traffic; this stays well inside it.
 * With 100 viewers each gets one batch every 625 ms.
 */
export const FANOUT_RATE = 160;
/**
 * The policy is re-announced this often while viewers may draw, and for
 * POLICY_RESYNC_WINDOW_MS after any change, so a recipient whose batch the
 * relay dropped converges. An unused, host-only stream sends nothing.
 */
export const POLICY_RESYNC_MS = 5000;
export const POLICY_RESYNC_WINDOW_MS = 30_000;
