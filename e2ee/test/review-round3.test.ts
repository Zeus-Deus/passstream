/**
 * Regression tests for the findings of review round 3 (docs/PROTOCOL.md,
 * "Review round 3 fixes"), ported from the reviewer's reproductions. Each
 * fails against the code before the fixes (e9a0844).
 */
import { describe, expect, it } from "vitest";
import type { ViewerAdmission } from "../src/admission.ts";
import { type Bytes, randomBytes } from "../src/bytes.ts";
import { deriveSessionKeys, type EpochKey, SenderSession } from "../src/channel.ts";
import { FrameEncryptor } from "../src/frame-cryptor.ts";
import { SenderKeyRing } from "../src/keyring.ts";
import { SenderKeyManager } from "../src/keys.ts";
import { type Json, MemoryStorage } from "../src/storage.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { expectReject, FakeClock, type Host, join, makeHost, viewer } from "./harness.ts";
import { FakeFrame } from "./media.ts";

function gate() {
  let open!: () => void;
  let entered!: () => void;
  const released = new Promise<void>((r) => (open = r));
  const reached = new Promise<void>((r) => (entered = r));
  return { released, reached, open, entered };
}

const flush = () => new Promise<void>((r) => setImmediate(r));

/** Reject if `p` does not settle within `ms` of real time (a hang is a failure, not a stuck suite). */
function within<T>(p: Promise<T>, ms = 2_000): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) => {
      const t = setTimeout(() => reject(new Error("blocked")), ms);
      t.unref();
    }),
  ]);
}

/** Make the storage throw on `update` of keys with this prefix, until the returned function is called. */
function failingUpdates(storage: MemoryStorage, prefix: string) {
  let on = true;
  const update = storage.update.bind(storage);
  storage.update = (key, fn) => (on && key.startsWith(prefix) ? Promise.reject(new Error("injected persistence failure")) : update(key, fn));
  return () => {
    on = false;
  };
}

/** Only the durable state, copied into a separate storage: what a crashed sender leaves behind. */
async function snapshot(storage: MemoryStorage): Promise<MemoryStorage> {
  const disk = new MemoryStorage();
  for (const [k, v] of Object.entries(storage.dump())) await disk.update(k, () => JSON.parse(v) as Json);
  return disk;
}

/** A fresh sender on the same durable state (a reload or a crash), with a different PIN. */
async function restart(h: Host, storage: MemoryStorage = h.storage): Promise<Host> {
  const { pinVersion } = await (await import("../src/host.ts")).SenderHost.inspect(storage, "stream-abc123");
  return makeHost({ identity: h.identity, linkSecret: h.linkSecret, storage, pin: "1357", pinVersion });
}

/** HELLO → CHALLENGE → RESPONSE, without delivering the RESPONSE yet. */
async function responseFor(h: Host, peer: string, v: ViewerAdmission): Promise<Bytes> {
  await h.host.receive(peer, v.start());
  const [challenge] = h.take(peer);
  expect(peekType(challenge!)).toBe(MsgType.Challenge);
  const [response] = await v.handle(challenge!);
  return response!;
}

/** A key manager on its own, with a real SenderKeyRing as the encryptor. */
async function manager() {
  const clock = new FakeClock();
  const storage = new MemoryStorage();
  const events: { kind: "activate" | "suspend"; at: number; epoch?: number }[] = [];
  const out: string[] = [];
  const ring = new SenderKeyRing();
  const k = new SenderKeyManager({
    streamId: "recovery",
    storage,
    now: clock.now,
    send: (id) => out.push(id),
    onActivate: (e) => {
      events.push({ kind: "activate", at: clock.t, epoch: e.epoch });
      ring.setEpoch(e);
    },
    onSuspend: () => {
      events.push({ kind: "suspend", at: clock.t });
      ring.suspend();
    },
  });
  await k.init();
  const add = async (id: string) => {
    const s = new SenderSession({ id, viewerSid: randomBytes(16), grantId: randomBytes(16), keys: await deriveSessionKeys(randomBytes(64), randomBytes(64)) });
    await k.admit(s);
    return s;
  };
  return { k, clock, storage, ring, events, out, add };
}

