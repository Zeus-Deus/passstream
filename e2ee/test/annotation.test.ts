import { describe, expect, it } from "vitest";
import type { ViewerAdmissionResult } from "../src/admission.ts";
import {
  ANNOTATION_MAX_BATCH,
  ANNOTATION_MAX_BODY,
  ANNOTATION_MAX_RECORDS,
  type AnnotationRecord,
  annotationEpochSecret,
  decodeAnnotationRecords,
  encodeAnnotationRecords,
} from "../src/annotation.ts";
import { type Bytes, concat, randomBytes, toHex } from "../src/bytes.ts";
import { isE2EEError } from "../src/errors.ts";
import { deriveBaseKey, kidFor } from "../src/keyring.ts";
import { MemoryStorage, type Json } from "../src/storage.ts";
import { decodeMessage, encodeMessage, MsgType, peekType } from "../src/wire.ts";
import { type Host, join, makeHost, viewer } from "./harness.ts";

interface V {
  peer: string;
  res: ViewerAdmissionResult;
  code: string;
  id: string;
}

async function admit(h: Host, peer: string): Promise<V> {
  const res = await join(h, peer, viewer(h));
  for (const a of res.acks) await h.host.receive(peer, a);
  return { peer, res, code: res.rowCode, id: h.idOf(peer) };
}

/** Collect host "annotation" events. */
function listen(h: Host) {
  const got: { admissionId: string; rowCode: string; epoch: number; body: Bytes }[] = [];
  h.host.on("annotation", (e) => got.push(e));
  return got;
}

/** Deliver queued messages; REKEYs are handled and acked, ANNOTATIONS returned. */
async function drain(h: Host, v: V): Promise<Bytes[]> {
  const annotations: Bytes[] = [];
  for (const m of h.take(v.peer)) {
    if (peekType(m) === MsgType.Rekey) {
      const { ack } = await v.res.session.handle(m);
      await h.host.receive(v.peer, ack);
    } else if (peekType(m) === MsgType.Annotations) annotations.push(m);
  }
  return annotations;
}

const body = (s: string) => new TextEncoder().encode(s);
const text = (b: Uint8Array) => new TextDecoder().decode(b);
async function rejects(p: Promise<unknown>, code: string) {
  try {
    await p;
  } catch (e) {
    expect(isE2EEError(e) ? e.code : String(e)).toBe(code);
    return;
  }
  throw new Error(`expected ${code}, but succeeded`);
}

