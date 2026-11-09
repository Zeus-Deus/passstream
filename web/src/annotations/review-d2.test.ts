import { fixtureBatch, payloadRecords } from "./test-input";
import { viewerBody } from "./test-input";
/**
 * Regressions for the D2 review round 1 (GPT-6.1-Sol): each started from the
 * reviewer's probe with its assertion inverted.
 */
import { describe, expect, it } from "vitest";
import type { AnnotationRecord } from "@passstream/e2ee";
import { decodeAnnotation, encodeAnnotation } from "./codec";
import type { HostLink } from "./host";
import { HostAnnotations } from "./test-input";
import { FANOUT_RATE, VIEWER_SEND } from "./rate";
import { OUTBOX_MAX, ViewerAnnotations } from "./viewer";

type Sent = { id: string; records: AnnotationRecord[]; at: number };
function link(recipients: { admissionId: string; rowCode: string }[], clock: { t: number }) {
  const sent: Sent[] = [];
  const allowed = new Set(recipients.map((r) => r.admissionId));
  let revoked: ((e: { admissionIds: string[]; rowCode: string }) => void) | null = null;
  const l: HostLink & { sent: Sent[]; allow: Set<string>; revoke: (ids: string[], code: string) => void; list: typeof recipients } = {
    sent,
    allow: allowed,
    list: recipients,
    onAnnotation: () => () => undefined,
    onAdmitted: () => () => undefined,
    onRevoked: (f) => ((revoked = f), () => (revoked = null)),
    allowed: (id) => allowed.has(id),
    recipients: () => l.list,
    sendAnnotations: async (id, records) => (sent.push({ id, records, at: clock.t }), true),
    revoke: (ids, code) => {
      for (const id of ids) allowed.delete(id);
      revoked?.({ admissionIds: ids, rowCode: code });
    },
  };
  return l;
}
const kinds = (rs: AnnotationRecord[]) => payloadRecords(rs).map((r) => `${r.origin}:${decodeAnnotation(r.body)!.kind}`);
const stroke = (id: number, n = 1) => encodeAnnotation({ kind: "stroke", id, end: true, points: Array.from({ length: n }, () => ({ x: 0.1, y: 0.2 })) });