describe("issue 1: a restart never revives a revoked grant", () => {
  it("a kick whose grant-table write failed is still refused after a restart", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = await join(h, "victim", viewer(h, { viewerSid: sid }));
    const id = h.idOf("victim");
    const restore = failingUpdates(h.storage, "grants:");
    await expect(h.host.kick(id)).rejects.toThrow(/persistence/);
    restore();
    await expectReject(join(h, "again", viewer(h, { viewerSid: sid, grant: a.grant })), "grant_invalid");
    // The reconnecting viewer has no PIN, only the stale grant.
    const r = await restart(h);
    await expectReject(join(r, "after-restart", viewer(r, { viewerSid: sid, grant: a.grant })), "grant_invalid");
  });

  it("a crash while the kick's rekey is stalled leaves a durable tombstone", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = await join(h, "victim", viewer(h, { viewerSid: sid }));
    await join(h, "stay", viewer(h));
    const g = gate();
    const seal = SenderSession.prototype.sealRekey;
    SenderSession.prototype.sealRekey = async function (k: EpochKey) {
      g.entered();
      await g.released;
      return seal.call(this, k);
    };
    let kicked: Promise<string[]> | null = null;
    try {
      kicked = h.host.kick(h.idOf("victim"));
      await within(g.reached);
      // Crash here: only what is already in storage survives.
      const r = await restart(h, await snapshot(h.storage));
      await expectReject(join(r, "reconnect", viewer(r, { viewerSid: sid, grant: a.grant })), "grant_invalid");
    } finally {
      g.open();
      SenderSession.prototype.sealRekey = seal;
      await kicked;
    }
  });

  it("if the tombstone cannot be written: revoked in memory, media stopped, every admission refused, retried with backoff", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = await join(h, "victim", viewer(h, { viewerSid: sid }));
    const staySid = randomBytes(16);
    const s = await join(h, "stay", viewer(h, { viewerSid: staySid }));
    const id = h.idOf("victim");
    const changes: boolean[] = [];
    h.host.on("revocationDurability", (e) => changes.push(e.notDurable));
    let failing = true;
    let writes = 0;
    const update = h.storage.update.bind(h.storage);
    h.storage.update = (key, fn) => {
      if (key.startsWith("revocations:")) {
        writes++;
        if (failing) return Promise.reject(new Error("injected persistence failure"));
      }
      return update(key, fn);
    };
    const suspends = h.suspends();
    await expect(h.host.kick(id)).rejects.toThrow(/not durable/);
    expect(h.host.revocationNotDurable).toBe(true);
    expect(changes).toEqual([true]);
    expect(h.host.keys.hasSession(id)).toBe(false);
    expect(h.host.keys.suspended).toBe(true);
    expect(h.suspends()).toBe(suspends + 1);

    // Media stays stopped, even once the kick's fresh epoch is active.
    const delivered = h.activated.length;
    h.clock.advance(300);
    await h.host.tick();
    expect(h.activated.length).toBe(delivered);

    // Nobody is admitted meanwhile: not a new PIN viewer, not a current viewer's grant, not the victim.
    await expectReject(join(h, "new", viewer(h)), "locked");
    await expectReject(join(h, "stay-reload", viewer(h, { viewerSid: staySid, grant: s.grant })), "locked");
    await expect(join(h, "again", viewer(h, { viewerSid: sid, grant: a.grant }))).rejects.toThrow(/rejected/);

    // Retried with backoff, not on every tick.
    const before = writes;
    for (let i = 0; i < 20; i++) {
      h.clock.advance(50);
      await h.host.tick();
    }
    expect(writes - before).toBeGreaterThan(0);
    expect(writes - before).toBeLessThan(10);

    // Storage recovers: durable within the 2 s retry bound, media resumes on a never-delivered epoch.
    failing = false;
    const t0 = h.clock.t;
    while (h.host.revocationNotDurable && h.clock.t - t0 <= 2_000) {
      h.clock.advance(50);
      await h.host.tick();
    }
    expect(h.host.revocationNotDurable).toBe(false);
    expect(changes).toEqual([true, false]);
    expect(h.host.keys.suspended).toBe(false);
    expect(h.activated.length).toBe(delivered + 1);
    expect(h.activated.at(-1)!.epoch).toBe(h.host.keys.activeEpoch);

    await expect(h.host.kick(id)).resolves.toEqual([id]);
    await join(h, "later", viewer(h));
    const r = await restart(h);
    await expectReject(join(r, "after-restart", viewer(r, { viewerSid: sid, grant: a.grant })), "grant_invalid");
  });

  it("PIN rotation bumps a durable grant generation: older grants stay refused after a restart", async () => {
    const rotations: [string, (h: Host, id: string) => Promise<unknown>][] = [
      ["rotatePin", (h) => h.host.rotatePin("9999")],
      ["unlock({ newPin })", (h) => h.host.unlock({ newPin: "9999" })],
      ["kickAndRotatePin", (h, id) => h.host.kickAndRotatePin(id, "9999")],
    ];
    for (const [name, rotate] of rotations) {
      const h = await makeHost();
      const sid = randomBytes(16);
      const a = await join(h, "old", viewer(h, { viewerSid: sid }));
      // Clearing the grant table fails; the generation alone must refuse the old grant.
      const restore = failingUpdates(h.storage, "grants:");
      await expect(rotate(h, h.idOf("old")), name).rejects.toThrow(/persistence/);
      restore();
      const r = await restart(h);
      // Host-control fences also gate grant admission while rotation cleanup is incomplete.
      await expectReject(join(r, "back", viewer(r, { viewerSid: sid, grant: a.grant })), "locked");
      await r.host.rotatePin("1357", { version: r.host.pinVersion });
      await expectReject(join(r, "after-cleanup", viewer(r, { viewerSid: sid, grant: a.grant })), "grant_invalid");
    }
  });

  it("a grant from the current generation still re-admits after a restart", async () => {
    const h = await makeHost();
    await h.host.rotatePin("9999");
    const sid = randomBytes(16);
    const a = await join(h, "v", viewer(h, { viewerSid: sid, pin: "9999" }));
    const r = await restart(h);
    const back = await join(r, "back", viewer(r, { viewerSid: sid, grant: a.grant }));
    expect(back.grant.id).toEqual(a.grant.id);
  });
});

