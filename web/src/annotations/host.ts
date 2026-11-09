import { ANNOTATION_HOST, ANNOTATION_MAX_BATCH, ANNOTATION_MAX_RECORDS, type AnnotationRecord } from "@passstream/e2ee";
import { decodeAnnotation, encodeAnnotation, hostOnly, MAX_POINTS, MAX_STATE_ORIGINS, MAX_SOURCES, MAX_SOURCE_PAGE, type AnnotationMsg } from "./codec";
import { FANOUT_MS, FANOUT_RATE, HOST_ACCEPT, POLICY_RESYNC_MS, POLICY_RESYNC_WINDOW_MS, TokenBucket } from "./rate";
import { AnnotationStore } from "./store";
import { ClearGenerations } from "./generations";

/** The part of SenderHost the router needs (a fake in tests). */
export interface HostLink {
  onAnnotation(f: (e: { admissionId: string; rowCode: string; body: Uint8Array }) => void): () => void;
  onAdmitted(f: (e: { admissionId: string }) => void): () => void;
  /** A kick was requested: these admissions may no longer annotate (synchronous, before the kick runs). */
  onRevoked(f: (e: { admissionIds: string[]; rowCode: string }) => void): () => void;
  /** May this admission annotate right now (SenderHost.annotationAllowed)? */
  allowed(admissionId: string): boolean;
  /** Connected admissions and their row codes. */
  recipients(): { admissionId: string; rowCode: string }[];
  sendAnnotations(admissionId: string, records: AnnotationRecord[], current?: () => boolean): Promise<boolean>;
}

/** Ink records waiting for fan-out at most. Control state is kept apart and never evicted. */
export const MAX_QUEUE = 256;
/** Per-origin clears carried in one batch at most (the rest wait a tick). */
const MAX_CLEARS_PER_BATCH = 32;

type Queued = { origin: string; source: string | null; msg: AnnotationMsg; body: Uint8Array };
type InputState = { admissionId: string; origin: string; source: bigint; session: bigint; seen: number; through: number };
/** One host-authenticated causal receipt: 17-byte body + 4-byte record header. */
const RECEIPT_BYTES = 21;
const BOOTSTRAP_BYTES = 29;

/**
 * The sender's annotation router (FEATURES.md "Track D2" §3): the policy
 * (host-only by default), per-admission rate limits, the host's own overlay,
 * and fan-out to every other viewer.
 *
 * Fan-out is bounded in aggregate: each tick sends exactly one sealed batch
 * per recipient, and ticks are spaced so the total stays within FANOUT_RATE
 * envelopes per second however many viewers watch (100 viewers: one batch
 * each every 625 ms). Lasers coalesce per origin; strokes wait their turn in
 * a bounded queue.
 *
 * Restrictive control state (policy off, clear-all, clearing a kicked or
 * resynchronised origin) is held apart, coalesced, sent first in every batch
 * and never evicted. Evicting ink clears that origin everywhere, so no
 * recipient keeps a half-delivered stroke.
 *
 * Clears are retained independently of ink in a fixed namespace table. Fair
 * partial state pages repair missed clears; every ink origin's current
 * generation also leads its ink, so a later repair never erases newer ink.
 * Repair runs while drawing/ink is active and for at least 30 seconds plus
 * enough pages to cover every origin twice after the last control change.
 */
export class HostAnnotations {
  /** Viewers may draw. Off by default and on every reload (restrictive). */
  viewersEnabled = false;
  readonly store = new AnnotationStore();
  readonly counters = {
    accepted: 0,
    dropped_rate: 0,
    dropped_policy: 0,
    dropped_revoked: 0,
    dropped_stale: 0,
    malformed: 0,
    dropped_overflow: 0,
    batches_sent: 0,
    ticks: 0,
  };
  private buckets = new Map<string, TokenBucket>();
  private queue: Queued[] = [];
  /** Pending control state, coalesced: the newest policy, clear-all, per-origin clears. */
  private control: { policy: boolean | null; clearAll: boolean; clearOrigins: Set<string>; state: boolean } = { policy: null, clearAll: false, clearOrigins: new Set(), state: false };
  /** Clear generations: clear-all, and per origin. */
  private clearGen = 0;
  private originGen = new ClearGenerations();
  private barriers = new ClearGenerations();
  /** Fixed admission slots; reuse advances the incarnation, so no tombstone journal grows. */
  readonly namespace: bigint;
  private inputs = new Map<string, InputState>();
  private sources: InputState[] = [];
  private sourceCursor = 0;
  /** Three wraps include two full visits even if a change occurs just behind the cursor. */
  private repairSweeps = 0;
  private sourceSweeps = 0;
  private lifetime = 0;
  /** When control state last changed (a clear, a policy): resync runs for a while after. */
  private controlChangedAt = -Infinity;
  private offs: (() => void)[] = [];
  private timer?: ReturnType<typeof setInterval>;
  private flushing = false;
  private nextAt = -Infinity;
  private lastPolicyAt = -Infinity;
  private policyChangedAt = -Infinity;
  private listeners = new Set<() => void>();

