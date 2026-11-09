import { afterEach, expect, it, vi } from "vitest";
import { encodeAnnotationRecords, type AnnotationRecord } from "@passstream/e2ee";
import { HostAnnotations, MAX_QUEUE } from "./host";
import { ViewerAnnotations } from "./viewer";
import { decodeAnnotation, encodeAnnotation, MAX_SOURCES, sourceSlot } from "./codec";
import { join, makeHost, viewer } from "../../../e2ee/test/harness";
import { randomBytes } from "../../../e2ee/src/bytes";
import { MsgType, peekType } from "../../../e2ee/src/wire";

const alive: ViewerAnnotations[] = [];
afterEach(() => { alive.splice(0).forEach(v => v.dispose()); vi.restoreAllMocks(); });
const ids = (v: { store: ViewerAnnotations["store"] }, row: string) => [...(v.store.origins.get(row)?.strokes.keys() ?? [])];
const settle = async () => { for (let i = 0; i < 50; i++) await Promise.resolve(); };
const stroke = (id: number) => ({ kind: "stroke" as const, id, end: true, points: [{ x: .2, y: .3 }] });

it.each([2, 3])("%i concurrent shared-grant admissions preserve colliding strokes, held authentic first input, own clear and host purges", async count => {
  vi.spyOn(Math, "random").mockReturnValue(0); // Deliberately collide every local 16-bit stroke ID.
  const c = await makeHost(), sid = randomBytes(16);
  const first = await join(c, "p0", viewer(c, { viewerSid: sid }));
  const admissions = [first];
  for (let i = 1; i < count; i++) admissions.push(await join(c, `p${i}`, viewer(c, { viewerSid: sid, grant: first.grant })));
  const observer = await join(c, "observer", viewer(c));
  admissions.push(observer);
  for (let i = 0; i < admissions.length; i++) for (const ack of admissions[i]!.acks) await c.host.receive(i === count ? "observer" : `p${i}`, ack);
  let t = 0, drop = false;
  const up: Uint8Array[][] = admissions.map(() => []);
  const viewers = admissions.map((a, i) => new ViewerAnnotations({ seal: b => a.session.sealAnnotation(b), send: b => up[i]!.push(b), rowCode: () => a.rowCode }, { now: () => t }));
  alive.push(...viewers);
  const peer = (i: number) => i === count ? "observer" : `p${i}`;
  const admissionIds = admissions.map((_, i) => c.idOf(peer(i)));
  const h = new HostAnnotations({ onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: id => c.host.annotationAllowed(id),
    recipients: () => admissions.map((a, i) => ({ admissionId: admissionIds[i]!, rowCode: a.rowCode })),
    sendAnnotations: (id, rs, current) => drop ? Promise.resolve(true) : c.host.sendAnnotations(id, rs, current),
  }, { now: () => t });
  const off = c.host.on("annotation", e => h.receive(e));
  const tick = async () => {
    t += 1000; await h.flush();
    for (let i = 0; i < admissions.length; i++) for (const b of c.take(peer(i))) if (peekType(b) === MsgType.Annotations) viewers[i]!.receive((await admissions[i]!.session.openAnnotations(b)).records); else if (peekType(b) === MsgType.Rekey) { const { ack } = await admissions[i]!.session.handle(b); await c.host.receive(peer(i), ack); }
  };
  const draw = async (i: number) => {
    const v = viewers[i]!, n = up[i]!.length;
    v.setTool("pen"); v.input.begin({ x: .2, y: .3 }); v.input.end(); v.input.flush(true);
    await vi.waitFor(() => expect(up[i]).toHaveLength(n + 1));
    return up[i]![n]!;
  };
  const deliver = (i: number, b: Uint8Array) => c.host.receive(peer(i), b);
  const row = first.rowCode;
  h.setViewers(true); await tick();
  const held = await draw(0);
  for (let i = 1; i < count; i++) await deliver(i, await draw(i));
  await tick();
  const fresh = ids(h, row); expect(fresh).toHaveLength(count - 1);
  await deliver(0, held); await tick();
  for (const v of [h, ...viewers]) expect(ids(v, row)).toHaveLength(count);
  expect(new Set([...h.store.origins.get(row)!.strokes.values()].map(s => s.id)).size).toBe(1);
  for (const id of fresh) expect(ids(viewers[count]!, row)).toContain(id);
  // The drawer clears only its causal incarnation; all other accepted ink survives.
  const beforeClear = up[0]!.length;
  viewers[0]!.clearOwn(); await vi.waitFor(() => expect(up[0]).toHaveLength(beforeClear + 1)); await deliver(0, up[0]!.at(-1)!);
  drop = true; await tick(); drop = false; // Lose only the purge, never the new live-only ink.
  const newInk = await draw(0); await deliver(0, newInk); await tick();
  for (let i = 0; i < 40; i++) await tick();
  for (const v of [h, ...viewers]) { expect(ids(v, row)).toHaveLength(count); for (const id of fresh) expect(ids(v, row)).toContain(id); }
  // Overflow is a forced row barrier covering every live incarnation, including held input.
  const delayed = await draw(1);
  await deliver(0, await draw(0));
  for (let i = 0; i < MAX_QUEUE; i++) h.draw(stroke(1000 + i));
  expect(h.counters.dropped_overflow).toBeGreaterThan(0);
  await deliver(1, delayed); await tick();
  for (const v of [h, ...viewers]) expect(ids(v, row)).toEqual([]);
  // Fresh input remains allowed for every shared-grant admission after the barrier.
  for (let i = 0; i < count; i++) { expect(c.host.annotationAllowed(admissionIds[i]!)).toBe(true); await deliver(i, await draw(i)); }
  await tick(); expect(ids(h, row)).toHaveLength(count);
  h.clear(); await tick(); for (const v of [h, ...viewers]) expect(ids(v, row)).toEqual([]);
  for (let i = 0; i < count; i++) await deliver(i, await draw(i));
  await tick(); expect(ids(h, row)).toHaveLength(count);
  await c.host.kick(admissionIds[0]!);
  h.revoke({ admissionIds: admissions.slice(0, count).map((_, i) => admissionIds[i]!), rowCode: row });
  // Only the observer remains eligible; repair purges every victim incarnation.
  await tick(); expect(ids(h, row)).toEqual([]); expect(ids(viewers[count]!, row)).toEqual([]);
  for (let i = 0; i < count; i++) expect(c.host.annotationAllowed(admissionIds[i]!)).toBe(false);
  off(); admissions.forEach(a => a.session.dispose());
});

