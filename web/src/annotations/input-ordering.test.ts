import { afterEach, expect, it, vi } from "vitest";
import { encodeAnnotationRecords, type AnnotationRecord } from "@passstream/e2ee";
import { HostAnnotations } from "./test-input";
import { ViewerAnnotations } from "./viewer";
import { decodeAnnotation, encodeAnnotation, type AnnotationMsg, type InputMsg } from "./codec";
import { join, makeHost, viewer } from "../../../e2ee/test/harness";
import { decodeMessage, MsgType, peekType } from "../../../e2ee/src/wire";

const settle = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
const ids = (v: { store: ViewerAnnotations["store"] }, origin = "AAA") => [...(v.store.origins.get(origin)?.strokes.keys() ?? [])];
const stroke = (id: number) => ({ kind: "stroke" as const, id, end: true, points: [{ x: 0.3, y: 0.4 }] });
const alive: ViewerAnnotations[] = [];
afterEach(() => { alive.splice(0).forEach(v => v.dispose()); vi.restoreAllMocks(); });
function pair() {
  let t = 0;
  const upstream: Uint8Array[] = [];
  const batches: AnnotationRecord[][] = [];
  let hold = false, drop = false;
  let dropId: string | undefined;
  const observer = new ViewerAnnotations({ seal: async b => b, send: () => {}, rowCode: () => "AAC" }, { now: () => t });
  let h!: HostAnnotations;
  const drawer = new ViewerAnnotations({ seal: async b => b, send: body => {
    if (hold) upstream.push(body);
    else h.receive({ admissionId: "a", rowCode: "AAA", body });
  }, rowCode: () => "AAA" }, { now: () => t });
  alive.push(drawer, observer);
  h = new HostAnnotations({
    onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: () => true,
    recipients: () => [{ admissionId: "a", rowCode: "AAA" }, { admissionId: "b", rowCode: "AAC" }],
    sendAnnotations: async (id, rs) => {
      expect(encodeAnnotationRecords(rs).length).toBeLessThanOrEqual(2048);
      expect(rs.length).toBeLessThanOrEqual(64);
      batches.push(rs);
      if (!drop || (dropId && id !== dropId)) (id === "a" ? drawer : observer).receive(rs);
      return true;
    },
  }, { now: () => t });
  const tick = async () => { t += 1000; await h.flush(); };
  const draw = async () => {
    drawer.setTool("pen"); drawer.input.begin({ x: 0.7, y: 0.8 }); drawer.input.end(); drawer.input.flush(true);
    await settle();
    return ids(drawer).at(-1)!;
  };
  const deliver = (body: Uint8Array) => h.receive({ admissionId: "a", rowCode: "AAA", body });
  return { h, drawer, observer, upstream, batches, tick, draw, deliver,
    hold: (v: boolean) => { hold = v; }, drop: (v: boolean, id?: string) => { drop = v; dropId = id; },
    start: async () => { h.setViewers(true); await tick(); },
    repair: async () => { for (let i = 0; i < 45; i++) await tick(); },
  };
}

it("review: clearOwn confirmation preserves the immediately drawn and accepted new local stroke", async () => {
  const p = pair(); await p.start();
  const old = await p.draw(); await p.tick();
  p.drawer.clearOwn(); await settle();
  const fresh = await p.draw(); expect(fresh).not.toBe(old);
  expect(ids(p.h)).toEqual([fresh]);
  await p.tick(); await p.repair();
  expect(ids(p.drawer)).toEqual([fresh]); expect(ids(p.observer)).toEqual([fresh]);
});

it("lost clear request, multiple rapid local clears, and lost confirmation batches preserve only the newest input", async () => {
  const p = pair(); await p.start(); await p.draw(); await p.tick();
  p.hold(true);
  p.drawer.clearOwn(); await settle();
  await p.draw();
  p.drawer.clearOwn(); await settle();
  const fresh = await p.draw();
  const latest = p.upstream.at(-1)!;
  expect(decodeAnnotation(latest)).toMatchObject({ kind: "input", clearThrough: 4, op: 5 });
  // All earlier requests/data are silently lost; the newest input carries its clear boundary.
  p.deliver(latest); expect(ids(p.h)).toEqual([fresh]);
  p.drop(true, "a"); await p.tick(); p.drop(false);
  await p.repair();
  expect(ids(p.drawer)).toEqual([fresh]); expect(ids(p.observer)).toEqual([fresh]);
});

it.each(["before-clear", "after-clear", "after-new-ink"])("a forced purge interleaved %s with a pending own clear is authoritative, without blocking fresh input after observing it", async when => {
  const p = pair(); await p.start(); await p.draw(); await p.tick();
  p.hold(true);
  p.drawer.clearOwn(); await settle(); const clear = p.upstream.at(-1)!;
  const concurrent = await p.draw(); const ink = p.upstream.at(-1)!;
  const force = () => p.h.revoke({ admissionIds: [], rowCode: "AAA" });
  if (when === "before-clear") force();
  p.deliver(clear);
  if (when === "after-clear") force();
  p.deliver(ink);
  if (when === "after-new-ink") force();
  expect(ids(p.h)).not.toContain(concurrent);
  p.drop(true); await p.tick(); p.drop(false); await p.repair();
  expect(ids(p.drawer)).toEqual([]); expect(ids(p.observer)).toEqual([]);
  p.hold(false); const fresh = await p.draw();
  expect(ids(p.h)).toEqual([fresh]); await p.tick();
  expect(ids(p.drawer)).toEqual([fresh]); expect(ids(p.observer)).toEqual([fresh]);
});

