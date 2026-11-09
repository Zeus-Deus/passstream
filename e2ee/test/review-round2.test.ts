/**
 * Regression tests for the findings of review round 2 (docs/PROTOCOL.md,
 * "Review round 2 fixes"), ported from the reviewer's reproductions. Each
 * fails against the code before the fixes (294ba5d).
 */
import { describe, expect, it } from "vitest";
import type { ViewerAdmission } from "../src/admission.ts";
import { type Bytes, randomBytes, toBase64Url } from "../src/bytes.ts";
import { deriveSessionKeys, type EpochKey, SenderSession } from "../src/channel.ts";
import { FrameEncryptor } from "../src/frame-cryptor.ts";
import { SenderKeyRing } from "../src/keyring.ts";
import { SenderKeyManager } from "../src/keys.ts";
import { MemoryStorage } from "../src/storage.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { installTransformWorker, type WorkerScopeLike } from "../src/worker-runtime.ts";
import { expectReject, FakeClock, type Host, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";
import { FakeFrame } from "./media.ts";

const types = (msgs: Bytes[]) => msgs.map((m) => peekType(m));

function gate() {
  let open!: () => void;
  let entered!: () => void;
  const released = new Promise<void>((r) => (open = r));
  const reached = new Promise<void>((r) => (entered = r));
  return { released, reached, open, entered };
}

/** HELLO → CHALLENGE → RESPONSE, without delivering the RESPONSE yet. */
async function responseFor(h: Host, peer: string, v: ViewerAdmission): Promise<Bytes> {
  await h.host.receive(peer, v.start());
  const [challenge] = h.take(peer);
  expect(peekType(challenge!)).toBe(MsgType.Challenge);
  const [response] = await v.handle(challenge!);
  return response!;
}

/** Make the storage throw on the next `update` of keys with this prefix, while `on()`. */
function failingUpdates(storage: MemoryStorage, prefix: string) {
  let on = true;
  const update = storage.update.bind(storage);
  storage.update = (key, fn) => (on && key.startsWith(prefix) ? Promise.reject(new Error("injected persistence failure")) : update(key, fn));
  return () => {
    on = false;
  };
}

describe("issue 1: a failed kick stays visible and retryable, and fails closed", () => {
  it("a persistence failure in kickAndRotatePin leaves the victim listed, keyless and kickable", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    await join(h, "stay", viewer(h));
    const id = h.idOf("victim");
    const e0 = h.host.keys.activeEpoch;
    h.take("victim");
    h.take("stay");
    const unlock = h.host.budget.unlock.bind(h.host.budget);
    h.host.budget.unlock = () => Promise.reject(new Error("injected persistence failure"));
    await expect(h.host.kickAndRotatePin(id, "9999")).rejects.toThrow(/persistence/);
    h.host.budget.unlock = unlock;

    // Still listed (so the owner can see and retry it), but already revoked in memory.
    const rec = h.host.viewers().find((v) => v.admissionId === id);
    expect(rec).toMatchObject({ connected: false, kickIncomplete: true });
    expect(h.host.keys.hasSession(id)).toBe(false);
    expect(types(h.take("victim"))).not.toContain(MsgType.Rekey);
    expect(types(h.take("stay"))).toContain(MsgType.Rekey);
    // The switch away from the victim's epoch happens on time regardless.
    h.clock.advance(300);
    await h.host.keys.tick();
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e0);

    // Retrying finishes the action (the same new PIN is allowed to finish a failed rotation).
    const kicked: string[][] = [];
    h.host.on("kicked", (k) => kicked.push(k.peerIds));
    await expect(h.host.kickAndRotatePin(id, "9999")).resolves.toEqual([id]);
    expect(kicked).toEqual([["victim"]]);
    expect(h.host.viewers().map((v) => v.peerId)).toEqual(["stay"]);
    await expectReject(join(h, "back", viewer(h)), "wrong_pin");
    await join(h, "new", viewer(h, { pin: "9999" }));
  });

  it("a failed grant revocation keeps the grant refused in memory and the kick retryable", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = await join(h, "victim", viewer(h, { viewerSid: sid }));
    const id = h.idOf("victim");
    const restore = failingUpdates(h.storage, "grants:");
    await expect(h.host.kick(id)).rejects.toThrow(/persistence/);
    restore();
    expect(h.host.keys.hasSession(id)).toBe(false);
    expect(h.host.viewers()).toMatchObject([{ admissionId: id, kickIncomplete: true }]);
    // The grant table still lists it, but the write-ahead tombstone (round 3) refuses it.
    expect(h.storage.dump()[`grants:${STREAM}`]).toContain(toBase64Url(a.grant.id));
    expect(await h.host.grants.lookup(a.grant.id, sid)).toBeNull();
    await expectReject(join(h, "clone", viewer(h, { viewerSid: sid, grant: a.grant, pin: undefined })), "grant_invalid");
    await expect(h.host.kick(id)).resolves.toEqual([id]);
    expect(await h.host.grants.lookup(a.grant.id, sid)).toBeNull();
    expect(h.host.viewers()).toEqual([]);
  });

  it("a kick whose rekey cannot allocate an epoch stops encryption at once, then recovers", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    const stay = await join(h, "stay", viewer(h));
    const id = h.idOf("victim");
    const e0 = h.host.keys.activeEpoch;
    h.take("victim");
    const restore = failingUpdates(h.storage, "epoch:");
    await expect(h.host.kick(id)).rejects.toThrow(/persistence/);
    // The victim holds the active key and no fresh one exists: stop encrypting now.
    expect(h.suspends()).toBe(1);
    expect(h.host.keys.suspended).toBe(true);
    restore();
    h.clock.advance(300);
    await h.host.keys.tick();
    await h.host.keys.tick();
    // A fresh epoch, which the victim never received, resumes encryption.
    expect(h.host.keys.suspended).toBe(false);
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e0);
    expect(h.active().epoch).toBe(h.host.keys.activeEpoch);
    expect(types(h.take("victim"))).not.toContain(MsgType.Rekey);
    const got = [];
    for (const m of h.take("stay")) if (peekType(m) === MsgType.Rekey) got.push((await stay.session.handle(m)).epoch.epoch);
    expect(got).toContain(h.host.keys.activeEpoch);
    await expect(h.host.kick(id)).resolves.toEqual([id]);
  });
});