describe("issue 2: an in-flight encryption never emits an old-key frame", () => {
  for (const change of ["suspend", "rekey"] as const) {
    it(`a frame awaiting WebCrypto when the sender ring changes (${change}) is dropped`, async () => {
      const ring = new SenderKeyRing();
      ring.setEpoch({ epoch: 1, secret: randomBytes(32) });
      const enc = new FrameEncryptor(ring, { track: 0, kind: "video", codec: "video/VP9" });
      await ring.key(0);
      const subtle = crypto.subtle;
      const encrypt = subtle.encrypt;
      const g = gate();
      subtle.encrypt = async function (...args: Parameters<SubtleCrypto["encrypt"]>) {
        g.entered();
        await g.released;
        return encrypt.apply(subtle, args);
      };
      try {
        const f = new FakeFrame(randomBytes(80), "video/VP9");
        const plain = f.bytes();
        const p = enc.encrypt(f);
        await within(g.reached);
        if (change === "suspend") ring.suspend();
        else ring.setEpoch({ epoch: 2, secret: randomBytes(32) });
        g.open();
        expect(await p).toBe(false);
        expect(f.bytes()).toEqual(plain);
        expect(enc.stats.ok).toBe(0);
        expect(enc.stats.dropped.key_changed).toBe(1);
      } finally {
        g.open();
        subtle.encrypt = encrypt;
      }
    });
  }
});

describe("issue 3: an epoch a revoked session received is never activated", () => {
  it("an overdue pending epoch already sent to the kicked viewer is discarded, not activated", async () => {
    const h = await manager();
    await h.add("victim");
    const stay = await h.add("stay");
    const g = gate();
    const seal = stay.sealRekey.bind(stay);
    stay.sealRekey = async (key: EpochKey) => {
      g.entered();
      await g.released;
      return seal(key);
    };
    const t0 = h.clock.t;
    const rekey = h.k.rekey();
    await within(g.reached);
    expect(h.out).toContain("victim");
    const stale = h.k.pendingEpoch!;
    const kicked = h.k.kick(["victim"]);
    h.clock.advance(300);
    const cutoffTick = h.k.tick();
    expect(h.k.suspended).toBe(true);
    // The superseded epoch's own 1.5 s deadline passes while the fan-out is still stalled.
    h.clock.advance(1200);
    const lateTick = h.k.tick();
    g.open();
    await rekey;
    await kicked;
    await cutoffTick;
    await lateTick;
    expect(h.events.filter((e) => e.kind === "activate" && e.at > t0)).not.toContainEqual(expect.objectContaining({ epoch: stale }));
    expect(h.k.suspended).toBe(false);
    expect(h.k.activeEpoch).toBeGreaterThan(stale);
    expect(h.ring.epoch).toBe(h.k.activeEpoch);
  });
});