  constructor(
    private readonly link: HostLink,
    private readonly o: { now?: () => number; fanoutMs?: number; fanoutRate?: number; namespace?: bigint } = {},
  ) { this.namespace = o.namespace ?? (new DataView(crypto.getRandomValues(new Uint8Array(8)).buffer).getBigUint64(0) || 1n); }

  private admission(admissionId: string, origin: string): InputState | null {
    const old = this.inputs.get(admissionId);
    if (old) return old.origin === origin ? old : null;
    for (const [id] of this.inputs) if (!this.link.allowed(id)) { this.inputs.delete(id); this.buckets.delete(id); }
    let slot = this.sources.length;
    if (slot === MAX_SOURCES) slot = this.sources.findIndex(s => !this.inputs.has(s.admissionId));
    if (slot < 0) return null;
    const prior = this.sources[slot];
    const generation = prior ? (prior.source >> 16n) + 1n : 1n;
    if (generation > 0xffff_ffffn) return null;
    const input = { admissionId, origin, source: (generation << 16n) | BigInt(slot), session: 0n, seen: 0, through: 0 };
    if (prior) this.store.purgeSource(prior.source, 0xffff_ffff, prior.origin);
    this.sources[slot] = input;
    this.inputs.set(admissionId, input);
    this.control.state = true;
    this.repairSweeps = this.sourceSweeps = 3;
    return input;
  }

