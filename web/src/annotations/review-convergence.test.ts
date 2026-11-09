import { fixtureBatch, payloadRecords } from "./test-input";
import { viewerBody } from "./test-input";
/** Final D2 review probes: silent relay loss after kick and ordinary queue overflow.
 * Both assertions reproduce the independent failure scenarios without changing their expectations. */
import { expect, it } from "vitest";
import type { AnnotationRecord } from "@passstream/e2ee";
import type { HostLink } from "./host";
import { HostAnnotations } from "./test-input";
import { ViewerAnnotations } from "./viewer";
import { encodeAnnotation, decodeAnnotation, MAX_BODY, MAX_STATE_ORIGINS, ORIGIN_COUNT, originFromIndex } from "./codec";
import { ClearGenerations } from "./generations";
const alphabet = "ACDEFHJKMNPRTUVWXY3479";
const code = (i: number) => alphabet[Math.floor(i / 484)]! + alphabet[Math.floor(i / 22) % 22]! + alphabet[i % 22]!;
const stroke = (id: number) => ({ kind: "stroke" as const, id, end: true, points: [{ x: 0.5, y: 0.5 }] });
function pair(rowCode = "YYY") {
  const clock = { t: 0, drop: false };
  const allow = new Set(["observer"]);
  const v = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => rowCode }, { now: () => clock.t });
  const states: Extract<NonNullable<ReturnType<typeof decodeAnnotation>>, { kind: "state" }>[] = [];
  const batches: AnnotationRecord[][] = [];
  const link: HostLink = {
    onAnnotation: () => () => {},
    onAdmitted: () => () => {},
    onRevoked: () => () => {},
    allowed: (id) => allow.has(id),
    recipients: () => [{ admissionId: "observer", rowCode }],
    sendAnnotations: async (_, records) => {
      batches.push(records);
      if (clock.drop) return true; // relay silently discards an otherwise accepted envelope
      for (const r of records) {
        const m = decodeAnnotation(r.body);
        if (m?.kind === "state") states.push(m);
      }
      v.receive(records);
      return true;
    },
  };
  const h = new HostAnnotations(link, { now: () => clock.t });
  return { clock, allow, v, h, states, batches, link };
}
it.each([82, 502])("lost kicked-origin clear must converge with %i historical ink origins", async (origins) => {
  const { clock, allow, v, h, states } = pair();
  h.setViewers(true);
  await h.flush();
  for (let i = 0; i < origins; i++) {
    const id = `a${i}`;
    allow.add(id);
    clock.t += 1000;
    h.receive({ admissionId: id, rowCode: code(i), body: viewerBody(encodeAnnotation(stroke(1))) });
    await h.flush();
    if (i > 0) allow.delete(id); // departed origins retain ink; concurrent admissions stay below 500
  }
  expect(v.store.origins.has(code(0))).toBe(true);
  clock.t += 1000;
  clock.drop = true;
  allow.delete("a0");
  h.revoke({ admissionIds: ["a0"], rowCode: code(0) });
  await h.flush();
  clock.drop = false;
  for (let i = 0; i < 40; i++) {
    clock.t += 1000;
    await h.flush();
  }
  console.log(
    JSON.stringify({
      scenario: "partial-state-kick",
      host_origins: h.store.origins.size,
      recipient_origins: v.store.origins.size,
      host_victim: h.store.origins.has(code(0)),
      recipient_victim: v.store.origins.has(code(0)),
      last_state_partial: states.at(-1)?.partial,
      last_state_origins: states.at(-1)?.origins.length,
      pending: h.pending,
    }),
  );
  const stale = v.store.origins.has(code(0));
  v.dispose();
  expect(stale).toBe(false);
});
it("a lost host per-origin clear must reach the original drawer via state", async () => {
  const { clock, allow, v, h, states } = pair("AAA");
  allow.add("drawer");
  h.setViewers(true);
  await h.flush();
  clock.t += 1000;
  const msg = stroke(1);
  v.store.apply("AAA", msg, clock.t);
  h.receive({ admissionId: "drawer", rowCode: "AAA", body: viewerBody(encodeAnnotation(msg)) });
  // Fill the production queue, whose oldest record belongs to the drawer.
  // The 256th host record evicts that pending stroke and clears its origin.
  for (let i = 0; i < 256; i++) h.draw(stroke(i + 10));
  expect(h.counters.dropped_overflow).toBe(1);
  expect(h.store.origins.has("AAA")).toBe(false);
  clock.drop = true;
  await h.flush();
  clock.drop = false;
  for (let i = 0; i < 40; i++) {
    clock.t += 1000;
    await h.flush();
  }
  console.log(
    JSON.stringify({
      scenario: "own-origin-clear",
      host_origins: h.store.origins.size,
      recipient_origins: v.store.origins.size,
      last_state_partial: states.at(-1)?.partial,
      last_state_origins: states.at(-1)?.origins.length,
      pending: h.pending,
    }),
  );
  const stale = v.store.origins.has("AAA");
  v.dispose();
  expect(stale).toBe(false);
});

