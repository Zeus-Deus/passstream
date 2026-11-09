import { fixtureBatch, payloadRecords } from "./test-input";
import { viewerBody } from "./test-input";
import { describe, expect, it } from "vitest";
import type { AnnotationRecord } from "@passstream/e2ee";
import { decodeAnnotation, encodeAnnotation, hostOnly, MAX_BODY, MAX_POINTS, type AnnotationMsg } from "./codec";
import { MAX_QUEUE, type HostLink } from "./host";
import { HostAnnotations } from "./test-input";
import { InkInput } from "./input";
import { HOST_ACCEPT, TokenBucket, VIEWER_SEND } from "./rate";
import { AnnotationStore, LASER_FADE_MS, MAX_STROKE_POINTS, MAX_STROKES } from "./store";
import { ViewerAnnotations } from "./viewer";

const pts = (n: number) => Array.from({ length: n }, (_, i) => ({ x: i / 100, y: 1 - i / 100 }));

describe("annotation codec", () => {
  it("round-trips every kind within 1/65535", () => {
    const msgs: AnnotationMsg[] = [
      { kind: "laser", points: pts(3) },
      { kind: "stroke", id: 513, end: true, points: pts(MAX_POINTS) },
      { kind: "stroke", id: 0, end: false, points: [] },
      { kind: "clear", scope: "all" },
      { kind: "clear", scope: "own" },
      { kind: "policy", viewers: true },
      { kind: "policy", viewers: false },
    ];
    for (const m of msgs) {
      const b = encodeAnnotation(m);
      expect(b.length).toBeLessThanOrEqual(MAX_BODY);
      const d = decodeAnnotation(b)!;
      expect(d.kind).toBe(m.kind);
      if ("points" in m && "points" in d) d.points.forEach((p, i) => (expect(Math.abs(p.x - m.points[i]!.x)).toBeLessThan(2e-5), expect(Math.abs(p.y - m.points[i]!.y)).toBeLessThan(2e-5)));
      else expect(d).toEqual(m.kind === "clear" ? { ...m, gen: 0 } : m);
    }
  });

  it("clamps coordinates and bounds points", () => {
    const d = decodeAnnotation(encodeAnnotation({ kind: "laser", points: [{ x: -3, y: 9 }, { x: Number.NaN, y: 0.5 }] }));
    expect(d).toEqual({ kind: "laser", points: [{ x: 0, y: 1 }, { x: 0, y: 32768 / 65535 }] });
    expect(() => encodeAnnotation({ kind: "laser", points: pts(MAX_POINTS + 1) })).toThrow();
    expect(() => encodeAnnotation({ kind: "laser", points: [] })).toThrow();
    expect(() => encodeAnnotation({ kind: "stroke", id: 70000, end: false, points: [] })).toThrow();
  });

  it("rejects malformed bodies", () => {
    const good = encodeAnnotation({ kind: "stroke", id: 1, end: false, points: pts(2) });
    for (const bad of [
      new Uint8Array(0),
      Uint8Array.of(1),
      Uint8Array.of(9, 0),
      Uint8Array.of(1, 0),
      Uint8Array.of(1, 2, 0, 0, 0, 0), // says 2 points, has 1
      good.slice(0, -1),
      Uint8Array.of(...good, 0),
      Uint8Array.of(2, 0, 1, 2, 0), // end flag 2
      Uint8Array.of(3, 2),
      Uint8Array.of(4, 1, 0),
      new Uint8Array(MAX_BODY + 1),
    ])
      expect(decodeAnnotation(bad)).toBeNull();
  });

  it("host-only kinds: policy and clearing everything", () => {
    expect(hostOnly({ kind: "policy", viewers: true })).toBe(true);
    expect(hostOnly({ kind: "clear", scope: "all" })).toBe(true);
    expect(hostOnly({ kind: "clear", scope: "own" })).toBe(false);
    expect(hostOnly({ kind: "laser", points: pts(1) })).toBe(false);
  });
});