it("real host queue overflow purges the original drawer after a dropped receipt, and rejects held tails of that pen operation", async () => {
  const p = pair(); await p.start();
  p.drawer.setTool("pen"); p.drawer.input.begin({ x: 0.1, y: 0.1 }); p.drawer.input.flush(true); await settle();
  const old = ids(p.drawer)[0]!;
  p.hold(true); p.drawer.input.move({ x: 0.2, y: 0.2 }); p.drawer.input.flush(true); await settle();
  const tail = p.upstream.at(-1)!;
  for (let i = 0; i < 256; i++) p.h.draw(stroke(100 + i));
  expect(p.h.counters.dropped_overflow).toBe(1); expect(ids(p.h)).toEqual([]);
  p.deliver(tail); expect(ids(p.h)).toEqual([]);
  p.drop(true); await p.tick(); p.drop(false); await p.repair();
  expect(ids(p.drawer)).not.toContain(old); expect(p.drawer.input.active).toBe(false);
  p.hold(false); const fresh = await p.draw(); await p.tick();
  expect(ids(p.h)).toEqual([fresh]); expect(ids(p.drawer)).toEqual([fresh]);
});

it.each(["global", "origin"])("an authoritative %s barrier clears optimistic ink whose upstream envelope was silently lost with highestObserved=0", async scope => {
  const p = pair(); await p.start(); p.hold(true);
  const old = await p.draw(); const held = p.upstream.at(-1)!;
  expect(ids(p.h)).toEqual([]); expect(ids(p.drawer)).toEqual([old]);
  if (scope === "global") p.h.clear(); else p.h.revoke({ admissionIds: [], rowCode: "AAA" });
  await p.tick(); expect(ids(p.drawer)).toEqual([]);
  // Even its first delivery after the barrier cannot revive it at the host or observers.
  p.deliver(held); await p.tick(); await p.repair();
  expect(ids(p.h)).toEqual([]); expect(ids(p.observer)).toEqual([]); expect(ids(p.drawer)).toEqual([]);
  expect(p.h.counters.dropped_stale).toBe(1);
  p.hold(false); const fresh = await p.draw(); await p.tick();
  expect(ids(p.drawer)).toEqual([fresh]); expect(ids(p.observer)).toEqual([fresh]);
});

it("a new authenticated admission keeps a separate input source and ignores old-session receipts", async () => {
  const p = pair(); await p.start(); await p.draw();
  p.drawer.clearOwn(); await settle(); await p.tick();
  const oldReceipt = p.batches.flat().find(r => decodeAnnotation(r.body)?.kind === "receipt")!;
  const reloaded = new ViewerAnnotations({ seal: async b => b, send: body => p.h.receive({ admissionId: "new-a", rowCode: "AAA", body }), rowCode: () => "AAA" }); alive.push(reloaded);
  reloaded.receive([{ origin: "host", body: encodeAnnotation({ kind: "bootstrap", namespace: 1n, source: 65538n, gen: 0, barrier: 0 }) }, { origin: "host", body: encodeAnnotation({ kind: "policy", viewers: true }) }]); reloaded.setTool("pen"); reloaded.input.begin({ x: 0.2, y: 0.2 }); reloaded.input.end(); reloaded.input.flush(true); await settle();
  const fresh = ids(reloaded);
  reloaded.receive([oldReceipt]); expect(ids(reloaded)).toEqual(fresh);
  await p.tick(); expect(ids(p.h)).toEqual(fresh); expect(ids(p.observer)).toEqual(fresh);
});

it.each(["all", "own"] as const)("real rekey: delayed first epoch-1 ink cannot resurrect %s-cleared ink after epoch-2 authentication", async scope => {
  const c = await makeHost(); const a = await join(c, "observer", viewer(c));
  for (const ack of a.acks) await c.host.receive("observer", ack);
  let t = 0;
  const v = new ViewerAnnotations({ seal: async b => b, send: () => {}, rowCode: () => a.rowCode }, { now: () => t }); alive.push(v);
  const h = new HostAnnotations({
    onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: id => c.host.annotationAllowed(id),
    recipients: () => [{ admissionId: c.idOf("observer"), rowCode: a.rowCode }], sendAnnotations: (id, rs) => c.host.sendAnnotations(id, rs),
  }, { now: () => t });
  const deliver = async () => {
    for (const msg of c.take("observer")) {
      if (peekType(msg) === MsgType.Rekey) { const { ack } = await a.session.handle(msg); await c.host.receive("observer", ack); }
      else if (peekType(msg) === MsgType.Annotations) v.receive((await a.session.openAnnotations(msg)).records);
    }
  };
  h.draw(stroke(1)); await h.flush();
  const held = c.take("observer").find(m => peekType(m) === MsgType.Annotations)!;
  const heldMessage = decodeMessage(held);
  expect(heldMessage.type).toBe(MsgType.Annotations);
  if (heldMessage.type !== MsgType.Annotations) throw new Error("expected annotations");
  expect(heldMessage.epoch).toBe(1);
  await c.host.keys.rekey("manual"); await deliver(); expect(c.active().epoch).toBe(2);
  t = 1000; if (scope === "all") h.clear(); else h.draw({ kind: "clear", scope: "own" });
  await h.flush(); await deliver();
  await expect(a.session.openAnnotations(held)).rejects.toMatchObject({ code: "replay" });
  for (t = 2000; t < 45000; t += 1000) { await h.flush(); await deliver(); }
  expect(ids(v, "host")).toEqual([]); a.session.dispose();
});