describe("annotations: E2EE round trip and authentication (PROTOCOL §20)", () => {
  it("viewer → host: the host learns the body and the authenticated row code", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    await h.host.receive("a", await a.res.session.sealAnnotation(body("laser at 10,20")));
    expect(got).toHaveLength(1);
    expect(got[0]!.rowCode).toBe(a.code);
    expect(got[0]!.admissionId).toBe(a.id);
    expect(got[0]!.epoch).toBe(h.active().epoch);
    expect(text(got[0]!.body)).toBe("laser at 10,20");
    expect(h.errors).toHaveLength(0);
  });

  it("host → viewer: origin-stamped records decrypt only for the recipient", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const b = await admit(h, "b");
    const records: AnnotationRecord[] = [
      { origin: "host", body: body("host laser") },
      { origin: b.code, body: body("b stroke") },
    ];
    expect(await h.host.sendAnnotations(a.id, records)).toBe(true);
    const [msg] = await drain(h, a);
    const opened = await a.res.session.openAnnotations(msg!);
    expect(opened.epoch).toBe(h.active().epoch);
    expect(opened.records.map((r) => [r.origin, text(r.body)])).toEqual([["host", "host laser"], [b.code, "b stroke"]]);
    // The same bytes are useless to another viewer (pairwise key): B can't
    // reuse what the host sent A, so it can't forge "the host says …".
    await rejects(b.res.session.openAnnotations(msg!), "decrypt_failed");
  });

  it("a relay that relabels the sending peer gets nothing through", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    await admit(h, "b");
    const got = listen(h);
    const sealed = await a.res.session.sealAnnotation(body("from a"));
    await h.host.receive("b", sealed); // claims B sent it
    expect(got).toHaveLength(0);
    expect(h.errors.at(-1)?.code).toBe("decrypt_failed");
    // Delivered under the right peer, the same message is fine (the failure didn't burn its counter).
    await h.host.receive("a", sealed);
    expect(got.map((g) => g.rowCode)).toEqual([a.code]);
  });

  it("upstream carries no origin: whatever the body says, the host stamps the sender's own row", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const b = await admit(h, "b");
    const got = listen(h);
    // A body that "claims" to be B or the host is still attributed to A.
    await h.host.receive("a", await a.res.session.sealAnnotation(body(`origin=${b.code};origin=host`)));
    expect(got[0]!.rowCode).toBe(a.code);
    expect(got[0]!.admissionId).toBe(a.id);
  });

  it("an upstream message can't be passed off as downstream (types and directions differ)", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const up = await a.res.session.sealAnnotation(body("x".repeat(40)));
    await rejects(a.res.session.openAnnotations(up), "unexpected_message");
    // Same fields, type byte switched: the direction label in the key and AAD differs.
    const m = decodeMessage(up);
    if (m.type !== MsgType.Annotation) throw new Error("type");
    const forged = encodeMessage({ type: MsgType.Annotations, epoch: m.epoch, ctr: m.ctr, box: concat(m.box, new Uint8Array(0)) });
    await rejects(a.res.session.openAnnotations(forged), "decrypt_failed");
  });

  it("tampering with epoch, counter or box fails authentication", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    const sealed = await a.res.session.sealAnnotation(body("tamper me"));
    const m = decodeMessage(sealed);
    if (m.type !== MsgType.Annotation) throw new Error("type");
    const box = m.box.slice();
    box[3]! ^= 1;
    await h.host.receive("a", encodeMessage({ ...m, box }));
    await h.host.receive("a", encodeMessage({ ...m, ctr: m.ctr + 5n }));
    expect(got).toHaveLength(0);
    expect(h.errors.map((e) => e.code)).toEqual(["decrypt_failed", "decrypt_failed"]);
  });

  it("replays and reordering are refused; fresh counters still pass", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    const m1 = await a.res.session.sealAnnotation(body("one"));
    const m2 = await a.res.session.sealAnnotation(body("two"));
    await h.host.receive("a", m2);
    await h.host.receive("a", m1); // older counter after a newer one
    await h.host.receive("a", m2); // replay
    await h.host.receive("a", await a.res.session.sealAnnotation(body("three")));
    expect(got.map((g) => text(g.body))).toEqual(["two", "three"]);
    expect(h.errors.map((e) => e.code)).toEqual(["replay", "replay"]);
    // Downstream too.
    await h.host.sendAnnotations(a.id, [{ origin: "host", body: body("d1") }]);
    const [d1] = await drain(h, a);
    expect((await a.res.session.openAnnotations(d1!)).records[0]!.origin).toBe("host");
    await rejects(a.res.session.openAnnotations(d1!), "replay");
  });

  it("epoch rotation: both directions follow the new epoch; epochs beyond the newest four are refused", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    const e0 = h.active().epoch;
    const stale = await a.res.session.sealAnnotation(body("old epoch"));
    for (let i = 0; i < 4; i++) {
      await h.host.keys.rekey("manual");
      await drain(h, a);
    }
    expect(h.active().epoch).toBe(e0 + 4);
    expect(a.res.session.annotationEpoch).toBe(e0 + 4);
    await h.host.receive("a", await a.res.session.sealAnnotation(body("new epoch")));
    expect(got.map((g) => [g.epoch, text(g.body)])).toEqual([[e0 + 4, "new epoch"]]);
    // e0 is now the fifth-newest epoch: the host no longer holds its key.
    await h.host.receive("a", stale);
    expect(h.errors.at(-1)?.code).toBe("unknown_key");
    await h.host.sendAnnotations(a.id, [{ origin: "host", body: body("after rotation") }]);
    const [d] = await drain(h, a);
    const opened = await a.res.session.openAnnotations(d!);
    expect(opened.epoch).toBe(e0 + 4);
  });

  it("kick: the kicked viewer is cut off at once, and the epoch moves on for the others", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const b = await admit(h, "b");
    const got = listen(h);
    const before = h.active().epoch;
    // A kicked viewer's best try: messages it sealed with keys it still holds.
    const presealed = [await a.res.session.sealAnnotation(body("pre 1")), await a.res.session.sealAnnotation(body("pre 2"))];
    const kick = h.host.kick(a.id);
    // Synchronously after the kick call: no annotation from A is accepted, none is sent to A.
    await h.host.receive("a", presealed[0]!);
    expect(await h.host.sendAnnotations(a.id, [{ origin: "host", body: body("to a") }])).toBe(false);
    await drain(h, b);
    h.clock.advance(400);
    await h.host.keys.tick();
    await kick;
    await drain(h, b);
    expect(h.active().epoch).toBeGreaterThan(before);
    await h.host.receive("a", presealed[1]!);
    expect(got).toHaveLength(0);
    expect(h.errors.map((e) => e.code)).toEqual(["unexpected_message", "unexpected_message"]);
    expect(h.take("a").filter((m) => peekType(m) === MsgType.Annotations)).toHaveLength(0);
    // B carries on, on the new epoch, and A can't read what B gets.
    await h.host.receive("b", await b.res.session.sealAnnotation(body("b still draws")));
    expect(got.map((g) => g.rowCode)).toEqual([b.code]);
    await h.host.sendAnnotations(b.id, [{ origin: "host", body: body("to b") }]);
    const [toB] = await drain(h, b);
    expect((await b.res.session.openAnnotations(toB!)).epoch).toBe(h.active().epoch);
    await rejects(a.res.session.openAnnotations(toB!), "unknown_key");
  });

  it("a disposed viewer session can't seal or open", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    await h.host.sendAnnotations(a.id, [{ origin: "host", body: body("x") }]);
    const [d] = await drain(h, a);
    a.res.session.dispose();
    await rejects(a.res.session.sealAnnotation(body("x")), "state");
    await rejects(a.res.session.openAnnotations(d!), "state");
  });
});