describe("rate limits", () => {
  it("token bucket: burst, then the steady rate", () => {
    let t = 0;
    const b = new TokenBucket(VIEWER_SEND.rate, VIEWER_SEND.burst, () => t);
    let ok = 0;
    for (let i = 0; i < 20; i++) if (b.take()) ok++;
    expect(ok).toBe(8);
    expect(b.wait()).toBe(125);
    // Burst spent; over the next 10 s at 100 tries per second: 8 per second.
    ok = 0;
    for (t = 0; t <= 10_000; t += 10) if (b.take()) ok++;
    expect(ok).toBeGreaterThanOrEqual(79);
    expect(ok).toBeLessThanOrEqual(80);
  });

  it("viewer drawing is batched to ≤ 8 messages per second however fast the pointer moves", () => {
    let t = 0;
    const timers: { at: number; f: () => void }[] = [];
    const emitted: AnnotationMsg[] = [];
    const bucket = new TokenBucket(VIEWER_SEND.rate, VIEWER_SEND.burst, () => t);
    const input = new InkInput({
      echo: () => undefined,
      emit: (m) => (bucket.take(), emitted.push(m)),
      canSend: () => bucket.wait(),
      now: () => t,
      setTimer: (f, ms) => (timers.push({ at: t + ms, f }), timers.length as unknown as ReturnType<typeof setTimeout>),
      clearTimer: () => undefined,
    });
    const runTimers = () => {
      for (let due = timers.findIndex((x) => x.at <= t); due >= 0; due = timers.findIndex((x) => x.at <= t)) timers.splice(due, 1)[0]!.f();
    };
    input.setTool("pen");
    input.begin({ x: 0, y: 0 });
    for (t = 0; t < 5000; t += 4) {
      input.move({ x: (t % 1000) / 1000, y: 0.5 }); // a 250 Hz pointer
      runTimers();
    }
    input.end();
    for (; t < 7000; t += 4) runTimers();
    expect(emitted.length).toBeLessThanOrEqual(5 * 8 + 2);
    expect(emitted.length).toBeGreaterThan(30);
    // Every message is a stroke of the same id, ≤ 64 points, and the last one ends it.
    expect(emitted.every((m) => m.kind === "stroke" && m.points.length <= MAX_POINTS)).toBe(true);
    expect(new Set(emitted.map((m) => (m as { id: number }).id)).size).toBe(1);
    expect((emitted.at(-1) as { end: boolean }).end).toBe(true);
  });

  it("host accepts at most 10/s (burst 20) per admission and counts the rest", () => {
    let t = 0;
    const link = fakeLink([{ admissionId: "a", rowCode: "K7P" }]);
    const h = new HostAnnotations(link, { now: () => t });
    h.setViewers(true);
    const laser = encodeAnnotation({ kind: "laser", points: pts(1) });
    for (let i = 0; i < 100; i++) h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(laser) });
    expect(h.counters.accepted).toBe(HOST_ACCEPT.burst);
    expect(h.counters.dropped_rate).toBe(100 - HOST_ACCEPT.burst);
    t = 1000;
    for (let i = 0; i < 100; i++) h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(laser) });
    expect(h.counters.accepted).toBe(HOST_ACCEPT.burst + HOST_ACCEPT.rate);
    // Another admission has its own bucket.
    h.receive({ admissionId: "b", rowCode: "M3X", body: viewerBody(laser) });
    expect(h.counters.accepted).toBe(HOST_ACCEPT.burst + HOST_ACCEPT.rate + 1);
  });
});

describe("annotation store", () => {
  it("bounds strokes per origin and points per stroke; lasers fade", () => {
    const s = new AnnotationStore();
    for (let id = 0; id < MAX_STROKES + 5; id++) s.apply("K7P", { kind: "stroke", id, end: true, points: pts(2) }, id);
    expect(s.origins.get("K7P")!.strokes.size).toBe(MAX_STROKES);
    expect(s.origins.get("K7P")!.strokes.has(0)).toBe(false);
    for (let i = 0; i < 20; i++) s.apply("M3X", { kind: "stroke", id: 1, end: false, points: pts(64) }, 0);
    expect(s.origins.get("M3X")!.strokes.get(1)!.points.length).toBe(MAX_STROKE_POINTS);
    s.apply("M3X", { kind: "stroke", id: 1, end: true, points: [] }, 0);
    expect(s.apply("M3X", { kind: "stroke", id: 1, end: false, points: pts(1) }, 0)).toBe(false); // finished
    s.apply("host", { kind: "laser", points: pts(4) }, 1000, 100);
    expect(s.prune(1000)).toBe(true);
    expect(s.prune(1100 + LASER_FADE_MS)).toBe(false);
  });

  it("clear own vs clear all", () => {
    const s = new AnnotationStore();
    s.apply("K7P", { kind: "stroke", id: 1, end: true, points: pts(2) }, 0);
    s.apply("M3X", { kind: "stroke", id: 1, end: true, points: pts(2) }, 0);
    s.apply("K7P", { kind: "clear", scope: "own" }, 0);
    expect(s.origins.has("K7P")).toBe(false); // its strokes and laser
    expect(s.origins.get("M3X")!.strokes.size).toBe(1);
    s.apply("host", { kind: "clear", scope: "all" }, 0);
    expect(s.origins.size).toBe(0);
  });
});

