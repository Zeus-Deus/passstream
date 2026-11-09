import { afterEach, expect, it, vi } from "vitest";
import { AnnotationChannel, importAnnotationBase } from "../src/annotation.ts";
import { randomBytes } from "../src/bytes.ts";
import { join, makeHost, viewer } from "./harness.ts";
import { MsgType, peekType } from "../src/wire.ts";

afterEach(() => vi.restoreAllMocks());
const body = Uint8Array.of(1, 2, 3);
async function channels(role: "sender" | "viewer") {
  const base = await importAnnotationBase(randomBytes(32));
  return { tx: new AnnotationChannel(base, role), rx: new AnnotationChannel(base, role === "sender" ? "viewer" : "sender"), secrets: [randomBytes(32), randomBytes(32)] };
}
function holdDecrypt() {
  let release!: () => void, entered!: () => void;
  const held = new Promise<void>(r => { release = r; });
  const ready = new Promise<void>(r => { entered = r; });
  const decrypt = crypto.subtle.decrypt.bind(crypto.subtle);
  vi.spyOn(crypto.subtle, "decrypt").mockImplementationOnce(async (...args) => {
    const result = await decrypt(...args); // authentic old plaintext is already available
    entered(); await held;
    return result;
  });
  return { release, ready };
}

it.each(["sender", "viewer"] as const)("%s: first delivery of retained old epochs and concurrent old decrypt output cannot cross authenticated newer output", async role => {
  const { tx, rx, secrets: [s1, s2] } = await channels(role);
  const old = await tx.seal(1, s1!, body);
  const late = await tx.seal(1, s1!, body);
  const newer = await tx.seal(2, s2!, body);
  const gate = holdDecrypt();
  const opening = rx.open(1, s1!, old.ctr, old.box);
  const rejected = expect(opening).rejects.toMatchObject({ code: "replay" });
  await gate.ready;
  expect(await rx.open(2, s2!, newer.ctr, newer.box)).toEqual(body);
  gate.release(); await rejected;
  await expect(rx.open(1, s1!, late.ctr, late.box)).rejects.toMatchObject({ code: "replay" });
  const fresh = await tx.seal(2, s2!, body);
  expect(await rx.open(2, s2!, fresh.ctr, fresh.box)).toEqual(body);
});

it.each(["sender", "viewer"] as const)("%s: unauthenticated future epochs/counters do not poison valid old or new data; identical plaintext with fresh counters is valid", async role => {
  const { tx, rx, secrets: [s1, s2] } = await channels(role);
  const first = await tx.seal(1, s1!, body);
  const future = await tx.seal(2, s2!, body);
  const bad = future.box.slice(); bad[0]! ^= 1;
  await expect(rx.open(2, s2!, 999n, bad)).rejects.toMatchObject({ code: "decrypt_failed" });
  expect(await rx.open(1, s1!, first.ctr, first.box)).toEqual(body);
  expect(await rx.open(2, s2!, future.ctr, future.box)).toEqual(body);
  const same = await tx.seal(2, s2!, body);
  expect(await rx.open(2, s2!, same.ctr, same.box)).toEqual(body);
  await expect(rx.open(2, s2!, future.ctr, future.box)).rejects.toMatchObject({ code: "replay" });
});

it("same-counter concurrent opens authenticate at most once; disposed pending decrypt/encrypt cannot escape", async () => {
  const { tx, rx, secrets: [s] } = await channels("sender");
  const msg = await tx.seal(1, s!, body);
  const gate = holdDecrypt();
  const pending = rx.open(1, s!, msg.ctr, msg.box);
  const rejected = expect(pending).rejects.toMatchObject({ code: "replay" });
  await gate.ready;
  expect(await rx.open(1, s!, msg.ctr, msg.box)).toEqual(body);
  gate.release(); await rejected;
  const fresh = await tx.seal(1, s!, body);
  const g2 = holdDecrypt();
  const p2 = rx.open(1, s!, fresh.ctr, fresh.box);
  const disposed = expect(p2).rejects.toMatchObject({ code: "state" });
  await g2.ready; rx.dispose(); g2.release(); await disposed;
  await expect(rx.open(1, s!, fresh.ctr, fresh.box)).rejects.toMatchObject({ code: "state" });
  let release!: () => void, entered!: () => void;
  const ready = new Promise<void>(r => { entered = r; });
  const hold = new Promise<void>(r => { release = r; });
  const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
  vi.spyOn(crypto.subtle, "encrypt").mockImplementationOnce(async (...args) => { const b = await encrypt(...args); entered(); await hold; return b; });
  const sealing = tx.seal(1, s!, body);
  const noSeal = expect(sealing).rejects.toMatchObject({ code: "state" });
  await ready; tx.dispose(); release(); await noSeal;
});