it("namespace bootstrap fails closed, rebases once per admission, and fences held prior-router input", async () => {
  let t = 0; const sent: Uint8Array[] = [], batches: AnnotationRecord[][] = [];
  let h!: HostAnnotations;
  const d = new ViewerAnnotations({ seal: async b => b, send: b => sent.push(b), rowCode: () => "AAA" }); alive.push(d);
  const make = () => new HostAnnotations({ onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: () => true,
    recipients: () => [{ admissionId: "a", rowCode: "AAA" }], sendAnnotations: async (_, rs) => { batches.push(rs); d.receive(rs); return true; },
  }, { now: () => t });
  d.receive([{ origin: "host", body: encodeAnnotation({ kind: "policy", viewers: true }) }]); expect(d.policy).toBe(false);
  h = make(); h.setViewers(true); await h.flush();
  d.setTool("pen"); d.input.begin({ x: .2, y: .3 }); d.input.end(); d.input.flush(true); await settle();
  const held = sent[0]!, old = batches[0]!;
  h = make(); d.beginAdmission(); h.setViewers(true); t += 1000; await h.flush();
  d.receive(old); // A prior namespace can no longer rebase this current authenticated admission.
  h.receive({ admissionId: "a", rowCode: "AAA", body: held }); expect(h.counters.dropped_stale).toBe(1);
  d.input.begin({ x: .4, y: .5 }); d.input.end(); d.input.flush(true); await settle();
  h.receive({ admissionId: "a", rowCode: "AAA", body: sent.at(-1)! }); expect(h.counters.accepted).toBe(1);
});

it("500 admission slots and their causal repair pages stay finite through churn, and every cutoff is visited before idle", async () => {
  let t = 0; const allowed = new Set<string>(); let recipients: { admissionId: string; rowCode: string }[] = [];
  let maxBytes = 0, maxRecords = 0, maxBody = 0; const states = new Set<bigint>();
  const h = new HostAnnotations({ onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: id => allowed.has(id), recipients: () => recipients,
    sendAnnotations: async (_, rs) => {
      maxBytes = Math.max(maxBytes, encodeAnnotationRecords(rs).length); maxRecords = Math.max(maxRecords, rs.length);
      for (const r of rs) { maxBody = Math.max(maxBody, r.body.length); const m = decodeAnnotation(r.body); if (m?.kind === "sources") for (const e of m.entries) states.add(e.source); }
      return true;
    },
  }, { now: () => t, namespace: 1n });
  for (let i = 0; i < 1200; i++) {
    if (i >= MAX_SOURCES) allowed.delete(`a${i - MAX_SOURCES}`);
    const id = `a${i}`; allowed.add(id);
    h.receive({ admissionId: id, rowCode: "AAA", body: encodeAnnotation({ kind: "input", namespace: 1n, session: BigInt(i + 1), op: 1, clearThrough: 1, gen: 0, barrier: 0, msg: { kind: "clear", scope: "own" } }) });
    if (i === 0) h.setViewers(true);
  }
  recipients = [...allowed].map(admissionId => ({ admissionId, rowCode: "AAA" }));
  h.setViewers(false);
  for (let i = 0; i < 100; i++) { t += 3125; await h.flush(); }
  const internals = h as unknown as { inputs: Map<string, unknown>; sources: { source: bigint }[]; buckets: Map<string, unknown> };
  expect(internals.inputs.size).toBe(MAX_SOURCES); expect(internals.sources).toHaveLength(MAX_SOURCES); expect(internals.buckets.size).toBeLessThanOrEqual(MAX_SOURCES);
  for (const s of internals.sources) expect(states.has(s.source)).toBe(true);
  expect(new Set(internals.sources.map(s => sourceSlot(s.source))).size).toBe(MAX_SOURCES);
  expect(maxBytes).toBeLessThanOrEqual(2048); expect(maxRecords).toBeLessThanOrEqual(64); expect(maxBody).toBeLessThanOrEqual(512);
  const ticks = h.counters.ticks; t += 10000; await h.flush(); expect(h.counters.ticks).toBe(ticks);
});