it("real crypto: held authentic upstream pre-global-clear input is refused even when never previously received", async () => {
  const c = await makeHost(); const a = await join(c, "drawer", viewer(c));
  for (const ack of a.acks) await c.host.receive("drawer", ack);
  let t = 0;
  const h = new HostAnnotations({
    onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: id => c.host.annotationAllowed(id),
    recipients: () => [{ admissionId: c.idOf("drawer"), rowCode: a.rowCode }], sendAnnotations: (id, rs) => c.host.sendAnnotations(id, rs),
  }, { now: () => t });
  c.host.on("annotation", e => h.receive(e));
  const up: Uint8Array[] = [];
  const d = new ViewerAnnotations({ seal: b => a.session.sealAnnotation(b), send: b => up.push(b), rowCode: () => a.rowCode }); alive.push(d);
  const down = async () => { for (const msg of c.take("drawer")) if (peekType(msg) === MsgType.Annotations) d.receive((await a.session.openAnnotations(msg)).records); };
  h.setViewers(true); await h.flush(); await down();
  d.setTool("pen"); d.input.begin({ x: 0.2, y: 0.3 }); d.input.end(); d.input.flush(true);
  await vi.waitFor(() => expect(up).toHaveLength(1));
  const held = up[0]!; h.clear(); t = 1000; await h.flush(); await down();
  expect(ids(d, a.rowCode)).toEqual([]);
  await c.host.receive("drawer", held); expect(h.counters.dropped_stale).toBe(1);
  expect(ids(h, a.rowCode)).toEqual([]);
  // Cross a real rekey with a pending local clear and immediate new stroke.
  await c.host.keys.rekey("manual");
  for (const msg of c.take("drawer")) if (peekType(msg) === MsgType.Rekey) { const { ack } = await a.session.handle(msg); await c.host.receive("drawer", ack); }
  d.clearOwn(); d.input.begin({ x: 0.5, y: 0.6 }); d.input.end(); d.input.flush(true);
  await vi.waitFor(() => expect(up).toHaveLength(3));
  for (const msg of up.slice(1)) await c.host.receive("drawer", msg);
  const fresh = ids(d, a.rowCode); expect(fresh).toHaveLength(1);
  t = 2000; await h.flush(); await down();
  expect(ids(h, a.rowCode)).toEqual(fresh); expect(ids(d, a.rowCode)).toEqual(fresh);
  a.session.dispose();
});

it("causal codecs reject nesting, host-only input, zero/wrapped operations, mismatched clear boundaries and invalid receipts", () => {
  const input: InputMsg = { kind: "input", namespace: 1n, session: 1n, op: 1, clearThrough: 0, gen: 0, barrier: 0, msg: stroke(1) };
  expect(decodeAnnotation(encodeAnnotation(input))).toEqual({ ...input, msg: { ...stroke(1), points: [{ x: 19661 / 65535, y: 0.4 }] } });
  const maximum = encodeAnnotation({ ...input, msg: { ...stroke(1), points: Array.from({ length: 64 }, () => ({ x: 0, y: 0 })) } });
  expect(maximum.length).toBe(294);
  for (const msg of [{ ...input, op: 0 }, { ...input, op: 0x1_0000_0000 }, { ...input, session: 0n }, { ...input, clearThrough: 2 }]) expect(() => encodeAnnotation(msg)).toThrow();
  for (const kind of [4, 5, 6, 7]) { const bad = encodeAnnotation(input); bad[33] = kind; expect(decodeAnnotation(bad)).toBeNull(); }
  const invalidClear = encodeAnnotation({ ...input, clearThrough: 1, msg: { kind: "clear", scope: "own" } });
  invalidClear[34] = 0; expect(decodeAnnotation(invalidClear)).toBeNull();
  const receipt: AnnotationMsg = { kind: "receipt", session: 0n, through: 0, barrier: 12 };
  expect(decodeAnnotation(encodeAnnotation(receipt))).toEqual(receipt);
  expect(() => encodeAnnotation({ ...receipt, through: 1 })).toThrow();
  const truncated = encodeAnnotation(input).subarray(0, 26); expect(decodeAnnotation(truncated)).toBeNull();
});
