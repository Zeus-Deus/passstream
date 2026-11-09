import { describe, expect, it } from "vitest";
import type { ViewerAdmissionResult } from "../src/admission.ts";
import { randomBytes } from "../src/bytes.ts";
import { FrameDecryptor, FrameEncryptor } from "../src/frame-cryptor.ts";
import { ReceiverKeyRing, SenderKeyRing } from "../src/keyring.ts";
import { decodeHeader } from "../src/sframe.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { FakeFrame } from "./media.ts";
import { FakeClock, type Host, join, makeHost, viewer } from "./harness.ts";

/** A viewer endpoint: its admission result and its receiver key ring. */
interface V {
  peer: string;
  res: ViewerAdmissionResult;
  ring: ReceiverKeyRing;
  dec: FrameDecryptor;
}

async function admit(h: Host, peer: string, clock: FakeClock): Promise<V> {
  const res = await join(h, peer, viewer(h));
  const ring = new ReceiverKeyRing({ now: clock.now });
  for (const e of res.epochs) ring.addEpoch(e);
  for (const a of res.acks) await h.host.receive(peer, a);
  return { peer, res, ring, dec: new FrameDecryptor(ring, { track: 0, kind: "video", codec: "video/VP9" }) };
}

/** Deliver queued REKEYs to a viewer; optionally withhold the ack. */
async function deliver(h: Host, v: V, ack = true): Promise<number> {
  let n = 0;
  for (const m of h.take(v.peer)) {
    expect(peekType(m)).toBe(MsgType.Rekey);
    const { epoch, ack: a } = await v.res.session.handle(m);
    v.ring.addEpoch(epoch);
    if (ack) await h.host.receive(v.peer, a);
    n++;
  }
  return n;
}

async function setup() {
  const h = await makeHost();
  const sring = new SenderKeyRing();
  sring.setEpoch(h.activated.at(-1)!);
  const enc = new FrameEncryptor(sring, { track: 0, kind: "video", codec: "video/VP9" });
  const encrypt = async () => {
    const f = new FakeFrame(randomBytes(200), "video/VP9");
    expect(await enc.encrypt(f)).toBe(true);
    return f;
  };
  const sync = () => {
    const k = h.activated.at(-1)!;
    if (sring.epoch !== k.epoch) sring.setEpoch(k);
  };
  return { h, sring, encrypt, sync };
}

