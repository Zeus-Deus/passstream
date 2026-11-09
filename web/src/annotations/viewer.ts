import { ANNOTATION_HOST, type AnnotationRecord } from "@passstream/e2ee";
import { decodeAnnotation, encodeAnnotation, hostOnly, MAX_SOURCES, sourceSlot, type InkMsg, type InputMsg } from "./codec";
import { InkInput, type Tool } from "./input";
import { FANOUT_MS, TokenBucket, VIEWER_SEND } from "./rate";
import { AnnotationStore } from "./store";
import { ClearGenerations } from "./generations";

/** Viewer messages waiting for a send token at most (about 1 s at 8/s). */
export const OUTBOX_MAX = 8;

/** What the viewer side needs from the admitted session (a fake in tests). */
export interface ViewerLink {
  /** Seal one body under the newest epoch (ViewerSession.sealAnnotation); throws when not admitted. */
  seal(body: Uint8Array): Promise<Uint8Array>;
  send(bytes: Uint8Array): void;
  rowCode(): string | null;
}

/**
 * The viewer's annotation state (FEATURES.md "Track D2" §3): the host's
 * policy, everyone's ink, and this viewer's own drawing, rate-limited to
 * 8 messages per second.
 */
export class ViewerAnnotations {
  /** The host lets viewers draw. Unknown (false) until the host says so. */
  policy = false;
  readonly store = new AnnotationStore();
  readonly counters = { sent: 0, received: 0, malformed: 0, ignored: 0, send_failed: 0, dropped_outbox: 0 };
  readonly input: InkInput;
  /** The newest host clear generations applied: clear-all, and per origin. */
  private clearGen = 0;
  private namespace: bigint | null = null;
  private source = 0n;
  private adopting = true;
  private sources: ({ source: bigint; origin: string; through: number } | undefined)[] = Array(MAX_SOURCES);
  private originGen = new ClearGenerations();
  private barriers = new ClearGenerations();
  private readonly inputSession = new DataView(crypto.getRandomValues(new Uint8Array(8)).buffer).getBigUint64(0) || 1n;
  private operation = 0;
  private clearThrough = 0;
  private purgedThrough = 0;
  private stroke: { id: number; op: number } | null = null;
  private lifetime = 0;
  private bucket: TokenBucket;
  private listeners = new Set<() => void>();
  /**
   * Messages waiting for a token, at most OUTBOX_MAX. The bucket is enforced
   * here, at the final send: nothing reaches the relay faster than 8/s, so
   * the viewer's own rekey ACKs keep their share of its 16/s relay budget.
   */
  private outbox: InputMsg[] = [];
  private pumping = false;
  private pumpTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly link: ViewerLink,
    private readonly o: { now?: () => number } = {},
  ) {
    this.bucket = new TokenBucket(VIEWER_SEND.rate, VIEWER_SEND.burst, () => this.now());
    this.input = new InkInput({
      echo: (m) => {
        if (m.kind !== "stroke" && m.kind !== "laser") return;
        if (m.kind === "laser") this.nextOperation();
        else if (this.stroke?.id !== m.id) this.stroke = { id: m.id, op: this.nextOperation() };
        const me = this.link.rowCode();
        if (me) this.store.apply(me, m, this.now(), 0, m.kind === "stroke" ? this.stroke!.op : this.operation, this.source);
      },
      emit: (m) => { if (m.kind === "stroke" || m.kind === "laser") this.send(m); },
      canSend: () => this.bucket.wait(),
      now: () => this.now(),
    });
  }

  private now() {
    return this.o.now?.() ?? performance.now();
  }
  subscribe(f: () => void): () => void {
    this.listeners.add(f);
    return () => void this.listeners.delete(f);
  }
  private changed() {
    for (const f of this.listeners) f();
  }

  setTool(t: Tool) {
    this.input.setTool(t);
    this.changed();
  }
  get tool(): Tool {
    return this.input.tool;
  }

  /** Clear this viewer's own strokes, here and for everyone. */
  clearOwn() {
    this.input.dispose();
    this.stroke = null;
    this.clearThrough = this.nextOperation();
    const me = this.link.rowCode();
    if (me) this.store.purgeSource(this.source);
    this.send({ kind: "clear", scope: "own" });
  }

  private nextOperation(): number {
    if (this.operation === 0xffff_ffff) throw new RangeError("annotation input exhausted");
    return ++this.operation;
  }

  private send(m: InkMsg | { kind: "clear"; scope: "own" }) {
    if (!this.policy || this.namespace === null || this.adopting) return;
    if (m.kind === "clear") {
      // Everything of ours still queued is moot once our ink is cleared.
      this.outbox = [];
    } else if (m.kind === "laser") {
      // Only the newest pointer position matters: replace a queued laser.
      const at = this.outbox.findIndex((x) => x.msg.kind === "laser");
      if (at >= 0) this.outbox.splice(at, 1);
    }
    if (this.outbox.length >= OUTBOX_MAX) {
      // Bounded: drop a laser, else the oldest stroke segment, and resynchronise
      // by clearing our ink (here and, once sent, everywhere).
      const laser = this.outbox.findIndex((x) => x.msg.kind === "laser");
      if (laser >= 0) this.outbox.splice(laser, 1);
      else {
        this.outbox = [];
        this.input.dispose();
        this.stroke = null;
        this.clearThrough = this.nextOperation();
        const me = this.link.rowCode();
        if (me) this.store.purgeSource(this.source);
        m = { kind: "clear", scope: "own" };
      }
      this.counters.dropped_outbox++;
    }
    const op = m.kind === "stroke" ? this.stroke!.op : this.operation;
    const me = this.link.rowCode();
    this.outbox.push({ kind: "input", namespace: this.namespace, session: this.inputSession, op, clearThrough: this.clearThrough, gen: this.clearGen, barrier: me ? this.barriers.get(me) : 0, msg: m });
    this.pump();
  }

  /** Send queued messages in order, one token each, never more than one seal in flight. */
  private pump() {
    if (this.pumping || this.pumpTimer || this.outbox.length === 0) return;
    if (!this.policy) {
      this.outbox = [];
      return;
    }
    const wait = this.bucket.wait();
    if (wait > 0) {
      this.pumpTimer = setTimeout(() => {
        this.pumpTimer = null;
        this.pump();
      }, wait);
      return;
    }
    if (!this.bucket.take()) return;
    const m = this.outbox.shift()!;
    const lifetime = this.lifetime;
    this.pumping = true;
    void this.link
      .seal(encodeAnnotation(m))
      .then((bytes) => {
        if (!this.policy || this.adopting || lifetime !== this.lifetime) return;
        const me = this.link.rowCode();
        if (m.gen !== this.clearGen || (me && m.barrier !== this.barriers.get(me)) || m.op <= this.purgedThrough) return;
        this.link.send(bytes);
        this.counters.sent++;
      })
      .catch(() => void this.counters.send_failed++)
      .finally(() => {
        this.pumping = false;
        this.pump();
      });
  }

  /** Invalidate pending seals immediately when the authenticated channel changes. */
  beginAdmission() {
    this.lifetime++;
    this.policy = false;
    this.adopting = true;
    this.input.dispose();
    this.stroke = null;
    this.outbox = [];
    this.changed();
  }

  /** Downstream records, already authenticated and origin-stamped by the host. */
  receive(records: AnnotationRecord[]) {
    const me = this.link.rowCode();
    const first = records[0];
    const boot = first?.origin === ANNOTATION_HOST ? decodeAnnotation(first.body) : null;
    // Only the current authenticated channel may deliver a bootstrap. Once bound,
    // another namespace cannot reset counters until a fresh admission completes.
    if (boot?.kind !== "bootstrap" || (!this.adopting && boot.namespace !== this.namespace)) return;
    if (this.adopting) {
      if (this.namespace !== boot.namespace) {
        this.store.clear();
        this.clearGen = 0;
        this.originGen = new ClearGenerations();
        this.barriers = new ClearGenerations();
        this.sources = Array(MAX_SOURCES);
      }
      this.namespace = boot.namespace;
      this.source = boot.source;
      this.purgedThrough = 0;
      this.adopting = false;
    }
    if (boot.source !== this.source) return;
    if (boot.gen > this.clearGen) { this.clearGen = boot.gen; this.authoritativeClear(); }
    if (me && this.barriers.observe(me, boot.barrier)) this.authoritativeClear(me);
    for (const r of records) {
      const m = decodeAnnotation(r.body);
      if (!m) {
        this.counters.malformed++;
        continue;
      }
      this.counters.received++;
      if (hostOnly(m) && r.origin !== ANNOTATION_HOST) {
        this.counters.ignored++;
        continue;
      }
      if (m.kind === "bootstrap") continue;
      if (m.kind === "input") {
        this.counters.ignored++;
        continue;
      }
      if (m.kind === "sources") {
        for (const e of m.entries) {
          const slot = sourceSlot(e.source), old = this.sources[slot];
          if (old && old.source > e.source) continue;
          if (old && old.source !== e.source) this.store.purgeSource(old.source, 0xffff_ffff, old.origin);
          const through = old?.source === e.source ? Math.max(old.through, e.through) : e.through;
          this.sources[slot] = { source: e.source, origin: e.origin, through };
          this.store.purgeSource(e.source, through, e.origin);
          if (e.source === this.source) this.purgeLocal(through);
        }
        continue;
      }
      if (m.kind === "ink") {
        const state = this.sources[sourceSlot(m.source)];
        if (m.source === this.source || state?.source !== m.source || m.op <= state.through) continue;
        this.store.apply(r.origin, m.msg, this.now(), FANOUT_MS, m.op, m.source);
        continue;
      }
      if (m.kind === "receipt") {
        if (me && this.barriers.observe(me, m.barrier)) this.authoritativeClear(me);
        if (m.session !== this.inputSession || m.through <= this.purgedThrough) continue;
        this.purgeLocal(m.through);
        continue;
      }
      if (m.kind === "policy") {
        this.policy = m.viewers;
        if (!m.viewers) {
          this.lifetime++;
          this.input.dispose();
          this.outbox = [];
        }
        continue;
      }
      if (m.kind === "state") {
        this.converge(m, me);
        continue;
      }
      if (m.kind === "clear") {
        // Versioned: only a clear newer than what we applied counts, so a stale
        // or duplicate one never erases newer ink. Our sequenced local ink uses
        // the host's receipt/barrier, which identifies the actual purge boundary.
        const gen = m.gen ?? 0;
        if (m.scope === "all") {
          if (gen <= this.clearGen) continue;
          this.clearGen = gen;
          this.authoritativeClear();
        } else {
          if (!this.originGen.observe(r.origin, gen)) continue;
          this.clearOrigin(r.origin, me);
        }
        continue;
      }
      // Bare downstream primitives are reserved for the host; viewer ink carries a source.
      if (r.origin !== ANNOTATION_HOST) continue;
      this.store.apply(r.origin, m, this.now(), FANOUT_MS);
    }
    this.changed();
  }

  private purgeLocal(through: number) {
    if (through <= this.purgedThrough) return;
    this.purgedThrough = through;
    this.store.purgeSource(this.source, through);
    this.outbox = this.outbox.filter(x => x.op > through);
    if (this.stroke && this.stroke.op <= through) { this.input.dispose(); this.stroke = null; }
  }

  /** Forced host barriers win over all older optimistic ink, even unsent ink. */
  private authoritativeClear(origin?: string) {
    this.lifetime++;
    this.input.dispose();
    this.stroke = null;
    this.outbox = [];
    if (origin) this.store.apply(origin, { kind: "clear", scope: "own" }, this.now());
    else this.store.clear();
  }

  private clearOrigin(origin: string, me: string | null) {
    if (origin === me) { this.store.clearRemote(origin, this.source); this.store.purgeSource(this.source, this.purgedThrough); }
    else this.store.apply(origin, { kind: "clear", scope: "own" }, this.now());
  }

  /**
   * Apply authoritative remote generations; our input uses receipts/barriers.
   * The host sends partial repair pages: absence never implies a clear. Legacy
   * complete lists can still remove remote ink, but exempt our pending local ink.
   */
  private converge(m: { gen: number; partial: boolean; origins: { origin: string; gen: number }[] }, me: string | null) {
    if (m.gen > this.clearGen) {
      this.clearGen = m.gen;
      this.authoritativeClear();
    }
    const listed = new Set<string>();
    for (const o of m.origins) {
      listed.add(o.origin);
      if (this.originGen.observe(o.origin, o.gen)) {
        this.clearOrigin(o.origin, me);
      }
    }
    if (m.partial) return;
    for (const origin of [...this.store.origins.keys()]) {
      if (origin !== me && !listed.has(origin)) this.store.apply(origin, { kind: "clear", scope: "own" }, this.now());
    }
  }

  dispose() {
    this.lifetime++;
    this.input.dispose();
    this.outbox = [];
    if (this.pumpTimer) clearTimeout(this.pumpTimer);
    this.pumpTimer = null;
    this.store.clear();
    this.policy = false;
    this.changed();
  }
}