describe("issue 2: lock() is rechecked at commit", () => {
  it("locking while the grant is being persisted refuses the admission and revokes the grant", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const v = viewer(h, { viewerSid: sid });
    const response = await responseFor(h, "p", v);
    const g = gate();
    const issue = h.host.grants.issue.bind(h.host.grants);
    let issued: Uint8Array | null = null;
    h.host.grants.issue = async (s) => {
      g.entered();
      await g.released;
      const grant = await issue(s);
      issued = grant.id;
      return grant;
    };
    const receiving = h.host.receive("p", response);
    await g.reached;
    h.host.lock(true);
    g.open();
    await receiving;
    const out = h.take("p");
    expect(types(out)).toEqual([MsgType.Reject]);
    await expect(v.handle(out[0]!)).rejects.toMatchObject({ details: { reason: "locked" } });
    expect(h.host.viewers()).toEqual([]);
    expect(issued).not.toBeNull();
    expect(await h.host.grants.lookup(issued!, sid)).toBeNull();
    h.host.grants.issue = issue;
    h.host.lock(false);
    await join(h, "after", viewer(h));
  });
});

describe("issue 3: an unchanged PIN is not a rotation", () => {
  it("rotatePin and unlock refuse the same PIN, also when only NFC-equivalent", async () => {
    const h = await makeHost({ pin: "é", limits: { perStream: 1 } });
    await expectReject(join(h, "wrong", viewer(h, { pin: "wrong" })), "frozen");
    for (const same of ["é", "é"]) {
      await expect(h.host.unlock({ newPin: same })).rejects.toMatchObject({ code: "invalid_argument" });
      await expect(h.host.rotatePin(same)).rejects.toMatchObject({ code: "invalid_argument" });
    }
    // Nothing changed: still frozen, no rekey.
    expect((await h.host.budget.status()).frozen).toBe(true);
    expect(h.host.keys.pendingEpoch).toBeNull();
    await h.host.unlock({ keepPin: true });
    await join(h, "ok", viewer(h, { pin: "é" }));
  });

  it("kickAndRotatePin with the current PIN is refused before anything changes", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    const id = h.idOf("victim");
    const e0 = h.host.keys.activeEpoch;
    await expect(h.host.kickAndRotatePin(id, PIN)).rejects.toMatchObject({ code: "invalid_argument" });
    await expect(h.host.kickAndRotatePin(id, PIN.normalize("NFD"))).rejects.toMatchObject({ code: "invalid_argument" });
    expect(h.host.viewers()).toMatchObject([{ admissionId: id, connected: true, kickIncomplete: false }]);
    expect(h.host.keys.hasSession(id)).toBe(true);
    expect(h.host.keys.pendingEpoch).toBeNull();
    // The refused action armed no cutoff: nothing is suspended later.
    h.clock.advance(1000);
    await h.host.keys.tick();
    expect(h.suspends()).toBe(0);
    expect(h.host.keys.activeEpoch).toBe(e0);
    // A real rotation still works and keeps the kicked viewer out.
    await h.host.kickAndRotatePin(id, "1357");
    await expectReject(join(h, "back", viewer(h)), "wrong_pin");
  });

  it("a no-PIN stream cannot 'rotate' to no PIN", async () => {
    const h = await makeHost({ pin: null });
    await expect(h.host.rotatePin(null)).rejects.toMatchObject({ code: "invalid_argument" });
    await h.host.rotatePin("2468");
    await join(h, "p", viewer(h, { pin: "2468" }));
  });
});