describe("annotations: size bounds and encoding", () => {
  it("upstream bodies are 1–512 bytes", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    await h.host.receive("a", await a.res.session.sealAnnotation(randomBytes(ANNOTATION_MAX_BODY)));
    expect(got).toHaveLength(1);
    await rejects(a.res.session.sealAnnotation(randomBytes(ANNOTATION_MAX_BODY + 1)), "invalid_argument");
    await rejects(a.res.session.sealAnnotation(new Uint8Array(0)), "invalid_argument");
    // A relay-crafted oversized message is refused by the parser before any crypto.
    const big = concat(Uint8Array.of(1, MsgType.Annotation, 4), new Uint8Array(4), Uint8Array.of(8), new Uint8Array(8), Uint8Array.of(0x91, 0x04), randomBytes(529));
    await h.host.receive("a", big);
    expect(h.errors.at(-1)?.code).toBe("malformed");
  });

  it("downstream batches: 1–64 records, bodies 1–512 bytes, at most 2048 bytes", () => {
    const one = { origin: "host", body: randomBytes(10) };
    expect(() => encodeAnnotationRecords([])).toThrow(/record count/);
    expect(() => encodeAnnotationRecords(Array(ANNOTATION_MAX_RECORDS + 1).fill(one))).toThrow(/record count/);
    expect(() => encodeAnnotationRecords([{ origin: "host", body: randomBytes(ANNOTATION_MAX_BODY + 1) }])).toThrow(/body size/);
    expect(() => encodeAnnotationRecords(Array(5).fill({ origin: "host", body: randomBytes(500) }))).toThrow(/too large/);
    expect(() => encodeAnnotationRecords([{ origin: "K7Q", body: randomBytes(4) }])).toThrow(/noncanonical/);
    const ok = encodeAnnotationRecords(Array(4).fill({ origin: "K7P", body: randomBytes(500) }));
    expect(ok.length).toBeLessThanOrEqual(ANNOTATION_MAX_BATCH);
    expect(decodeAnnotationRecords(ok).map((r) => r.origin)).toEqual(["K7P", "K7P", "K7P", "K7P"]);
  });

  it("decoding is strict: counts, lengths, origins, trailing bytes", () => {
    const good = encodeAnnotationRecords([{ origin: "host", body: Uint8Array.of(1, 2, 3) }]);
    expect(decodeAnnotationRecords(good)[0]!.origin).toBe("host");
    expect(() => decodeAnnotationRecords(concat(good, Uint8Array.of(0)))).toThrow(/trailing/);
    expect(() => decodeAnnotationRecords(Uint8Array.of(0))).toThrow(/count/);
    expect(() => decodeAnnotationRecords(Uint8Array.of(1, 0, 0, 0, 9, 1))).toThrow(/length/);
    expect(() => decodeAnnotationRecords(Uint8Array.of(1, 0x29, 0x98, 0, 1, 7))).toThrow(/out of range/); // index 10648
    expect(() => decodeAnnotationRecords(Uint8Array.of(1, 0, 0, 0, 0))).toThrow(/length/);
  });
});

describe("annotations: domain separation from SFrame", () => {
  it("the annotation epoch input differs from the epoch secret and every SFrame base key", () => {
    const k = { epoch: 7, secret: randomBytes(32) };
    const a = toHex(annotationEpochSecret(k));
    expect(a).not.toBe(toHex(k.secret));
    for (let track = 0; track < 16; track++) expect(a).not.toBe(toHex(deriveBaseKey(k.secret, kidFor(7, track))));
    // Bound to the epoch number too.
    expect(a).not.toBe(toHex(annotationEpochSecret({ epoch: 8, secret: k.secret })));
  });
});