describe("issue 4: suspension always recovers within a bounded time", () => {
  it("abandoning a revocation while recovery is in flight still resumes within 2 s", async () => {
    const h = await manager();
    const release = h.k.beginRevocation();
    h.clock.advance(300);
    const tick = h.k.tick();
    await Promise.resolve();
    release();
    await tick;
    await flush();
    const t0 = h.clock.t;
    while (h.k.suspended && h.clock.t - t0 <= 2_000) {
      h.clock.advance(100);
      await h.k.tick();
    }
    expect(h.k.suspended).toBe(false);
    expect(h.ring.epoch).toBe(h.k.activeEpoch);
  });

  it("a hung epoch allocation times out, and recovery proceeds while it is still unresolved", async () => {
    const h = await manager();
    await h.add("victim");
    await h.add("stay");
    const g = gate();
    const update = h.storage.update.bind(h.storage);
    h.storage.update = async (key, fn) => {
      g.entered();
      await g.released;
      return update(key, fn);
    };
    const kicked = h.k.kick(["victim"]);
    kicked.catch(() => undefined);
    try {
      await within(g.reached);
      // Later storage calls are healthy; the original one never settles.
      h.storage.update = update;
      h.out.length = 0;
      const t0 = h.clock.t;
      const ticks: Promise<void>[] = [];
      while ((h.k.suspended || h.k.activeEpoch === 1) && h.clock.t - t0 <= 3_000) {
        h.clock.advance(50);
        ticks.push(h.k.tick());
        await flush();
      }
      expect(h.k.suspended).toBe(false);
      expect(h.k.activeEpoch).toBeGreaterThan(1);
      expect(h.clock.t - t0).toBeLessThanOrEqual(1_000 + 2_000);
      expect(h.out).not.toContain("victim");
      await expect(kicked).rejects.toThrow(/storage/);
      await Promise.all(ticks);
    } finally {
      g.open();
    }
  });

  it("a persistently failing storage is retried with backoff, at most 2 s apart, and recovers once it heals", async () => {
    const h = await manager();
    await h.add("victim");
    await h.add("stay");
    const calls: number[] = [];
    let failing = true;
    const update = h.storage.update.bind(h.storage);
    h.storage.update = (key, fn) => {
      if (key.startsWith("epoch:")) {
        calls.push(h.clock.t);
        if (failing) return Promise.reject(new Error("injected storage failure"));
      }
      return update(key, fn);
    };
    await expect(h.k.kick(["victim"])).rejects.toThrow(/storage failure/);
    expect(h.k.suspended).toBe(true);
    for (let i = 0; i < 200; i++) {
      h.clock.advance(50);
      await h.k.tick();
    }
    const gaps = calls.slice(1).map((t, i) => t - calls[i]!);
    expect(calls.length).toBeGreaterThan(5);
    expect(calls.length).toBeLessThan(30); // not every 50 ms tick
    expect(Math.max(...gaps)).toBeLessThanOrEqual(2_050);
    expect(h.k.suspended).toBe(true);
    failing = false;
    const t0 = h.clock.t;
    while (h.k.suspended && h.clock.t - t0 <= 2_050) {
      h.clock.advance(50);
      await h.k.tick();
    }
    expect(h.k.suspended).toBe(false);
    expect(h.ring.epoch).toBe(h.k.activeEpoch);
  });

  it("a hung grant write during an admission times out instead of blocking the owner's kick", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    const id = h.idOf("victim");
    const g = gate();
    const update = h.storage.update.bind(h.storage);
    let once = true;
    h.storage.update = async (key, fn) => {
      if (once && key.startsWith("grants:")) {
        once = false;
        g.entered();
        await g.released;
      }
      return update(key, fn);
    };
    try {
      const v = viewer(h);
      const r = await responseFor(h, "pending", v);
      const receiving = h.host.receive("pending", r);
      await within(g.reached);
      const kicked = h.host.kick(id);
      for (let i = 0; i < 30; i++) {
        h.clock.advance(50);
        void h.host.tick();
        await flush();
      }
      await expect(within(kicked)).resolves.toEqual([id]);
      await within(receiving);
      expect(h.take("pending").map((m) => peekType(m))).not.toContain(MsgType.Admit);
    } finally {
      g.open();
    }
  });
});