it("repair retains every namespace tombstone through churn, without starving pages or breaking the 500-recipient budget", async () => {
  const { clock, v, h, allow, batches, link: originalLink } = pair();
  const victim = code(0);
  v.receive(fixtureBatch([]));
  v.store.apply(victim, stroke(1), 0);
  // Exercise the entire finite namespace, well beyond 500 concurrent admissions.
  for (let i = 0; i < ORIGIN_COUNT; i++) h.revoke({ admissionIds: [], rowCode: originFromIndex(i)! });
  const recipients = Array.from({ length: 500 }, (_, i) => ({ admissionId: `r${i}`, rowCode: code(i) }));
  for (const r of recipients) allow.add(r.admissionId);
  let envelopes = 0,
    largestBatch = 0,
    largestBody = 0,
    mostRecords = 0;
  const seen = new Set<string>();
  const link: HostLink = {
    onAnnotation: () => () => {},
    onAdmitted: () => () => {},
    onRevoked: () => () => {},
    allowed: (id) => allow.has(id),
    recipients: () => recipients,
    sendAnnotations: async (id, records) => {
      envelopes++;
      mostRecords = Math.max(mostRecords, records.length);
      largestBatch = Math.max(largestBatch, 1 + records.reduce((n, r) => n + 4 + r.body.length, 0));
      for (const r of records) largestBody = Math.max(largestBody, r.body.length);
      if (id === "r0" && !clock.drop) {
        for (const r of records) {
          const m = decodeAnnotation(r.body);
          if (m?.kind === "state") for (const o of m.origins) seen.add(o.origin);
        }
        v.receive(records);
      }
      return true;
    },
  };
  // Use the production router's existing link object, whose closures the fixture owns.
  Object.assign(originalLink, link);
  clock.drop = true;
  await h.flush(); // one silently discarded batch per recipient
  clock.drop = false;
  expect(v.store.origins.has(victim)).toBe(true);
  // Every tick sends <=500 envelopes, separated by 500/160 seconds. Continue
  // changing an early origin: this must not reset the repair cursor and starve its tail.
  for (let tick = 1; tick <= 410; tick++) {
    clock.t = tick * 3125;
    if (tick <= 134) h.revoke({ admissionIds: [], rowCode: code(1) });
    await h.flush();
    if (tick === 10) expect(v.store.origins.has(victim)).toBe(true); // its dropped page has not rotated back yet
  }
  expect(seen.size).toBe(ORIGIN_COUNT);
  expect(v.store.origins.has(victim)).toBe(false);
  expect(envelopes).toBeLessThanOrEqual(411 * 500);
  expect(envelopes / (clock.t / 1000)).toBeLessThanOrEqual(160 + 500 / (clock.t / 1000));
  expect(largestBody).toBeLessThanOrEqual(MAX_BODY);
  expect(largestBatch).toBeLessThanOrEqual(2048);
  expect(mostRecords).toBeLessThanOrEqual(64);
  expect(h.pending.clearOrigins).toBe(0);
  // Extra sweeps finish after the final change, then the host-only empty stream is silent.
  clock.t += 30_000;
  for (let tick = 0; tick < 410; tick++) {
    clock.t += 5000;
    await h.flush();
  }
  const quiet = envelopes;
  for (let tick = 0; tick < 10; tick++) {
    clock.t += 5000;
    await h.flush();
  }
  expect(envelopes).toBe(quiet);
  expect(batches).toHaveLength(0); // the replacement link is the sole delivery path
  console.log(JSON.stringify({ namespace_repair: { origins: seen.size, recipients: 500, envelopes, largestBody, largestBatch, mostRecords } }));
  v.dispose();
});