describe("issue 4: the revocation cutoff does not wait for queued work", () => {
  it("a stalled rekey fan-out cannot hold the switch past the deadline", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    await join(h, "stay", viewer(h));
    const e0 = h.host.keys.activeEpoch;
    const g = gate();
    const seal = SenderSession.prototype.sealRekey;
    let once = true;
    SenderSession.prototype.sealRekey = async function (k: EpochKey) {
      if (once) {
        once = false;
        g.entered();
        await g.released;
      }
      return seal.call(this, k);
    };
    try {
      const kicked = h.host.kick(h.idOf("victim"));
      await g.reached;
      h.clock.advance(300);
      const ticked = h.host.keys.tick();
      // Synchronously: the deadline is enforced before tick() queues anything.
      expect(h.host.keys.activeEpoch).toBeGreaterThan(e0);
      expect(h.suspends()).toBe(0);
      g.open();
      await kicked;
      await ticked;
    } finally {
      g.open();
      SenderSession.prototype.sealRekey = seal;
    }
  });

  it("a backlog of queued kicks switches at the first kick's deadline, never to a kicked session", async () => {
    const clock = new FakeClock();
    const sent = new Map<string, number>();
    const activated: number[] = [];
    let suspends = 0;
    const k = new SenderKeyManager({
      streamId: "load",
      storage: new MemoryStorage(),
      now: clock.now,
      send: (id) => sent.set(id, (sent.get(id) ?? 0) + 1),
      onActivate: (e) => activated.push(e.epoch),
      onSuspend: () => suspends++,
    });
    await k.init();
    for (let i = 0; i < 40; i++) {
      await k.admit(new SenderSession({ id: String(i), viewerSid: randomBytes(16), grantId: randomBytes(16), keys: await deriveSessionKeys(randomBytes(64), randomBytes(64)) }));
    }
    const e0 = k.activeEpoch;
    const g = gate();
    const seal = SenderSession.prototype.sealRekey;
    let once = true;
    SenderSession.prototype.sealRekey = async function (key: EpochKey) {
      if (once) {
        once = false;
        g.entered();
        await g.released;
      }
      return seal.call(this, key);
    };
    try {
      sent.clear();
      const kicks = Array.from({ length: 20 }, (_, i) => k.kick([String(i)]));
      await g.reached;
      clock.advance(299);
      void k.tick();
      expect(k.activeEpoch).toBe(e0);
      clock.advance(1);
      void k.tick();
      expect(k.activeEpoch).toBeGreaterThan(e0);
      expect(suspends).toBe(0);
      g.open();
      await Promise.all(kicks);
      clock.advance(300);
      await k.tick();
      expect(k.pendingEpoch).toBeNull();
    } finally {
      g.open();
      SenderSession.prototype.sealRekey = seal;
    }
    for (let i = 0; i < 20; i++) expect(sent.get(String(i)) ?? 0, `kicked session ${i} got a rekey`).toBe(0);
    expect(sent.get("20")).toBeGreaterThan(0);
  });

  it("a kick stuck behind the host's own queue suspends encryption at its cutoff", async () => {
    const h = await makeHost();
    await join(h, "victim", viewer(h));
    await join(h, "stay", viewer(h));
    const id = h.idOf("victim");
    const e0 = h.host.keys.activeEpoch;
    // A PIN admission's commit holds the host lock while its grant is persisted.
    const g = gate();
    const issue = h.host.grants.issue.bind(h.host.grants);
    h.host.grants.issue = async (s) => {
      g.entered();
      await g.released;
      return issue(s);
    };
    const v = viewer(h);
    const receiving = h.host.receive("slow", await responseFor(h, "slow", v));
    await g.reached;
    h.take("victim");
    const kicked = h.host.kick(id);
    h.clock.advance(300);
    await h.host.keys.tick();
    // The kick has not run, so every epoch may still reach the victim: stop.
    expect(h.suspends()).toBe(1);
    expect(h.host.keys.activeEpoch).toBe(e0);
    g.open();
    await receiving;
    await kicked;
    await h.host.keys.tick();
    // Past its cutoff, the kick's fresh epoch activates at once and resumes encryption.
    expect(h.host.keys.suspended).toBe(false);
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e0);
    expect(types(h.take("victim"))).not.toContain(MsgType.Rekey);
    h.host.grants.issue = issue;
  });

  it("250 sessions and 150 queued kicks: first switch within the 300 ms deadline (real time)", async () => {
    const events: number[] = [];
    const k = new SenderKeyManager({
      streamId: "load",
      storage: new MemoryStorage(),
      send() {},
      now: () => performance.now(),
      onActivate: () => events.push(performance.now()),
      onSuspend() {},
    });
    await k.init();
    for (let i = 0; i < 250; i++) {
      await k.admit(new SenderSession({ id: String(i), viewerSid: randomBytes(16), grantId: randomBytes(16), keys: await deriveSessionKeys(randomBytes(64), randomBytes(64)) }));
    }
    const start = performance.now();
    const ticks: Promise<void>[] = [];
    const timer = setInterval(() => ticks.push(k.tick()), 10);
    try {
      await Promise.all(Array.from({ length: 150 }, (_, i) => k.kick([String(i)])));
      // On a fast machine the backlog may drain before 300 ms; wait the deadline out.
      while (events.length < 2 && performance.now() - start < 1000) await new Promise((r) => setTimeout(r, 5));
    } finally {
      clearInterval(timer);
      await Promise.all(ticks);
    }
    const first = events[1]! - start;
    console.log(`backlog: first activation after ${first.toFixed(1)} ms`);
    expect(first).toBeLessThanOrEqual(350);
  }, 10_000);
});