function fakeLink(recipients: { admissionId: string; rowCode: string }[]) {
  const sent: { to: string; records: AnnotationRecord[] }[] = [];
  let onAnn: ((e: { admissionId: string; rowCode: string; body: Uint8Array }) => void) | null = null;
  let onAdm: ((e: { admissionId: string }) => void) | null = null;
  const link: HostLink & { sent: typeof sent; fire: (e: { admissionId: string; rowCode: string; body: Uint8Array }) => void; admit: (id: string) => void; recipientsList: typeof recipients } = {
    sent,
    recipientsList: recipients,
    onAnnotation: (f) => ((onAnn = f), () => (onAnn = null)),
    onAdmitted: (f) => ((onAdm = f), () => (onAdm = null)),
    onRevoked: () => () => undefined,
    allowed: () => true,
    recipients: () => recipients,
    sendAnnotations: async (to, records) => {
      sent.push({ to, records });
      return true;
    },
    fire: (e) => onAnn?.(e),
    admit: (id) => onAdm?.({ admissionId: id }),
  };
  return link;
}
const kinds = (records: AnnotationRecord[]) => payloadRecords(records).map((r) => `${r.origin}:${decodeAnnotation(r.body)!.kind}`);

describe("host router: policy and fan-out", () => {
  it("default host-only: viewer drawing is dropped until the host turns it on; host ink always goes out", async () => {
    const link = fakeLink([{ admissionId: "a", rowCode: "K7P" }, { admissionId: "b", rowCode: "M3X" }]);
    const h = new HostAnnotations(link);
    expect(h.viewersEnabled).toBe(false);
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation({ kind: "laser", points: pts(1) })) });
    expect(h.counters.dropped_policy).toBe(1);
    h.draw({ kind: "laser", points: pts(2) });
    await h.flush();
    // The host's overlay state leads every batch that carries ink.
    // (The first tick is also a resync tick: the policy rides along.)
    expect(link.sent.map((s) => [s.to, kinds(s.records)])).toEqual([["a", ["host:state", "host:policy", "host:laser"]], ["b", ["host:state", "host:policy", "host:laser"]]]);
  });

  it("relays a viewer's ink to everyone else, never back to the drawer, stamped with its row", async () => {
    const link = fakeLink([{ admissionId: "a", rowCode: "K7P" }, { admissionId: "b", rowCode: "M3X" }, { admissionId: "c", rowCode: "A9C" }]);
    let t = 0;
    const h = new HostAnnotations(link, { now: () => t });
    h.setViewers(true);
    await h.flush();
    link.sent.length = 0;
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation({ kind: "stroke", id: 1, end: false, points: pts(3) })) });
    t += 1000;
    await h.flush();
    // The drawer can receive restrictive repair state, but never its own ink.
    const ink = link.sent.filter((s) => payloadRecords(s.records).some((r) => decodeAnnotation(r.body)?.kind === "stroke"));
    expect(ink.map((s) => s.to).sort()).toEqual(["b", "c"]);
    for (const s of ink) expect(kinds(s.records)).toEqual(["host:state", "K7P:stroke"]);
    expect(link.sent.filter((s) => s.to === "a").every((s) => payloadRecords(s.records).every((r) => r.origin === "host" && ["state", "receipt"].includes(decodeAnnotation(r.body)?.kind ?? "")))).toBe(true);
    expect([...h.store.origins.get("K7P")!.strokes.values()][0]!.points.length).toBe(3);
  });

  it("viewers can't send host-only messages; malformed bodies are counted", () => {
    const h = new HostAnnotations(fakeLink([]));
    h.setViewers(true);
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation({ kind: "policy", viewers: true })) });
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation({ kind: "clear", scope: "all" })) });
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(Uint8Array.of(9, 9)) });
    expect(h.counters).toMatchObject({ dropped_policy: 2, malformed: 1, accepted: 0 });
  });

  it("a newly admitted viewer is told the policy at once; turning off and clearing reach everyone", async () => {
    const link = fakeLink([{ admissionId: "a", rowCode: "K7P" }]);
    const h = new HostAnnotations(link, { fanoutMs: 1e9, now: () => 0 });
    h.start();
    link.admit("a");
    await h.flush();
    expect(link.sent.map((s) => [s.to, kinds(s.records), decodeAnnotation(payloadRecords(s.records)[1]!.body)])).toEqual([["a", ["host:state", "host:policy"], { kind: "policy", viewers: false }]]);
    h.setViewers(true);
    h.setViewers(false);
    h.clear();
    (h as unknown as { nextAt: number }).nextAt = -Infinity;
    await h.flush();
    // Coalesced: only the newest policy goes out, then the clear.
    expect(kinds(link.sent.at(-1)!.records)).toEqual(["host:state", "host:policy", "host:clear"]);
    expect(decodeAnnotation(payloadRecords(link.sent.at(-1)!.records)[1]!.body)).toEqual({ kind: "policy", viewers: false });
    h.stop();
  });

  it("coalesces lasers per origin per tick, batches within 64 records and 2048 bytes, bounds the queue", async () => {
    const link = fakeLink([{ admissionId: "z", rowCode: "ZZZ".replace(/Z/g, "Y") }]);
    let t = 0;
    const h = new HostAnnotations(link, { now: () => t });
    h.setViewers(true);
    await h.flush();
    link.sent.length = 0;
    for (let i = 0; i < 10; i++) h.draw({ kind: "laser", points: pts(10) });
    t += 1000;
    await h.flush();
    expect(link.sent).toHaveLength(1);
    const laser = link.sent[0]!.records.map((r) => decodeAnnotation(r.body)!).find((m) => m.kind === "laser")!;
    expect(laser.kind === "laser" && laser.points.length).toBe(MAX_POINTS);
    link.sent.length = 0;
    for (let i = 0; i < 200; i++) h.draw({ kind: "stroke", id: i, end: true, points: pts(20) });
    // One batch per recipient per tick; the backlog drains over ticks.
    for (let tick = 0; tick < 40; tick++) {
      const before = link.sent.length;
      t += 1000;
      await h.flush();
      expect(link.sent.length - before).toBeLessThanOrEqual(1);
    }
    const sizes = link.sent.map((s) => s.records.reduce((n, r) => n + 4 + r.body.length, 1));
    expect(sizes.every((n) => n <= 2048)).toBe(true);
    expect(link.sent.every((s) => s.records.length <= 64)).toBe(true);
    // Every stroke arrives once (policy re-announcements ride along every 5 s).
    expect(link.sent.flatMap((s) => kinds(s.records)).filter((k) => k === "host:stroke")).toHaveLength(200);
    for (let i = 0; i < MAX_QUEUE + 10; i++) h.draw({ kind: "stroke", id: i, end: true, points: pts(1) });
    // A dropped stroke resynchronises its origin: that origin is cleared everywhere and its queued ink goes.
    expect(h.counters.dropped_overflow).toBeGreaterThanOrEqual(1);
    expect(h.pending.ink).toBeLessThanOrEqual(MAX_QUEUE);
    expect(h.pending.clearOrigins).toBe(1);
  });
});