it("new ink leads with its generation even while another full repair page occupies the batch", async () => {
  const { clock, v, h, batches } = pair();
  // Put host's tombstone at the very end of the namespace, away from the next page.
  h.draw(stroke(1));
  await h.flush();
  clock.t = 1000;
  clock.drop = true;
  for (let i = 0; i < 600; i++) h.revoke({ admissionIds: [], rowCode: code(i) });
  h.draw({ kind: "clear", scope: "own" });
  await h.flush();
  const stale = batches.at(-1)!;
  clock.t += 1000;
  clock.drop = false;
  h.draw(stroke(2));
  await h.flush();
  const next = batches.at(-1)!;
  const inkAt = next.findIndex((r) => r.origin === "host" && decodeAnnotation(r.body)?.kind === "stroke");
  expect(
    next.slice(0, inkAt).some((r) => {
      const m = decodeAnnotation(r.body);
      return m?.kind === "state" && m.origins.some((o) => o.origin === "host" && o.gen === 1);
    }),
  ).toBe(true);
  expect([...v.store.origins.get("host")!.strokes.keys()]).toEqual([2]);
  // Simulate delayed/duplicated restrictive records after the new stroke.
  v.receive(fixtureBatch([...stale, { origin: "host", body: encodeAnnotation({ kind: "clear", scope: "own", gen: 1 }) }]));
  for (let i = 0; i < 50; i++) {
    clock.t += 1000;
    await h.flush();
  }
  expect([...v.store.origins.get("host")!.strokes.keys()]).toEqual([2]);
  for (const batch of batches) {
    expect(1 + batch.reduce((n, r) => n + 4 + r.body.length, 0)).toBeLessThanOrEqual(2048);
    expect(batch.length).toBeLessThanOrEqual(64);
    for (const r of batch) {
      const m = decodeAnnotation(r.body);
      expect(r.body.length).toBeLessThanOrEqual(MAX_BODY);
      if (m?.kind === "state") expect(m.origins.length).toBeLessThanOrEqual(MAX_STATE_ORIGINS);
    }
  }
  v.dispose();
});

it("generation storage covers exactly the finite namespace and never wraps or forgets old clears", () => {
  const g = new ClearGenerations();
  for (let i = 0; i < ORIGIN_COUNT; i++) expect(g.advance(originFromIndex(i)!)).toBe(1);
  const visited = new Set<string>();
  for (let i = 0; i < Math.ceil(ORIGIN_COUNT / MAX_STATE_ORIGINS) + 1; i++) {
    for (const o of g.page().origins) visited.add(o.origin);
    g.advance(code(0)); // updates never move the cursor backwards
  }
  expect(visited.size).toBe(ORIGIN_COUNT);
  expect(g.get(code(1))).toBe(1);
  expect(g.observe(code(1), 0xffff_ffff)).toBe(true);
  expect(() => g.advance(code(1))).toThrow("generation exhausted");
  expect(() => g.get("bad")).toThrow("origin");
  expect(g.observe(code(1), 1)).toBe(false);
  // Reserved wire indices remain invalid even though the host has a dense table slot.
  const invalid = encodeAnnotation({ kind: "state", gen: 0, partial: true, origins: [{ origin: "host", gen: 1 }] });
  new DataView(invalid.buffer).setUint16(7, ORIGIN_COUNT - 1);
  expect(decodeAnnotation(invalid)).toBeNull();
});