describe("fail-closed suspension reaches the encryptor", () => {
  it("SenderKeyRing.suspend drops frames until a newer epoch; the worker handles sender-suspend", async () => {
    const posted: unknown[] = [];
    const scope: WorkerScopeLike = { onrtctransform: null, onmessage: null, postMessage: (m) => posted.push(m) };
    const w = installTransformWorker(scope);
    const ring = w.senderRing;
    const enc = new FrameEncryptor(ring, { track: 0, kind: "video", codec: "video/VP9" });
    scope.onmessage!({ data: { type: "sender-epoch", epoch: 5, secret: randomBytes(32) } });
    expect(await enc.encrypt(new FakeFrame(randomBytes(40), "video/VP9"))).toBe(true);
    scope.onmessage!({ data: { type: "sender-suspend" } });
    expect(ring.epoch).toBeNull();
    expect(await enc.encrypt(new FakeFrame(randomBytes(40), "video/VP9"))).toBe(false);
    expect(enc.stats.dropped.unknown_key).toBe(1);
    // Resuming needs a newer epoch: the suspended one (or older) is refused.
    expect(() => ring.setEpoch({ epoch: 5, secret: randomBytes(32) })).toThrow();
    scope.onmessage!({ data: { type: "sender-epoch", epoch: 6, secret: randomBytes(32) } });
    expect(await enc.encrypt(new FakeFrame(randomBytes(40), "video/VP9"))).toBe(true);
    expect(posted).toEqual([]);
  });
});