describe("annotations: a requested kick ends annotation access at once (review D2 #2)", () => {
  class Held extends MemoryStorage {
    held = false;
    entered!: () => void;
    enter = new Promise<void>((r) => (this.entered = r));
    release!: () => void;
    barrier = new Promise<void>((r) => (this.release = r));
    override async update(key: string, fn: (cur: Json | undefined) => Json) {
      if (this.held) {
        this.entered();
        await this.barrier;
      }
      return super.update(key, fn);
    }
  }

  it("a kick queued behind a held rotation refuses the victim's ink at kick +400 ms", async () => {
    const storage = new Held();
    const h = await makeHost({ storage });
    const a = await admit(h, "a");
    const b = await admit(h, "b");
    const msg = await a.res.session.sealAnnotation(body("after kick"));
    const got = listen(h);
    const revoked: { admissionIds: string[]; rowCode: string }[] = [];
    h.host.on("annotationRevoked", (e) => revoked.push(e));
    storage.held = true;
    const rotation = h.host.rotatePin("5937");
    await storage.enter;
    const kick = h.host.kick(a.id);
    // Synchronous with the request: the router hears it, and nothing more goes to A.
    expect(revoked).toEqual([{ admissionIds: [a.id], rowCode: a.code }]);
    expect(h.host.annotationAllowed(a.id)).toBe(false);
    expect(h.host.annotationAllowed(b.id)).toBe(true);
    expect(await h.host.sendAnnotations(a.id, [{ origin: "host", body: body("x") }])).toBe(false);
    h.clock.advance(400);
    const tick = h.host.keys.tick();
    await h.host.receive("a", msg);
    expect(got).toHaveLength(0);
    expect(h.errors.at(-1)?.code).toBe("unexpected_message");
    // B is unaffected while the kick waits.
    await h.host.receive("b", await b.res.session.sealAnnotation(body("b")));
    expect(got.map((g) => g.rowCode)).toEqual([b.code]);
    storage.held = false;
    storage.release();
    await rotation;
    await kick;
    await tick;
  });

  it("an annotation whose decrypt began before the kick request is refused after it", async () => {
    const h = await makeHost();
    const a = await admit(h, "a");
    const got = listen(h);
    const msg = await a.res.session.sealAnnotation(body("racing the kick"));
    const receiving = h.host.receive("a", msg); // decrypt is in flight (WebCrypto is async)
    const kick = h.host.kick(a.id);
    await receiving;
    expect(got).toHaveLength(0);
    h.clock.advance(400);
    await h.host.keys.tick();
    await kick;
  });
});

describe("annotations at 100 viewers: a kick still switches within 300 ms (review D2 #1)", () => {
  it("one viewer draws at 8/s, the host fans out to 99, a kick lands: every survivor acks, the switch is ≤ 300 ms", async () => {
    const h = await makeHost();
    const vs: V[] = [];
    for (let i = 0; i < 100; i++) vs.push(await admit(h, `p${i}`));
    for (const v of vs) h.take(v.peer);
    let activated: { acked: number; timedOut: string[]; epoch: number } | null = null;
    let activatedAt = -1;
    h.host.keys.on("activated", (e) => ((activated = e), (activatedAt = h.clock.t)));
    const got = listen(h);
    const drawer = vs[0]!;
    const victim = vs[50]!;
    const survivors = vs.filter((v) => v !== victim);
    const before = h.active().epoch;
    let kickAt = -1;
    let kick: Promise<string[]> | null = null;
    // 1 s of drawing in 125 ms ticks; the kick is requested at 500 ms.
    for (let tick = 0; tick < 8; tick++) {
      await h.host.receive(drawer.peer, await drawer.res.session.sealAnnotation(body(`laser ${tick}`)));
      for (const v of survivors) if (v !== drawer) await h.host.sendAnnotations(v.id, [{ origin: drawer.code, body: body(`laser ${tick}`) }]);
      if (tick === 4) {
        kickAt = h.clock.t;
        kick = h.host.kick(victim.id);
      }
      // The relay delivers everything in order: annotations and rekeys interleaved; viewers ack rekeys.
      for (const v of survivors) {
        for (const m of h.take(v.peer)) {
          if (peekType(m) === MsgType.Rekey) {
            const { ack } = await v.res.session.handle(m);
            await h.host.receive(v.peer, ack);
          } else expect(peekType(m)).toBe(MsgType.Annotations);
        }
      }
      h.clock.advance(125);
      await h.host.keys.tick();
    }
    await kick;
    expect(got.length).toBe(8);
    expect(h.active().epoch).toBeGreaterThan(before);
    expect(activated).not.toBeNull();
    // Every survivor acked: nothing timed out, and the switch didn't wait for the deadline.
    expect(activated!.acked).toBe(99);
    expect(activated!.timedOut).toEqual([]);
    expect(h.host.annotationAllowed(victim.id)).toBe(false);
    expect(activatedAt - kickAt).toBeLessThanOrEqual(300);
    console.log(JSON.stringify({ viewers: 100, survivors_acked: activated!.acked, switch_after_kick_ms: activatedAt - kickAt }));
  });
});