describe("epochs and rekey", () => {
  it("kick: the kicked session gets nothing and cannot decrypt the new epoch; others can", async () => {
    const { h, encrypt, sync } = await setup();
    const a = await admit(h, "a", h.clock);
    const b = await admit(h, "b", h.clock);
    const c = await admit(h, "c", h.clock);
    const e1 = h.host.keys.activeEpoch;
    const f1 = await encrypt();
    for (const v of [a, b, c]) expect(await v.dec.decrypt(f1.clone())).toBe(true);

    await h.host.kick(h.idOf("c"));
    expect(h.take("c")).toEqual([]); // no REKEY for the kicked session
    expect(await deliver(h, a)).toBe(1);
    expect(h.host.keys.activeEpoch).toBe(e1); // still waiting for b's ack
    expect(await deliver(h, b)).toBe(1);
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e1); // all acked → switched
    expect(h.keyframes()).toBe(1);
    sync();

    const f2 = await encrypt();
    expect(await a.dec.decrypt(f2.clone())).toBe(true);
    expect(await b.dec.decrypt(f2.clone())).toBe(true);
    expect(await c.dec.decrypt(f2.clone())).toBe(false);
    expect(c.dec.stats.dropped.unknown_key).toBe(1);
  });

  it("switches after the ack timeout if a session never acks", async () => {
    const { h } = await setup();
    const a = await admit(h, "a", h.clock);
    const b = await admit(h, "b", h.clock);
    const activated: { epoch: number; timedOut: string[] }[] = [];
    h.host.keys.on("activated", (e) => activated.push(e));
    const e1 = h.host.keys.activeEpoch;
    await h.host.keys.rekey("manual");
    await deliver(h, a);
    await deliver(h, b, false);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBe(e1);
    h.clock.advance(1_500);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBe(e1 + 1);
    expect(activated).toEqual([expect.objectContaining({ epoch: e1 + 1, timedOut: [h.idOf("b")] })]);
  });

  it("rekeys every 10 minutes", async () => {
    const { h } = await setup();
    await admit(h, "a", h.clock);
    const e1 = h.host.keys.activeEpoch;
    h.clock.advance(9 * 60_000);
    await h.host.tick();
    expect(h.host.keys.pendingEpoch).toBeNull();
    h.clock.advance(60_000);
    await h.host.tick();
    expect(h.host.keys.pendingEpoch).toBe(e1 + 1);
  });

  it("a kick during a pending rekey supersedes it, so the kicked session's pending key is never used", async () => {
    const { h } = await setup();
    const a = await admit(h, "a", h.clock);
    const c = await admit(h, "c", h.clock);
    await h.host.keys.rekey("manual");
    const leaked = await deliver(h, c, false); // c received the pending epoch
    expect(leaked).toBe(1);
    const pendingSeenByC = c.ring.newest();
    await h.host.kick(h.idOf("c"));
    await deliver(h, a);
    await deliver(h, a);
    const active = h.host.keys.activeEpoch;
    expect(active).toBeGreaterThan(pendingSeenByC!);
    expect(c.ring.epochs()).not.toContain(active);
  });

  it("a viewer admitted during a pending rekey also gets the pending epoch", async () => {
    const { h } = await setup();
    const a = await admit(h, "a", h.clock);
    await h.host.keys.rekey("manual");
    const pending = h.host.keys.pendingEpoch!;
    const late = await admit(h, "late", h.clock); // REKEY may overtake ADMIT: buffered by the viewer
    expect(late.ring.epochs()).toContain(pending);
    await deliver(h, a);
    expect(h.host.keys.activeEpoch).toBe(pending); // late's buffered ack counted
  });

  it("a replayed REKEY is rejected", async () => {
    const { h } = await setup();
    const a = await admit(h, "a", h.clock);
    await h.host.keys.rekey("manual");
    const [m] = h.take("a");
    await a.res.session.handle(m!);
    await expect(a.res.session.handle(m!)).rejects.toMatchObject({ code: "replay" });
  });

  it("receivers keep the old epoch for a grace period after the new one shows up, then drop it", async () => {
    const { h, encrypt, sync } = await setup();
    const a = await admit(h, "a", h.clock);
    const old = await encrypt();
    const oldLate = await encrypt();
    await h.host.keys.rekey("manual");
    await deliver(h, a);
    sync();
    const fresh = await encrypt();
    expect(await a.dec.decrypt(fresh.clone())).toBe(true); // first frame of the new epoch: grace starts
    expect(await a.dec.decrypt(old.clone())).toBe(true); // in-flight old frame still fine
    h.clock.advance(5_000);
    expect(await a.dec.decrypt(oldLate.clone())).toBe(false); // grace over: old keys gone
    expect(a.ring.epochs()).toEqual([h.host.keys.activeEpoch]);
  });

  it("receivers drop an old epoch by the fallback deadline even if no new frame arrives", () => {
    const clock = new FakeClock();
    const ring = new ReceiverKeyRing({ now: clock.now });
    ring.addEpoch({ epoch: 1, secret: randomBytes(32) });
    ring.addEpoch({ epoch: 2, secret: randomBytes(32) });
    clock.advance(29_999);
    ring.prune();
    expect(ring.epochs()).toEqual([1, 2]);
    clock.advance(1);
    ring.prune();
    expect(ring.epochs()).toEqual([2]);
    // A late, older epoch does not linger either.
    ring.addEpoch({ epoch: 1, secret: randomBytes(32) });
    clock.advance(30_000);
    ring.prune();
    expect(ring.epochs()).toEqual([2]);
  });
});

describe("counter and nonce uniqueness", () => {
  it("(KID, CTR) never repeats across tracks, epochs and a sender reload", async () => {
    const { h, sring } = await setup();
    const seen = new Set<string>();
    const encs = [0, 1].map((track) => new FrameEncryptor(sring, { track, kind: track ? "audio" : "video", codec: "video/VP9" }));
    const pump = async (n: number) => {
      for (let i = 0; i < n; i++) {
        for (const e of encs) {
          const f = new FakeFrame(randomBytes(16), e === encs[0] ? "video/VP9" : "audio/opus");
          expect(await e.encrypt(f)).toBe(true);
          const hd = decodeHeader(f.bytes());
          const key = `${hd.kid}:${hd.ctr}`;
          expect(seen.has(key)).toBe(false);
          seen.add(key);
        }
      }
    };
    await pump(50);
    for (let r = 0; r < 3; r++) {
      await h.host.keys.rekey("manual");
      sring.setEpoch(h.activated.at(-1)!);
      await pump(50);
    }
    // Sender reload: a new host over the same storage continues the epoch numbering.
    const before = h.host.keys.activeEpoch;
    const h2 = await makeHost({ storage: h.storage, identity: h.identity });
    expect(h2.host.keys.activeEpoch).toBeGreaterThan(before);
    sring.setEpoch(h2.active());
    await pump(50);
    expect(seen.size).toBe(2 * 50 * 5);
    // Going back to an old epoch would restart counters on a used KID: refused.
    expect(() => sring.setEpoch({ epoch: before, secret: randomBytes(32) })).toThrow(/epoch must increase/);
  });

  it("different epochs give different keys even for the same track", async () => {
    const a = new SenderKeyRing();
    a.setEpoch({ epoch: 1, secret: randomBytes(32) });
    const k1 = await a.key(0);
    a.setEpoch({ epoch: 2, secret: randomBytes(32) });
    const k2 = await a.key(0);
    expect(k1.kid).not.toBe(k2.kid);
    expect(k2.counter).toBe(0n);
  });
});