it("retained-key rollover never forgets accepted receive ordering or resets a live send counter", async () => {
  const { tx, rx, secrets: [s] } = await channels("sender");
  const first = await tx.seal(1, s!, body);
  await rx.open(1, s!, first.ctr, first.box);
  for (let e = 2; e < 10; e++) {
    const bad = first.box.slice(); bad[0]! ^= 1;
    await expect(rx.open(e, s!, 1n, bad)).rejects.toMatchObject({ code: "decrypt_failed" });
    await expect(tx.open(e, s!, 1n, bad)).rejects.toMatchObject({ code: "decrypt_failed" });
  }
  await expect(rx.open(1, s!, first.ctr, first.box)).rejects.toMatchObject({ code: "replay" });
  const next = await tx.seal(1, s!, body);
  expect(next.ctr).toBe(2n);
  await rx.open(1, s!, next.ctr, next.box);
  const newer = await tx.seal(9, s!, body);
  await rx.open(9, s!, newer.ctr, newer.box);
  await expect(tx.seal(1, s!, body)).rejects.toMatchObject({ code: "replay" });
  await expect(rx.open(1, s!, 999n, first.box)).rejects.toMatchObject({ code: "replay" });
});

it("grant re-admission has independent annotation counters and keys; a disposed old session cannot open held output", async () => {
  const h = await makeHost();
  const sid = randomBytes(16);
  const a = await join(h, "old", viewer(h, { viewerSid: sid }));
  for (const ack of a.acks) await h.host.receive("old", ack);
  await h.host.sendAnnotations(h.idOf("old"), [{ origin: "host", body }]);
  const old = h.take("old").find(m => peekType(m) === MsgType.Annotations)!;
  a.session.dispose();
  await h.host.peerClosed("old");
  const b = await join(h, "new", viewer(h, { viewerSid: sid, grant: a.grant }));
  for (const ack of b.acks) await h.host.receive("new", ack);
  await expect(a.session.openAnnotations(old)).rejects.toBeDefined();
  await expect(b.session.openAnnotations(old)).rejects.toMatchObject({ code: "decrypt_failed" });
  await h.host.sendAnnotations(h.idOf("new"), [{ origin: "host", body }]);
  const fresh = h.take("new").find(m => peekType(m) === MsgType.Annotations)!;
  expect((await b.session.openAnnotations(fresh)).records).toEqual([{ origin: "host", body }]);
  b.session.dispose();
});

it("router lifetime invalidation fences an already sealed downstream batch without removing a live admission", async () => {
  const c = await makeHost(), a = await join(c, "viewer", viewer(c));
  for (const ack of a.acks) await c.host.receive("viewer", ack);
  let current = true, entered!: () => void, release!: () => void;
  const ready = new Promise<void>(r => { entered = r; }), gate = new Promise<void>(r => { release = r; });
  const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
  vi.spyOn(crypto.subtle, "encrypt").mockImplementationOnce(async (...args) => { const box = await encrypt(...args); entered(); await gate; return box; });
  const sending = c.host.sendAnnotations(c.idOf("viewer"), [{ origin: "host", body }], () => current);
  await ready; current = false; release();
  expect(await sending).toBe(false); expect(c.host.annotationAllowed(c.idOf("viewer"))).toBe(true);
  expect(c.take("viewer").filter(b => peekType(b) === MsgType.Annotations)).toEqual([]);
  expect(await c.host.sendAnnotations(c.idOf("viewer"), [{ origin: "host", body }])).toBe(true);
  a.session.dispose();
});
