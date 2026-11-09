import type { AnnotationMsg, Point } from "./codec";

/** A laser point fades out over this long. */
export const LASER_FADE_MS = 1200;
/** Strokes kept per origin; the oldest goes first. */
export const MAX_STROKES = 32;
export const MAX_STROKE_POINTS = 1024;

export interface Stroke {
  id: number;
  points: Point[];
  done: boolean;
  at: number;
  /** Local input operation, for reconciling a host purge without replaying ink. */
  inputOp?: number;
  source?: bigint;
}
export interface TrailPoint extends Point {
  at: number;
  inputOp?: number;
  source?: bigint;
}
export interface OriginInk {
  strokes: Map<number | string, Stroke>;
  trail: TrailPoint[];
}

/**
 * Everything drawn on one overlay, by origin (a row code or "host"). Bounded
 * per origin. Remote laser batches are spread over `spreadMs` so the dot
 * glides between batches instead of jumping.
 */
export class AnnotationStore {
  readonly origins = new Map<string, OriginInk>();
  version = 0;
  private listeners = new Set<() => void>();

  subscribe(f: () => void): () => void {
    this.listeners.add(f);
    return () => void this.listeners.delete(f);
  }
  private changed() {
    this.version++;
    for (const f of this.listeners) f();
  }
  private ink(origin: string): OriginInk {
    let o = this.origins.get(origin);
    if (!o) this.origins.set(origin, (o = { strokes: new Map(), trail: [] }));
    return o;
  }

  /** Apply a drawing message. Policy messages aren't drawing: false. */
  apply(origin: string, m: AnnotationMsg, now: number, spreadMs = 0, inputOp?: number, source?: bigint): boolean {
    switch (m.kind) {
      case "laser": {
        const o = this.ink(origin);
        const n = m.points.length;
        m.points.forEach((p, i) => o.trail.push({ ...p, at: now + (spreadMs * i) / n, inputOp, source }));
        if (o.trail.length > 256) o.trail.splice(0, o.trail.length - 256);
        break;
      }
      case "stroke": {
        const o = this.ink(origin);
        const key = source === undefined ? m.id : `${source}/${m.id}`;
        let s = o.strokes.get(key);
        if (!s || s.inputOp !== inputOp) {
          s = { id: m.id, points: [], done: false, at: now, inputOp, source };
          o.strokes.set(key, s);
          while (o.strokes.size > MAX_STROKES) {
            const oldest = [...o.strokes.values()].reduce((a, b) => (b.at < a.at ? b : a));
            o.strokes.delete(oldest.source === undefined ? oldest.id : `${oldest.source}/${oldest.id}`);
          }
        }
        if (s.done) return false;
        for (const p of m.points) if (s.points.length < MAX_STROKE_POINTS) s.points.push(p);
        if (m.end) s.done = true;
        break;
      }
      case "clear":
        if (m.scope === "all") this.origins.clear();
        else this.origins.delete(origin); // its strokes and its laser
        break;
      case "policy":
      case "state":
      case "input":
      case "receipt":
      case "bootstrap":
      case "ink":
      case "sources":
        return false;
    }
    this.changed();
    return true;
  }

  clear() {
    this.origins.clear();
    this.changed();
  }

  /** A causal receipt covers whole pen operations, including their pending tails. */
  purgeInput(origin: string, through: number, source?: bigint) {
    const ink = this.origins.get(origin);
    if (!ink) return;
    const before = ink.strokes.size + ink.trail.length;
    for (const [id, stroke] of ink.strokes) if (stroke.source === source && (stroke.inputOp ?? 0) <= through) ink.strokes.delete(id);
    ink.trail = ink.trail.filter((p) => p.source !== source || (p.inputOp ?? 0) > through);
    if (!ink.strokes.size && !ink.trail.length) this.origins.delete(origin);
    if (before !== ink.strokes.size + ink.trail.length) this.changed();
  }

  /** Incarnation repair is independent of the shared display row and stroke ID. */
  purgeSource(source: bigint, through = 0xffff_ffff, origin?: string) {
    if (origin !== undefined) return this.purgeInput(origin, through, source);
    for (const origin of [...this.origins.keys()]) this.purgeInput(origin, through, source);
  }

  /** Remote row clears preserve only this admission's causally reconciled local ink. */
  clearRemote(origin: string, local: bigint) {
    const ink = this.origins.get(origin);
    if (!ink) return;
    for (const [key, s] of ink.strokes) if (s.source !== local) ink.strokes.delete(key);
    ink.trail = ink.trail.filter(p => p.source === local);
    if (!ink.strokes.size && !ink.trail.length) this.origins.delete(origin);
    this.changed();
  }

  /** Drop faded laser points. True while something still animates. */
  prune(now: number): boolean {
    let animating = false;
    for (const [origin, o] of this.origins) {
      const before = o.trail.length;
      o.trail = o.trail.filter((p) => now - p.at < LASER_FADE_MS);
      if (o.trail.length) animating = true;
      if (o.trail.length !== before && o.trail.length === 0 && o.strokes.size === 0) this.origins.delete(origin);
    }
    return animating;
  }
}