describe("viewer router", () => {
  function make(code = "K7P") {
    const sent: Uint8Array[] = [];
    const v = new ViewerAnnotations({ seal: async (b) => b, send: (b) => sent.push(b), rowCode: () => code });
    return { v, sent };
  }
  const rec = (origin: string, m: AnnotationMsg): AnnotationRecord => ({ origin, body: encodeAnnotation(m) });

  it("draws nothing to the network until the host's policy allows it", async () => {
    const { v, sent } = make();
    v.input.begin({ x: 0.5, y: 0.5 });
    v.input.end();
    v.input.flush(true);
    await new Promise((r) => setTimeout(r, 0));
    expect(sent).toHaveLength(0);
    v.receive(fixtureBatch([rec("host", { kind: "policy", viewers: true })], "K7P"));
    expect(v.policy).toBe(true);
    v.input.begin({ x: 0.2, y: 0.2 });
    v.input.flush(true);
    await new Promise((r) => setTimeout(r, 0));
    expect(sent.map((b) => { const m = decodeAnnotation(b); return m?.kind === "input" ? m.msg.kind : m?.kind; })).toEqual(["laser"]);
    v.input.dispose();
  });

  it("only the host may set policy or clear everything; own ink isn't drawn twice", () => {
    const { v } = make("K7P");
    v.receive(fixtureBatch([rec("M3X", { kind: "policy", viewers: true }), rec("M3X", { kind: "clear", scope: "all" })], "K7P"));
    expect(v.policy).toBe(false);
    expect(v.counters.ignored).toBe(2);
    v.receive(fixtureBatch([rec("K7P", { kind: "laser", points: pts(1) }), rec("M3X", { kind: "stroke", id: 2, end: true, points: pts(4) })], "K7P"));
    expect(v.store.origins.has("K7P")).toBe(false);
    expect(v.store.origins.get("M3X")!.strokes.size).toBe(1);
    v.receive(fixtureBatch([rec("host", { kind: "clear", scope: "all", gen: 1 })], "K7P"));
    expect(v.store.origins.size).toBe(0);
  });

  it("the host turning drawing off ends this viewer's drawing", () => {
    const { v } = make();
    v.receive(fixtureBatch([rec("host", { kind: "policy", viewers: true })], "K7P"));
    v.input.begin({ x: 0.1, y: 0.1 });
    expect(v.input.active).toBe(true);
    v.receive(fixtureBatch([rec("host", { kind: "policy", viewers: false })], "K7P"));
    expect(v.policy).toBe(false);
    expect(v.input.active).toBe(false);
  });
});