  private bootstrap(input: InputState): AnnotationRecord {
    return { origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "bootstrap", namespace: this.namespace, source: input.source, gen: this.clearGen, barrier: this.barriers.get(input.origin) }) };
  }

  private now() {
    return this.o.now?.() ?? performance.now();
  }
  private get rate() {
    return this.o.fanoutRate ?? FANOUT_RATE;
  }

  subscribe(f: () => void): () => void {
    this.listeners.add(f);
    return () => void this.listeners.delete(f);
  }
  private changed() {
    for (const f of this.listeners) f();
  }

  start() {
    this.offs.push(this.link.onAnnotation((e) => this.receive(e)));
    // Admissions join the existing paced flush; no unpaced policy/bootstrap sends.
    this.offs.push(this.link.onAdmitted(() => { this.control.policy = this.viewersEnabled; this.control.state = true; }));
    this.offs.push(this.link.onRevoked((e) => this.revoke(e)));
    this.timer = setInterval(() => void this.flush(), this.o.fanoutMs ?? FANOUT_MS);
  }
  stop() {
    this.lifetime++;
    this.offs.splice(0).forEach((f) => f());
    clearInterval(this.timer);
    this.queue = [];
  }

  /** Queued ink and control records. Diagnostics. */
  get pending() {
    return { ink: this.queue.length, policy: this.control.policy, clearAll: this.control.clearAll, clearOrigins: this.control.clearOrigins.size, state: this.control.state };
  }

  /** Turn viewer drawing on (local channels only) or off (anywhere). */
  setViewers(on: boolean) {
    this.viewersEnabled = on;
    if (!on) this.queue = this.queue.filter((q) => q.source === null); // queued viewer ink is no longer allowed
    this.control.policy = on;
    this.policyChangedAt = this.controlChangedAt = this.now();
    this.repairSweeps = this.sourceSweeps = 3;
    this.changed();
  }

  /** Clear everyone's strokes and lasers (allowed from anywhere). Queued ink before it is moot. */
  clear() {
    if (this.clearGen === 0xffff_ffff) throw new RangeError("clear generation exhausted");
    this.store.clear();
    this.queue = [];
    for (const input of this.sources) input.through = input.seen;
    this.clearGen++;
    this.control.clearAll = true;
    this.control.clearOrigins.clear();
    this.control.state = true;
    this.controlChangedAt = this.now();
    this.repairSweeps = this.sourceSweeps = 3;
    this.changed();
  }

  /** A kick was requested: drop the victim's pending ink and clear what it drew, here and everywhere. */
  revoke(e: { admissionIds: string[]; rowCode: string }) {
    const ids = new Set(e.admissionIds);
    const before = this.queue.length;
    this.queue = this.queue.filter((q) => !(q.source !== null && ids.has(q.source)));
    this.counters.dropped_revoked += before - this.queue.length;
    for (const id of ids) this.buckets.delete(id);
    this.clearOrigin(e.rowCode);
  }

  private clearOrigin(origin: string) {
    this.barriers.advance(origin);
    for (const input of this.sources) if (input.origin === origin) input.through = input.seen;
    this.store.apply(origin, { kind: "clear", scope: "own" }, this.now());
    this.queue = this.queue.filter((q) => q.origin !== origin);
    this.originGen.advance(origin);
    this.control.clearOrigins.add(origin);
    this.control.state = true;
    this.controlChangedAt = this.now();
    this.repairSweeps = this.sourceSweeps = 3;
    this.changed();
  }

  /**
   * The host's own drawing (the sender window): shown here and sent to every
   * viewer. `echoed` when the caller already drew it locally (InkInput's echo).
   */
  draw(msg: AnnotationMsg, echoed = false) {
    if (msg.kind === "policy") return;
    if (msg.kind === "clear") {
      if (msg.scope === "all") this.clear();
      else this.clearOrigin(ANNOTATION_HOST);
      return;
    }
    if (msg.kind !== "stroke" && msg.kind !== "laser") return;
    if (!echoed) this.store.apply(ANNOTATION_HOST, msg, this.now());
    this.enqueue({ origin: ANNOTATION_HOST, source: null, msg, body: encodeAnnotation(msg) });
  }

  /** An authenticated viewer annotation, from SenderHost's `annotation` event. */
  receive(e: { admissionId: string; rowCode: string; body: Uint8Array }) {
    // Rechecked after the decrypt: a kick requested meanwhile ends it here.
    if (!this.link.allowed(e.admissionId)) return void this.counters.dropped_revoked++;
    const envelope = decodeAnnotation(e.body);
    if (!envelope) return void this.counters.malformed++;
    if (!this.viewersEnabled || hostOnly(envelope)) return void this.counters.dropped_policy++;
    if (envelope.kind !== "input") return void this.counters.malformed++;
    let b = this.buckets.get(e.admissionId);
    if (!b) {
      for (const id of this.buckets.keys()) if (!this.link.allowed(id)) this.buckets.delete(id);
      this.buckets.set(e.admissionId, (b = new TokenBucket(HOST_ACCEPT.rate, HOST_ACCEPT.burst, () => this.now())));
    }
    if (!b.take()) return void this.counters.dropped_rate++;
    if (envelope.namespace !== this.namespace || envelope.gen !== this.clearGen || envelope.barrier !== this.barriers.get(e.rowCode)) {
      // Prompt bounded repair when a caller missed a host barrier. No retry or
      // pen-input hold: new input uses the barrier as soon as it is observed.
      this.control.state = true;
      this.counters.dropped_stale++;
      return;
    }
    const input = this.admission(e.admissionId, e.rowCode);
    if (!input || (input.session !== 0n && input.session !== envelope.session)) return void this.counters.dropped_stale++;
    input.session = envelope.session;
    // One current causal stream per authenticated admission, not per display row.
    if (envelope.op < input.seen) return void this.counters.dropped_stale++;
    this.counters.accepted++;
    if (envelope.clearThrough > input.through) {
      input.through = envelope.clearThrough;
      this.store.purgeSource(input.source, input.through, input.origin);
      this.queue = this.queue.filter(q => q.source !== e.admissionId || (q.msg.kind === "ink" && q.msg.op > input.through));
      this.control.state = true;
      this.controlChangedAt = this.now();
      this.repairSweeps = this.sourceSweeps = 3;
    }
    input.seen = envelope.op;
    const msg = envelope.msg;
    if (msg.kind === "clear" || envelope.op <= input.through) return;
    this.store.apply(e.rowCode, msg, this.now(), FANOUT_MS, envelope.op, input.source);
    const stamped: AnnotationMsg = { kind: "ink", source: input.source, op: envelope.op, msg };
    this.enqueue({ origin: e.rowCode, source: e.admissionId, msg: stamped, body: encodeAnnotation(stamped) });
  }

  private enqueue(q: Queued) {
    if (q.msg.kind === "laser" || (q.msg.kind === "ink" && q.msg.msg.kind === "laser")) {
      // Only the newest pointer position per admission matters: merge into a pending laser.
      const at = this.queue.findIndex((x) => x.source === q.source && x.origin === q.origin && (x.msg.kind === "laser" || (x.msg.kind === "ink" && x.msg.msg.kind === "laser")));
      if (at >= 0) {
        const previous = this.queue[at]!.msg;
        const prev = previous.kind === "ink" ? previous.msg : previous;
        const next = q.msg.kind === "ink" ? q.msg.msg : q.msg;
        if (prev.kind !== "laser" || next.kind !== "laser") return;
        const laser = { kind: "laser" as const, points: [...prev.points, ...next.points].slice(-MAX_POINTS) };
        const msg: AnnotationMsg = q.msg.kind === "ink" ? { ...q.msg, msg: laser } : laser;
        this.queue[at] = { ...q, msg, body: encodeAnnotation(msg) };
        return;
      }
    }
    if (this.queue.length >= MAX_QUEUE) {
      // Evict a laser first; otherwise the oldest stroke record, and resynchronise its origin.
      const laser = this.queue.findIndex((x) => (x.msg.kind === "laser" || (x.msg.kind === "ink" && x.msg.msg.kind === "laser")));
      const victim = this.queue.splice(laser >= 0 ? laser : 0, 1)[0]!;
      this.counters.dropped_overflow++;
      if (victim.msg.kind !== "laser" && !(victim.msg.kind === "ink" && victim.msg.msg.kind === "laser")) return this.clearOrigin(victim.origin);
    }
    this.queue.push(q);
  }

  /** Control records first (coalesced), then as much ink as fits one batch (64 records, 2048 bytes). */
  private takeBatch(): { control: AnnotationRecord[]; ink: Queued[]; resync: boolean; stateOnly: boolean } {
    const control: AnnotationRecord[] = [];
    const c = this.control;
    if (c.policy !== null) control.push({ origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "policy", viewers: c.policy }) });
    if (c.clearAll) control.push({ origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "clear", scope: "all", gen: this.clearGen }) });
    const clears = [...c.clearOrigins].slice(0, MAX_CLEARS_PER_BATCH);
    for (const o of clears) control.push({ origin: o, body: encodeAnnotation({ kind: "clear", scope: "own", gen: this.originGen.get(o) }) });
    const resync = c.state;
    const sourceEntries = this.sources.slice(this.sourceCursor, this.sourceCursor + MAX_SOURCE_PAGE / 2).map(s => ({ source: s.source, origin: s.origin, through: s.through }));
    this.sourceCursor += MAX_SOURCE_PAGE / 2;
    if (this.sourceCursor >= this.sources.length) { this.sourceCursor = 0; this.sourceSweeps = Math.max(0, this.sourceSweeps - 1); }
    control.push({ origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "sources", entries: sourceEntries }) });
    const otherControl = control.length > 1;
    // A rotating page always gets space before ink, even under continuous drawing.
    const page = this.originGen.page();
    if (page.wrapped) this.repairSweeps = Math.max(0, this.repairSweeps - 1);
    const state = (): AnnotationRecord => ({ origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "state", gen: this.clearGen, partial: true, origins: page.origins }) });
    control.unshift(state());
    c.policy = null;
    c.clearAll = false;
    c.state = false;
    for (const o of clears) c.clearOrigins.delete(o);
    // A recipient-specific receipt leads the shared records. Reserve its
    // largest size even for recipients with no input, preserving one batch.
    let size = 1 + BOOTSTRAP_BYTES + RECEIPT_BYTES + control.reduce((n, r) => n + 4 + r.body.length, 0);
    let count = 2 + control.length;
    const ink: Queued[] = [];
    const covered = new Set(page.origins.map((o) => o.origin));
    const generations: { origin: string; gen: number }[] = [];
    const coveredSources = new Set(sourceEntries.map(e => e.source));
    const extraSources: typeof sourceEntries = [];
    while (this.queue.length) {
      const q = this.queue[0]!;
      // A source kicked (or gone) since it was queued: never relayed.
      if (q.source !== null && !this.link.allowed(q.source)) {
        this.queue.shift();
        this.counters.dropped_revoked++;
        continue;
      }
      // Reserve the generation before its first ink record. This remains correct
      // when the repair cursor is far away, or this origin has no prior host ink.
      const needsGen = !covered.has(q.origin);
      const merge = needsGen && page.origins.length < MAX_STATE_ORIGINS;
      const extraRecord = needsGen && !merge && generations.length === 0 ? 1 : 0;
      const source = q.source === null ? undefined : this.inputs.get(q.source);
      const needsSource = source && !coveredSources.has(source.source);
      const mergeSource = sourceEntries.length < MAX_SOURCE_PAGE;
      if (needsSource && !mergeSource && extraSources.length === MAX_SOURCE_PAGE) break;
      const extraSourceRecord = needsSource && !mergeSource && extraSources.length === 0 ? 1 : 0;
      const n = (needsSource ? 14 + extraSourceRecord * 6 : 0) + 4 + q.body.length + (needsGen ? 6 : 0) + extraRecord * 11;
      if (count + 1 + extraRecord + extraSourceRecord > ANNOTATION_MAX_RECORDS || size + n > ANNOTATION_MAX_BATCH) break;
      this.queue.shift();
      if (needsGen) {
        covered.add(q.origin);
        (merge ? page.origins : generations).push({ origin: q.origin, gen: this.originGen.get(q.origin) });
      }
      if (needsSource) {
        coveredSources.add(source.source);
        (mergeSource ? sourceEntries : extraSources).push({ source: source.source, origin: source.origin, through: source.through });
      }
      ink.push(q);
      size += n;
      count += 1 + extraRecord + extraSourceRecord;
    }
    const sourceRecord = control.find(r => decodeAnnotation(r.body)?.kind === "sources")!;
    sourceRecord.body = encodeAnnotation({ kind: "sources", entries: sourceEntries });
    if (extraSources.length) control.push({ origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "sources", entries: extraSources }) });
    control[0] = state();
    if (generations.length) control.splice(1, 0, { origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "state", gen: this.clearGen, partial: true, origins: generations }) });
    return { control, ink, resync, stateOnly: !otherControl };
  }

  /**
   * One fan-out tick: exactly one batch per connected recipient (its own ink
   * filtered out). Ticks are spaced at least FANOUT_MS and at least
   * recipients / FANOUT_RATE apart. The policy is re-announced every
   * POLICY_RESYNC_MS, so a recipient that lost a batch converges.
   */
  async flush(): Promise<void> {
    const now = this.now();
    if (this.flushing || now < this.nextAt) return;
    // Resync (policy and overlay state) while viewers may draw, while the host has ink,
    // or until the finite repair window AND at least two full generation visits finish.
    const resync = this.viewersEnabled || this.store.origins.size > 0 || this.repairSweeps > 0 || this.sourceSweeps > 0 || now - Math.max(this.policyChangedAt, this.controlChangedAt) < POLICY_RESYNC_WINDOW_MS;
    if (resync && now - this.lastPolicyAt >= POLICY_RESYNC_MS) {
      if (this.control.policy === null) this.control.policy = this.viewersEnabled;
      this.control.state = true;
    }
    // Finish the retained-state sweeps at the normal bounded fan-out cadence.
    if (this.repairSweeps > 0 || this.sourceSweeps > 0) this.control.state = true;
    const c = this.control;
    if (this.queue.length === 0 && c.policy === null && !c.clearAll && c.clearOrigins.size === 0 && !c.state) return;
    const recipients = this.link.recipients().filter((r) => this.link.allowed(r.admissionId));
    if (recipients.length === 0) {
      this.queue = [];
      this.control = { policy: null, clearAll: false, clearOrigins: new Set(), state: false };
      return;
    }
    for (const r of recipients) this.admission(r.admissionId, r.rowCode);
    this.flushing = true;
    this.counters.ticks++;
    if (c.policy !== null) this.lastPolicyAt = now;
    this.nextAt = now + Math.max(this.o.fanoutMs ?? FANOUT_MS, (recipients.length * 1000) / this.rate);
    try {
      const lifetime = this.lifetime;
      const { control, ink, resync, stateOnly } = this.takeBatch();
      await Promise.all(
        recipients.map(async (r) => {
          const own = ink.filter((q) => q.source !== r.admissionId).map((q) => ({ origin: q.origin, body: q.body }));
          // A state record alone (the drawer, whose ink is filtered out) goes only on resync ticks.
          const records = own.length || !stateOnly || resync ? [...control, ...own] : [];
          if (records.length === 0) return;
          const input = this.inputs.get(r.admissionId);
          const barrier = this.barriers.get(r.rowCode);
          if (!input || !this.link.allowed(r.admissionId)) return;
          records.unshift(this.bootstrap(input));
          if (input.session !== 0n || barrier) records.splice(1, 0, { origin: ANNOTATION_HOST, body: encodeAnnotation({ kind: "receipt", session: input.session, through: input.through, barrier }) });
          if (await this.link.sendAnnotations(r.admissionId, records, () => lifetime === this.lifetime).catch(() => false)) this.counters.batches_sent++;
        }),
      );
    } finally {
      this.flushing = false;
    }
  }
}
