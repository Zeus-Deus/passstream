import { MAX_POINTS, type AnnotationMsg, type Point } from "./codec";
import { FANOUT_MS } from "./rate";

export type Tool = "laser" | "pen";

/**
 * Turns pointer input (normalised points) into annotation messages: echoed
 * locally at once, and emitted at most once per `intervalMs` (default 125 ms,
 * 8 per second) with every point gathered since. `canSend()` lets the caller
 * hold a batch back (its rate limit); points keep gathering meanwhile.
 */
export class InkInput {
  tool: Tool = "laser";
  private pending: Point[] = [];
  private strokeId = Math.floor(Math.random() * 0x10000);
  private drawing = false;
  private ending = false;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private lastEmit = -Infinity;

  constructor(
    private readonly o: {
      echo: (m: AnnotationMsg) => void;
      emit: (m: AnnotationMsg) => void;
      canSend?: () => number;
      intervalMs?: number;
      now?: () => number;
      setTimer?: (f: () => void, ms: number) => ReturnType<typeof setTimeout>;
      clearTimer?: (t: ReturnType<typeof setTimeout>) => void;
    },
  ) {}

  private get interval() {
    return this.o.intervalMs ?? FANOUT_MS;
  }
  private now() {
    return this.o.now?.() ?? performance.now();
  }

  /** Switch tools; anything gathered for the old one goes out first. */
  setTool(t: Tool) {
    if (t === this.tool) return;
    if (this.drawing) this.end();
    this.flush(true);
    this.tool = t;
  }

  begin(p: Point) {
    if (this.drawing) this.end();
    // The previous stroke's tail goes out before a new stroke id is used.
    if (this.pending.length || this.ending) this.flush(true);
    this.drawing = true;
    this.ending = false;
    this.strokeId = (this.strokeId + 1) & 0xffff;
    this.add(p);
  }
  move(p: Point) {
    if (this.drawing) this.add(p);
  }
  end() {
    if (!this.drawing) return;
    this.drawing = false;
    if (this.tool === "pen") {
      this.ending = true;
      this.o.echo({ kind: "stroke", id: this.strokeId, end: true, points: [] });
    }
    this.schedule();
  }
  get active() {
    return this.drawing;
  }

  private add(p: Point) {
    const q = { x: Math.min(1, Math.max(0, p.x)), y: Math.min(1, Math.max(0, p.y)) };
    this.pending.push(q);
    this.o.echo(this.tool === "laser" ? { kind: "laser", points: [q] } : { kind: "stroke", id: this.strokeId, end: false, points: [q] });
    this.schedule();
  }

  private schedule() {
    if (this.timer) return;
    const wait = Math.max(0, this.lastEmit + this.interval - this.now(), this.o.canSend?.() ?? 0);
    const set = this.o.setTimer ?? ((f, ms) => setTimeout(f, ms));
    this.timer = set(() => {
      this.timer = null;
      this.flush();
    }, wait);
  }

  /**
   * Emit what's gathered (one message), if allowed now; otherwise reschedule.
   * `force` (a new stroke or tool while the last one's tail is unsent) skips
   * the interval and the caller's hold: at most one extra message.
   */
  flush(force = false) {
    if (this.pending.length === 0 && !this.ending) return;
    const hold = force ? 0 : (this.o.canSend?.() ?? 0);
    if (hold > 0) return this.schedule();
    this.lastEmit = this.now();
    if (this.tool === "laser") {
      // Only where the pointer is now matters: the newest points.
      this.o.emit({ kind: "laser", points: this.pending.slice(-MAX_POINTS) });
      this.pending = [];
    } else {
      const points = this.pending.splice(0, MAX_POINTS);
      const end = this.ending && this.pending.length === 0;
      this.o.emit({ kind: "stroke", id: this.strokeId, end, points });
      if (end) this.ending = false;
    }
    if (force) {
      // Whatever is left of the old stroke (over 64 points) is dropped, not mixed into the next.
      this.pending = [];
      this.ending = false;
    }
    if (this.pending.length || this.ending) this.schedule();
  }

  dispose() {
    if (this.timer) (this.o.clearTimer ?? clearTimeout)(this.timer);
    this.timer = null;
    this.pending = [];
    this.drawing = false;
    this.ending = false;
  }
}