export const fanoutNumbers: Record<string, number>[] = [];
describe("#1 fan-out is bounded per recipient and in aggregate", () => {
  it("a full queue goes to one recipient as exactly one batch per tick (was 37)", async () => {
    const clock = { t: 0 };
    const l = link([{ admissionId: "b", rowCode: "AAA" }], clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.setViewers(true);
    for (let v = 0; v < 16; v++) l.allow.add(`a${v}`);
    for (let v = 0; v < 16; v++) for (let i = 0; i < 16; i++) h.receive({ admissionId: `a${v}`, rowCode: "K7P", body: viewerBody(stroke(i, 64)) });
    await h.flush();
    expect(l.sent.length).toBe(1);
    expect(l.sent[0]!.records.length).toBeLessThanOrEqual(64);
  });

  it("100 viewers, one drawing at 8/s for 10 s: ≤ FANOUT_RATE envelopes/s in every second, ≤ 1 batch per recipient per tick", async () => {
    const clock = { t: 0 };
    const recipients = Array.from({ length: 100 }, (_, i) => ({ admissionId: `v${i}`, rowCode: `A${"ACDEFHJKMNPRTUVWXY3479"[i % 22]}${"ACDEFHJKMNPRTUVWXY3479"[Math.floor(i / 22)]}` }));
    const l = link(recipients, clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.setViewers(true);
    const drawer = recipients[0]!;
    let ticks = 0;
    for (clock.t = 0; clock.t < 10_000; clock.t += 25) {
      if (clock.t % 125 === 0) h.receive({ admissionId: drawer.admissionId, rowCode: drawer.rowCode, body: viewerBody(encodeAnnotation({ kind: "laser", points: [{ x: clock.t / 10_000, y: 0.5 }] })) });
      if (clock.t % 125 === 0) {
        const before = l.sent.length;
        await h.flush();
        const perRecipient = new Map<string, number>();
        for (const s of l.sent.slice(before)) perRecipient.set(s.id, (perRecipient.get(s.id) ?? 0) + 1);
        expect(Math.max(0, ...perRecipient.values())).toBeLessThanOrEqual(1);
        if (l.sent.length > before) ticks++;
      }
    }
    // Average ≤ FANOUT_RATE; any sliding 1 s window ≤ FANOUT_RATE plus one tick (one batch per recipient).
    expect(l.sent.length / 10).toBeLessThanOrEqual(FANOUT_RATE);
    let worst = 0;
    for (const s of l.sent) worst = Math.max(worst, l.sent.filter((x) => x.at >= s.at && x.at < s.at + 1000).length);
    expect(worst).toBeLessThanOrEqual(FANOUT_RATE + recipients.length);
    // Well inside the server's separate annotation budget (publisher 256/s, burst 512).
    expect(worst).toBeLessThanOrEqual(256);
    fanoutNumbers.push({ envelopes: l.sent.length, per_s_avg: l.sent.length / 10, worst_1s_window: worst, ticks });
    // Without the bound this was 99 × 8 = 792/s. The drawer never gets its own ink.
    expect(l.sent.some((s) => s.id === drawer.admissionId && s.records.some((r) => r.origin === drawer.rowCode))).toBe(false);
    // The others still see the laser move: a batch each roughly every 625 ms.
    expect(ticks).toBeGreaterThanOrEqual(13);
    console.log(JSON.stringify({ fanout_100_viewers_one_drawer: fanoutNumbers.at(-1) }));
  });
});

describe("#2 a requested kick removes pending and visible ink", () => {
  it("ink queued from a viewer whose admission is no longer allowed is never relayed", async () => {
    const clock = { t: 0 };
    const l = link([{ admissionId: "a", rowCode: "K7P" }, { admissionId: "b", rowCode: "AAA" }], clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.setViewers(true);
    await h.flush();
    l.sent.length = 0;
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(stroke(1)) });
    l.allow.delete("a"); // kicked (or gone) before the tick
    l.list = l.list.filter((r) => r.admissionId !== "a");
    clock.t += 1000;
    await h.flush();
    expect(l.sent.flatMap((s) => kinds(s.records)).filter((k) => k.startsWith("K7P:stroke"))).toEqual([]);
    expect(h.counters.dropped_revoked).toBe(1);
  });

  it("the revocation event purges the victim's queued ink, clears its ink here and sends a clear of its origin", async () => {
    const clock = { t: 0 };
    const l = link([{ admissionId: "a", rowCode: "K7P" }, { admissionId: "b", rowCode: "AAA" }], clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.start();
    h.setViewers(true);
    await h.flush();
    l.sent.length = 0;
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(stroke(1)) });
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(stroke(2)) });
    expect(h.store.origins.has("K7P")).toBe(true);
    l.revoke(["a"], "K7P");
    expect(h.store.origins.has("K7P")).toBe(false);
    // Ink that was already decrypted when the kick was requested is refused too.
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(stroke(3)) });
    l.list = l.list.filter((r) => r.admissionId !== "a");
    clock.t += 1000;
    await h.flush();
    expect(l.sent.map((s) => kinds(s.records))).toEqual([["host:state", "K7P:clear"]]);
    expect(decodeAnnotation(payloadRecords(l.sent[0]!.records)[1]!.body)).toEqual({ kind: "clear", scope: "own", gen: 1 });
    expect(h.counters.dropped_revoked).toBe(3);
    h.stop();
  });
});

describe("#3 the viewer's 8/s limit is enforced at the final send", () => {
  it("50 new strokes at a frozen clock send at most the burst (was 49), with a bounded outbox", async () => {
    let sends = 0;
    const a = new ViewerAnnotations({ seal: async (b) => b, send: () => sends++, rowCode: () => "K7P" }, { now: () => 1000 });
    a.receive(fixtureBatch([{ origin: "host", body: encodeAnnotation({ kind: "policy", viewers: true }) }]));
    a.setTool("pen");
    for (let i = 0; i < 50; i++) {
      a.input.begin({ x: 0.2, y: 0.3 });
      a.input.end();
    }
    a.clearOwn();
    await new Promise((r) => setTimeout(r, 30));
    expect(sends).toBeLessThanOrEqual(VIEWER_SEND.burst);
    expect((a as unknown as { outbox: unknown[] }).outbox.length).toBeLessThanOrEqual(OUTBOX_MAX);
    a.dispose();
  });

  it("over 3 s of continuous forced strokes, sends stay within 8/s plus the burst", async () => {
    let t = 0;
    let sends = 0;
    const a = new ViewerAnnotations({ seal: async (b) => b, send: () => sends++, rowCode: () => "K7P" }, { now: () => t });
    a.receive(fixtureBatch([{ origin: "host", body: encodeAnnotation({ kind: "policy", viewers: true }) }]));
    a.setTool("pen");
    const start = performance.now();
    while (performance.now() - start < 3000) {
      t = performance.now() - start;
      a.input.begin({ x: 0.2, y: 0.3 });
      a.input.move({ x: 0.3, y: 0.3 });
      a.input.end();
      await new Promise((r) => setTimeout(r, 5));
    }
    await new Promise((r) => setTimeout(r, 50));
    expect(sends).toBeLessThanOrEqual(VIEWER_SEND.burst + Math.ceil(3 * VIEWER_SEND.rate) + 1);
    expect(sends).toBeGreaterThan(10);
    a.dispose();
  });
});

describe("#4 restrictive control state is never evicted", () => {
  it("a full stroke queue still delivers policy-off and clear-all, first in the batch", async () => {
    const clock = { t: 0 };
    const l = link([{ admissionId: "b", rowCode: "AAA" }], clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.setViewers(true);
    await h.flush();
    l.sent.length = 0;
    for (let v = 0; v < 16; v++) l.allow.add(`a${v}`);
    for (let v = 0; v < 16; v++) for (let i = 0; i < 16; i++) h.receive({ admissionId: `a${v}`, rowCode: "K7P", body: viewerBody(stroke(i)) });
    for (let i = 0; i < 300; i++) h.draw({ kind: "stroke", id: i, end: true, points: [{ x: 0.5, y: 0.5 }] }); // overflow
    h.setViewers(false);
    h.clear();
    clock.t += 1000;
    await h.flush();
    const first = kinds(l.sent[0]!.records);
    expect(first).toEqual(["host:state", "host:policy", "host:clear"]); // ink queued before the clear is moot
    expect(decodeAnnotation(payloadRecords(l.sent[0]!.records)[1]!.body)).toEqual({ kind: "policy", viewers: false });
    expect(decodeAnnotation(payloadRecords(l.sent[0]!.records)[2]!.body)).toMatchObject({ kind: "clear", scope: "all" });
  });

  it("evicting ink resynchronises: that origin is cleared everywhere", async () => {
    const clock = { t: 0 };
    const l = link([{ admissionId: "b", rowCode: "AAA" }], clock);
    const h = new HostAnnotations(l, { now: () => clock.t });
    for (let i = 0; i < 300; i++) h.draw({ kind: "stroke", id: i, end: true, points: [{ x: 0.5, y: 0.5 }] });
    expect(h.counters.dropped_overflow).toBeGreaterThan(0);
    expect(h.store.origins.has("host")).toBe(true); // the newest strokes after the resync
    await h.flush();
    expect(kinds(l.sent[0]!.records)[0]).toBe("host:state");
    expect(kinds(l.sent[0]!.records)).toContain("host:clear");
  });
});
