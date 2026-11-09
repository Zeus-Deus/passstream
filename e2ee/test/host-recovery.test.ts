import { describe, expect, it } from "vitest";
import type { Bytes } from "../src/bytes.ts";
import { SenderHost } from "../src/host.ts";
import { HostRecovery, CLEAR_LATCHES, generatePin, mergeLatches, reconcilePin, type RecoveryRecord, type RecoveryStatus, validatePinPolicy } from "../src/recovery.ts";
import { type Json, MemoryStorage } from "../src/storage.ts";
import { expectReject, type Host, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";

async function recover(base?: Host, opts: { storage?: MemoryStorage; init?: { knock: boolean; latches: typeof CLEAR_LATCHES }; initial?: boolean; onLatches?: (l: typeof CLEAR_LATCHES) => void; storageTimeoutMs?: number; onStateChange?: (s: RecoveryStatus) => void; onMismatch?: () => void } = {}) {
  const h = base ?? await makeHost();
  const storage = opts.storage ?? h.storage;
  const out: [string, Bytes][] = [];
  const r = await HostRecovery.create({
    storage, streamId: STREAM,
    ...(opts.initial === false ? {} : { initial: { pin: PIN, policy: { kind: "digits" as const, length: 4 } } }),
    init: opts.init,
    onLatches: opts.onLatches,
    onStateChange: opts.onStateChange,
    onMismatch: opts.onMismatch,
    host: { storageTimeoutMs: opts.storageTimeoutMs, identity: h.identity, linkSecret: h.linkSecret, now: h.clock.now, onEpoch: (k) => h.activated.push(k), onSuspend: () => {}, send: (peer, b) => out.push([peer, b]) },
  });
  const wrapped: Host = { ...h, host: r.host, storage,
    take(peer) { const found = out.filter(([p]) => p === peer).map(([, b]) => b); for (let i = out.length - 1; i >= 0; i--) if (out[i]![0] === peer) out.splice(i, 1); return found; },
    idOf(peer) { return r.host.viewers().find((v) => v.peerId === peer && v.connected)!.admissionId; },
  };
  return { r, h: wrapped };
}

async function snapshot(storage: MemoryStorage): Promise<MemoryStorage> {
  const copy = new MemoryStorage();
  for (const [key, raw] of Object.entries(storage.dump())) await copy.update(key, () => JSON.parse(raw) as Json);
  return copy;
}

function failWrites(storage: MemoryStorage, prefix: string) {
  const original = storage.update.bind(storage);
  let fail = true;
  storage.update = (key, fn) => fail && key.startsWith(prefix) ? Promise.reject(new Error("injected storage failure")) : original(key, fn);
  return () => { fail = false; };
}

const initialRecord = (): RecoveryRecord => ({ pin_state: { version: 0, policy: { kind: "digits", length: 4 }, pin: PIN }, pending_rotation: null, knock: false, latches: { ...CLEAR_LATCHES } });

describe("startup reconciliation table", () => {
  it.each([
    ["no pending, same complete version", false, 0, true, "agree"],
    ["bump never landed", true, 0, true, "apply_old"],
    ["bump landed, incomplete cleanup", true, 1, false, "apply_pending"],
    ["only page commit missing", true, 1, true, "finalize"],
    ["missing intent for incomplete rotation", false, 0, false, "mismatch"],
    ["library ahead without an intent", false, 1, true, "mismatch"],
    ["library ahead of intent", true, 2, true, "mismatch"],
    ["library corrupt", true, -1, false, "mismatch"],
    ["old fence incomplete", true, 0, false, "mismatch"],
  ])("%s", (_name, pending, pinVersion, rotationComplete, expected) => {
    const r = initialRecord();
    if (pending) r.pending_rotation = { version: 1, pin: "1357", policy: { kind: "digits", length: 4 }, op: "rotate" };
    expect(reconcilePin(r, { pinVersion: Number(pinVersion), rotationComplete: Boolean(rotationComplete), frozen: false })).toBe(expected);
  });

  it("refuses all admissions when create's pinVersion disagrees with the fence", async () => {
    const h = await makeHost({ pinVersion: 10 });
    await expectReject(join(h, "a", viewer(h)), "locked");
    expect(await SenderHost.inspect(h.storage, STREAM)).toMatchObject({ pinVersion: 0, rotationComplete: true });
  });

  it.each(["rotate", "unlock", "kick"] as const)("simulates a crash between EVERY persisted step of %s", async (op) => {
    const { r, h } = await recover();
    // Retain a grant and a failed guess so neither revocation nor budget checks are vacuous.
    const sid = new Uint8Array(16).fill(3);
    const grant = (await join(h, "first", viewer(h, { viewerSid: sid }))).grant;
    const reservation = await r.host.budget.reserve("wrong");
    if (reservation.ok) await r.host.budget.settle(reservation.ticket, "wrong_pin");
    const crashes: MemoryStorage[] = [await snapshot(h.storage)];
    const update = h.storage.update.bind(h.storage);
    const steps: string[] = [];
    h.storage.update = async (key, fn) => {
      const result = await update(key, fn);
      steps.push(key);
      crashes.push(await snapshot(h.storage));
      return result;
    };
    await r.rotate({ policy: { kind: "passphrase" }, value: "replacement passphrase", op, admissionId: op === "kick" ? h.idOf("first") : undefined });
    h.storage.update = update;
    expect(steps.filter((s) => s.startsWith("revocations:"))).toHaveLength(2);
    expect(steps).toContain(`budget:${STREAM}`);
    expect(steps).toContain(`grants:${STREAM}`);
    expect(steps.filter((s) => s === `ps:stream:${STREAM}`)).toHaveLength(2);
    for (let i = 0; i < crashes.length; i++) {
      const { r: reboot, h: next } = await recover(h, { storage: crashes[i]!, initial: false });
      expect(reboot.state, `crash ${i}: ${steps[i - 1] ?? "before intent"}`).toBe("idle");
      const beforeIntent = i === 0;
      expect(reboot.record.pin_state?.pin).toBe(beforeIntent ? PIN : "replacement passphrase");
      expect(reboot.record.pending_rotation).toBeNull();
      expect(await SenderHost.inspect(next.storage, STREAM)).toMatchObject({ pinVersion: beforeIntent ? 0 : 1, rotationComplete: true });
      if (!beforeIntent) {
        expect((await reboot.host.budget.status()).streamFailures).toBe(0);
        await expectReject(join(next, `old-${i}`, viewer(next, { viewerSid: sid, grant })), "grant_invalid");
        await join(next, `new-${i}`, viewer(next, { pin: "replacement passphrase" }));
      }
    }
  });

  it("an incomplete fence blocks valid grant reconnects until cleanup finishes", async () => {
    const h = await makeHost();
    const sid = new Uint8Array(16).fill(7);
    const g = (await join(h, "a", viewer(h, { viewerSid: sid }))).grant;
    const restore = failWrites(h.storage, "budget:");
    await expect(h.host.rotatePin("1357", { version: 1 })).rejects.toMatchObject({ code: "storage" });
    expect(await SenderHost.inspect(h.storage, STREAM)).toMatchObject({ pinVersion: 1, rotationComplete: false });
    restore();
    const restart = await makeHost({ storage: await snapshot(h.storage), pin: "1357", pinVersion: 1 });
    await expectReject(join(restart, "grant", viewer(restart, { viewerSid: sid, grant: g })), "locked");
    await expectReject(join(restart, "pin", viewer(restart, { pin: "1357" })), "locked");
    await restart.host.rotatePin("1357", { version: 1 });
    expect(await SenderHost.inspect(restart.storage, STREAM)).toMatchObject({ pinVersion: 1, rotationComplete: true });
  });
});

describe("PIN rotation state machine (CONTRACT §5.4)", () => {
  it("invalid policy, generated-value misuse, missing/short passphrase and unchanged PIN are terminal with no writes", async () => {
    const { r, h } = await recover();
    const before = h.storage.dump();
    await expect(r.rotate({ policy: { kind: "digits", length: 3 } })).rejects.toMatchObject({ code: "invalid_argument" });
    await expect(r.rotate({ policy: { kind: "digits", length: 4 }, value: "1234" })).rejects.toMatchObject({ code: "invalid_argument" });
    await expect(r.rotate({ policy: { kind: "passphrase" } })).rejects.toMatchObject({ code: "passphrase_required" });
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "short" })).rejects.toMatchObject({ code: "invalid_argument" });
    // Same-value policy transitions are also refused.
    const pin = r.record.pin_state?.pin!;
    expect(pin).toBe(PIN);
    await expect(r.host.rotatePin(pin, { version: 1 })).rejects.toMatchObject({ code: "invalid_argument" });
    expect(h.storage.dump()).toEqual(before);
    expect(r.state).toBe("idle");
  });

  it("normalizes passphrases and rejects unchanged none → none without an intent", async () => {
    const { r, h } = await recover();
    await r.rotate({ policy: { kind: "passphrase" }, value: "Cafe\u0301 secret" });
    expect(r.record.pin_state?.pin).toBe("Café secret");
    const before = h.storage.dump();
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "Café secret" })).rejects.toMatchObject({ code: "invalid_argument" });
    expect(h.storage.dump()).toEqual(before);
    await r.rotate({ policy: { kind: "none" } });
    const none = h.storage.dump();
    await expect(r.rotate({ policy: { kind: "none" } })).rejects.toMatchObject({ code: "invalid_argument" });
    expect(h.storage.dump()).toEqual(none);
  });

  it("idle → persisted intent → applying → committing → idle keeps the version and policy together", async () => {
    const { r, h } = await recover();
    const states: string[] = [];
    const update = h.storage.update.bind(h.storage);
    h.storage.update = async (key, fn) => { states.push(r.state); return update(key, fn); };
    await r.rotate({ policy: { kind: "passphrase" }, value: "another secret" });
    expect(states).toContain("intent_persisted");
    expect(states).toContain("applying");
    expect(states).toContain("committing");
    expect(r.state).toBe("idle");
    expect(r.reconciling).toBe(false);
    expect(r.record.pin_state).toEqual({ version: 1, policy: { kind: "passphrase" }, pin: "another secret" });
    expect(r.record.pending_rotation).toBeNull();
  });

  for (const prefix of ["ps:stream:", "revocations:", "grants:", "budget:", "epoch:"]) {
    it(`storage failure at ${prefix} is accepted, blocks replacements, retries the SAME value/version and completes`, async () => {
      const { r, h } = await recover();
      const restore = failWrites(h.storage, prefix);
      await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "storage", details: { pending: true } });
      expect(r.state).toBe("retrying");
      expect(r.reconciling).toBe(true);
      const intent = r.record.pending_rotation;
      await expect(r.rotate({ policy: { kind: "none" } })).rejects.toMatchObject({ code: "rotation_pending" });
      expect(r.record.pending_rotation).toEqual(intent);
      await expectReject(join(h, "blocked", viewer(h)), "locked");
      restore();
      for (let n = 0; n < 8 && r.state !== "idle"; n++) { h.clock.advance(2000); await r.tick(); }
      expect(r.state).toBe("idle");
      expect(r.record.pin_state?.version).toBe(intent!.version);
      expect(r.record.pin_state?.pin).toBe(intent!.pin);
      expect((await SenderHost.inspect(h.storage, STREAM)).rotationComplete).toBe(true);
      await join(h, "new", viewer(h, { pin: "replacement secret" }));
    });
  }

  it("a first-call non-storage error clears the persisted intent and reconciling", async () => {
    const { r, h } = await recover();
    r.host.rotatePin = async () => { throw new (await import("../src/errors.ts")).E2EEError("invalid_argument", "terminal"); };
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "invalid_argument" });
    expect(r.state).toBe("idle");
    expect(r.record.pending_rotation).toBeNull();
    expect((await h.storage.get(`ps:stream:${STREAM}`))).toMatchObject({ pending_rotation: null });
  });

  it("a non-storage error with an advanced incomplete fence retains the intent for retry", async () => {
    const { r, h } = await recover();
    const restore = failWrites(h.storage, "grants:");
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "storage" });
    restore();
    const rotate = r.host.rotatePin.bind(r.host);
    r.host.rotatePin = async () => { throw new (await import("../src/errors.ts")).E2EEError("invalid_argument", "retry disagrees"); };
    h.clock.advance(100);
    await r.tick();
    expect(r.state).toBe("retrying");
    expect(r.record.pending_rotation?.version).toBe(1);
    await expectReject(join(h, "blocked", viewer(h)), "locked");
    r.host.rotatePin = rotate;
    h.clock.advance(2000);
    await r.tick();
    expect(r.state).toBe("idle");
    expect(r.record.pin_state?.version).toBe(1);
  });

  it("failed terminal-intent cleanup retries the clear without calling the library again", async () => {
    const { r, h } = await recover();
    const update = h.storage.update.bind(h.storage);
    let writes = 0;
    h.storage.update = (key, fn) => key === `ps:stream:${STREAM}` && ++writes === 2 ? Promise.reject(new Error("cleanup failed")) : update(key, fn);
    let calls = 0;
    r.host.rotatePin = async () => { calls++; throw new (await import("../src/errors.ts")).E2EEError("invalid_argument", "terminal"); };
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "storage", details: { pending: true } });
    expect(r.state).toBe("retrying");
    h.clock.advance(100); await r.tick();
    expect(r.state).toBe("idle");
    expect(r.record.pending_rotation).toBeNull();
    expect(calls).toBe(1);
  });

  for (const reload of [false, true]) {
    it(`a timed-out page write landing after ${reload ? "reload" : "completion"} cannot restore an old intent`, async () => {
      const { r, h } = await recover(undefined, { storageTimeoutMs: 10 });
      const update = h.storage.update.bind(h.storage);
      let release!: () => void;
      const held = new Promise<void>((resolve) => { release = resolve; });
      const late: { promise?: Promise<Json> } = {};
      h.storage.update = (key, fn) => {
        if (key.startsWith("ps:stream:") && !late.promise) {
          late.promise = held.then(() => update(key, fn));
          return late.promise;
        }
        return update(key, fn);
      };
      await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ details: { pending: true } });
      h.storage.update = update;
      if (reload) await recover(h, { initial: false });
      else { h.clock.advance(100); await r.tick(); expect(r.state).toBe("idle"); }
      const before = h.storage.dump()[`ps:stream:${STREAM}`];
      release();
      await late.promise?.catch(() => undefined);
      expect(h.storage.dump()[`ps:stream:${STREAM}`]).toBe(before);
    });
  }

  it("restarts safely during a mismatch-repair rotation whose version skips ahead", async () => {
    const { r, h } = await recover();
    await r.host.rotatePin("1357", { version: 3 });
    // Page v0 and library v3 disagree.
    const reboot = await recover(h, { initial: false });
    expect(reboot.r.state).toBe("mismatch");
    const restore = failWrites(h.storage, "grants:");
    await expect(reboot.r.rotate({ policy: { kind: "passphrase" }, value: "fresh repair secret" })).rejects.toMatchObject({ details: { pending: true } });
    expect(reboot.r.record.pending_rotation?.version).toBe(4);
    restore();
    const afterCrash = await recover(h, { initial: false });
    expect(afterCrash.r.state).toBe("idle");
    expect(afterCrash.r.record.pin_state?.version).toBe(4);
    expect(afterCrash.r.record.latches).toMatchObject({ locked: true, frozen: true });
  });

  it("a final page write failure retries finalization without resetting the budget again", async () => {
    const { r, h } = await recover();
    let pageWrites = 0;
    let budgetResets = 0;
    r.host.budget.on("unlocked", () => budgetResets++);
    const update = h.storage.update.bind(h.storage);
    h.storage.update = (key, fn) => {
      if (key === `ps:stream:${STREAM}` && ++pageWrites === 2) return Promise.reject(new Error("final write failed"));
      return update(key, fn);
    };
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ details: { pending: true } });
    const before = budgetResets;
    h.clock.advance(100);
    await r.tick();
    expect(r.state).toBe("idle");
    expect(budgetResets).toBe(before);
    expect(r.record.pin_state?.version).toBe(1);
  });

  it("concurrent library rotation requests are retryable without queuing a second change", async () => {
    const h = await makeHost();
    const first = h.host.rotatePin("1357", { version: 1 });
    await expect(h.host.rotatePin("2468", { version: 2 })).rejects.toMatchObject({ code: "rotation_pending" });
    await first;
    expect(h.host.pinVersion).toBe(1);
  });

  it("a stale version on a mismatched host is refused BEFORE any library write", async () => {
    const h = await makeHost();
    await h.host.rotatePin("1357", { version: 5 });
    const restart = await makeHost({ storage: h.storage, pinVersion: 0 });
    const before = h.storage.dump();
    await expect(restart.host.rotatePin("2468", { version: 1 })).rejects.toMatchObject({ code: "invalid_argument" });
    expect(h.storage.dump()).toEqual(before);
    await expect(restart.host.unlock({ keepPin: true })).rejects.toMatchObject({ code: "rotation_pending" });
  });

  it("explicit version retries don't bump the grant generation twice", async () => {
    const h = await makeHost({ pinVersion: 0 });
    const restore = failWrites(h.storage, "budget:");
    await expect(h.host.rotatePin("1357", { version: 1 })).rejects.toMatchObject({ code: "storage" });
    const fence = await h.storage.get(`revocations:${STREAM}`);
    await expect(h.host.rotatePin("2468", { version: 2 })).rejects.toMatchObject({ code: "rotation_pending" });
    restore();
    await h.host.rotatePin("1357", { version: 1 });
    const final = await h.storage.get(`revocations:${STREAM}`);
    expect(final).toMatchObject({ gen: (fence as { gen: number }).gen, pin_version: 1, rotation_complete: true });
  });
});

describe("persisted privacy latches and knock mode", () => {
  it("OR-merges each latch and knock before host creation; reload cannot clear them", async () => {
    const initial = await recover(undefined, { init: { knock: true, latches: { locked: true, paused: true, frozen: true } } });
    const { r, h } = await recover(initial.h, { initial: false, init: { knock: false, latches: { ...CLEAR_LATCHES } } });
    expect(r.record).toMatchObject({ knock: true, latches: { locked: true, paused: true, frozen: true } });
    await expectReject(join(h, "new", viewer(h)), "locked");
    expect(mergeLatches({ locked: true, paused: false, frozen: false }, { locked: false, paused: true, frozen: true })).toEqual({ locked: true, paused: true, frozen: true });
  });

  it("restores a durable budget freeze before admitting, even when the page latch is false", async () => {
    const { r, h } = await recover();
    for (let i = 0; i < 20; i++) {
      h.clock.advance(r.host.budget.limits.paceMaxMs); // stream-wide pacing (PROTOCOL §6)
      const ticket = await r.host.budget.reserve(`wrong-${i}`);
      if (ticket.ok) await r.host.budget.settle(ticket.ticket, "wrong_pin");
    }
    const { r: reboot, h: next } = await recover(h, { initial: false });
    expect(reboot.record.latches.frozen).toBe(true);
    await expectReject(join(next, "new", viewer(next)), "locked");
  });

  it("sets a latch in memory before writing; failed storage is applied, retried, and survives reload", async () => {
    const seen: boolean[] = [];
    const { r, h } = await recover(undefined, { onLatches: (l) => seen.push(l.paused) });
    const restore = failWrites(h.storage, "ps:stream:");
    await expect(r.setLatch("paused", true)).rejects.toMatchObject({ code: "storage", details: { applied: true } });
    expect(seen.at(-1)).toBe(true);
    expect(r.record.latches.paused).toBe(true);
    restore(); await r.tick();
    const next = await recover(h, { initial: false });
    expect(next.r.record.latches.paused).toBe(true);
  });

  it("persists a clear before applying; a failed clear leaves the latch applied", async () => {
    const seen: boolean[] = [];
    const { r, h } = await recover(undefined, { onLatches: (l) => seen.push(l.locked) });
    await r.setLatch("locked", true);
    const restore = failWrites(h.storage, "ps:stream:");
    await expect(r.setLatch("locked", false)).rejects.toMatchObject({ code: "storage" });
    expect(seen.at(-1)).toBe(true);
    expect(r.record.latches.locked).toBe(true);
    restore(); await r.setLatch("locked", false);
    expect(seen.at(-1)).toBe(false);
  });

  it("a knock policy mismatch locks new admissions but retains current viewers' grants", async () => {
    const { r, h } = await recover();
    const sid = new Uint8Array(16).fill(8);
    const { grant } = await join(h, "current", viewer(h, { viewerSid: sid }));
    await r.mergeInit({ knock: true, latches: { ...CLEAR_LATCHES } });
    expect(r.state).toBe("idle");
    expect(r.knockMismatch).toBe(true);
    expect(r.reconciling).toBe(false);
    expect(r.record.latches.frozen).toBe(false);
    await join(h, "current-back", viewer(h, { viewerSid: sid, grant }));
    await expect(r.setLatch("locked", false)).rejects.toMatchObject({ code: "state_mismatch" });
    expect(r.record.knock).toBe(true);
    await expectReject(join(h, "new", viewer(h)), "locked");
  });

  it("unfreeze rotates by default, clears frozen durably, and preserves lock/pause", async () => {
    const { r, h } = await recover();
    await r.setLatch("frozen", true);
    await r.setLatch("locked", true);
    await r.setLatch("paused", true);
    await r.unfreeze({ policy: { kind: "digits", length: 4 } });
    expect(r.record.pin_state?.pin).not.toBe(PIN);
    expect(r.record.latches).toEqual({ locked: true, paused: true, frozen: false });
    const reboot = await recover(h, { initial: false });
    expect(reboot.r.record.latches).toEqual(r.record.latches);
  });

  it("unfreeze keepPin is explicit and cannot clear reconciliation", async () => {
    const { r } = await recover();
    await r.setLatch("frozen", true);
    await r.unfreeze({ keepPin: true });
    expect(r.record.pin_state?.pin).toBe(PIN);
    expect(r.record.pin_state?.version).toBe(0);
    expect(r.record.latches.frozen).toBe(false);
    await r.mergeInit({ knock: true, latches: { ...CLEAR_LATCHES } });
    await expect(r.unfreeze({ keepPin: true })).rejects.toMatchObject({ code: "rotation_pending" });
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "new local secret" })).rejects.toMatchObject({ code: "state_mismatch" });
  });

  it("generates digit PINs to the selected length and never reuses the current PIN", () => {
    for (const length of [4, 6, 12]) {
      const pin = generatePin(length, "0".repeat(length));
      expect(pin).toMatch(new RegExp(`^[0-9]{${length}}$`));
      expect(pin).not.toBe("0".repeat(length));
      validatePinPolicy({ kind: "digits", length }, pin);
    }
  });
});


describe("host-control review round 1 regressions", () => {
  // Ported from the review's zz-review reproduction A, with durable assertions.
  it.each(["paused", "frozen"] as const)("a failed clear preserves a concurrent %s latch", async (name) => {
    const seen: typeof CLEAR_LATCHES[] = [];
    const { r, h } = await recover(undefined, { onLatches: (l) => seen.push(l) });
    await r.setLatch("locked", true);
    const update = h.storage.update.bind(h.storage);
    let reject!: (e: Error) => void;
    let armed = true;
    h.storage.update = (key, fn) => {
      if (armed && key.startsWith("ps:stream:")) { armed = false; return new Promise((_, no) => { reject = no; }); }
      return update(key, fn);
    };
    const clearing = r.setLatch("locked", false);
    const failed = expect(clearing).rejects.toMatchObject({ code: "storage" });
    await r.setLatch(name, true);
    reject(new Error("clear failed"));
    await failed;
    expect(r.record.latches).toMatchObject({ locked: true, [name]: true });
    expect(seen.at(-1)).toMatchObject({ locked: true, [name]: true });
    await r.tick();
    expect(await h.storage.get(`ps:stream:${STREAM}`)).toMatchObject({ latches: { locked: true, [name]: true } });
  });

  // Review reproductions C and C2: two reloads must never turn corruption into no PIN.
  it("persists corruption as mismatch through repeated reloads until a fresh rotation", async () => {
    const { h } = await recover();
    await h.storage.update(`ps:stream:${STREAM}`, (cur) => ({ ...(cur as Record<string, Json>), latches: "garbage" }));
    for (let i = 0; i < 3; i++) {
      const { r, h: next } = await recover(h, { initial: false });
      expect(r.state).toBe("mismatch");
      expect(await h.storage.get(`ps:stream:${STREAM}`)).toMatchObject({ pin_state: null, pin_mismatch: true });
      await expect(r.unfreeze({ keepPin: true })).rejects.toMatchObject({ code: "rotation_pending" });
      await expect(r.setLatch("locked", false)).rejects.toMatchObject({ code: "rotation_pending" });
      await expectReject(join(next, `no-pin-${i}`, viewer(next, { pin: null })), "locked");
    }
    const { r } = await recover(h, { initial: false });
    await r.rotate({ policy: { kind: "passphrase" }, value: "fresh repair secret" });
    const next = await recover(h, { initial: false });
    expect(next.r.state).toBe("idle");
    expect(next.r.record.pin_state?.pin).toBe("fresh repair secret");
  });

  it("recovers a crash during rotation that repairs a corrupt page record", async () => {
    const { h } = await recover();
    await h.storage.update(`ps:stream:${STREAM}`, () => ({ corrupt: true }));
    const { r } = await recover(h, { initial: false });
    const restore = failWrites(h.storage, "grants:");
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "fresh repair secret" })).rejects.toMatchObject({ code: "storage" });
    restore();
    const reboot = await recover(h, { storage: await snapshot(h.storage), initial: false });
    expect(reboot.r.state).toBe("idle");
    expect(reboot.r.record.pin_state?.pin).toBe("fresh repair secret");
    expect(reboot.r.record.pin_mismatch).toBe(false);
  });

  it.each([false, true])("recovers a post-apply non-storage error (rotation complete=%s)", async (complete) => {
    const { r, h } = await recover();
    const rotate = r.host.rotatePin.bind(r.host);
    r.host.rotatePin = async (pin, opts) => {
      if (complete) await rotate(pin, opts);
      else {
        const restore = failWrites(h.storage, "grants:");
        try { await rotate(pin, opts); } catch { /* fence advanced, cleanup incomplete */ }
        restore();
      }
      throw new (await import("../src/errors.ts")).E2EEError("invalid_argument", "post-apply error");
    };
    try { await r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" }); } catch (e) {
      expect(e).toMatchObject({ code: "storage", details: { pending: true } });
    }
    r.host.rotatePin = rotate;
    if (r.state !== "idle") { h.clock.advance(2000); await r.tick(); }
    expect(r.state).toBe("idle");
    expect(r.record.pending_rotation).toBeNull();
    expect(r.record.pin_state?.pin).toBe("replacement secret");
    expect((await recover(h, { initial: false })).r.record.pin_state?.pin).toBe("replacement secret");
  });

  it("retries when the fence completed but the library call has not finished in memory", async () => {
    const { r, h } = await recover();
    const complete = r.host.grants.completeRotation.bind(r.host.grants);
    let fail = true;
    r.host.grants.completeRotation = async (version) => {
      await complete(version);
      if (fail) { fail = false; throw new (await import("../src/errors.ts")).E2EEError("invalid_argument", "error after durable completion"); }
    };
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "storage", details: { pending: true } });
    expect(r.state).toBe("retrying");
    expect(r.record.pending_rotation?.version).toBe(1);
    expect(await SenderHost.inspect(h.storage, STREAM)).toMatchObject({ pinVersion: 1, rotationComplete: true });
    h.clock.advance(2000); await r.tick();
    expect(r.state).toBe("idle");
    expect(r.record.pin_state?.pin).toBe("replacement secret");
    await join(h, "new", viewer(h, { pin: "replacement secret" }));
  });

  it("rejects generic frozen clears and latch clears while reconciling", async () => {
    const { r, h } = await recover();
    await r.setLatch("frozen", true);
    await expect(r.setLatch("frozen", false)).rejects.toMatchObject({ code: "invalid_argument" });
    await r.host.rotatePin("1357", { version: 2 });
    const reboot = await recover(h, { initial: false });
    for (const latch of ["locked", "paused"] as const) await expect(reboot.r.setLatch(latch, false)).rejects.toMatchObject({ code: "rotation_pending" });
  });
});


describe("recovery app integration", () => {
  it("rotates using an options object and publishes every transition through callback and event", async () => {
    const callbacks: RecoveryStatus[] = [];
    const { r } = await recover(undefined, { onStateChange: (s) => callbacks.push(s) });
    expect(callbacks).toEqual([{ state: "idle", reconciling: false, knockMismatch: false }]);
    const events: RecoveryStatus[] = [];
    const unsubscribe = r.on("stateChanged", (s) => events.push(s));
    await r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" });
    expect(events.map((s) => s.state)).toEqual(["intent_persisted", "applying", "committing", "idle"]);
    expect(callbacks.slice(1)).toEqual(events);
    expect(events.map((s) => s.reconciling)).toEqual([true, true, true, false]);
    unsubscribe();
    await r.rotate({ policy: { kind: "none" } });
    expect(events).toHaveLength(4);
  });

  it("reports retrying and mismatch without requiring polling", async () => {
    const states: string[] = [];
    const { r, h } = await recover(undefined, { onStateChange: (s) => states.push(s.state) });
    const restore = failWrites(h.storage, "ps:stream:");
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "storage" });
    expect(states.at(-1)).toBe("retrying");
    await expect(r.setLatch("paused", false)).rejects.toMatchObject({ code: "rotation_pending" });
    restore();
    h.clock.advance(2000); await r.tick();
    expect(states.at(-1)).toBe("idle");
    await r.host.rotatePin("1357", { version: 3 });
    const reboot = await recover(h, { initial: false, onStateChange: (s) => states.push(s.state) });
    expect(states.at(-1)).toBe("mismatch");
    expect(reboot.r.reconciling).toBe(true);
  });

  it("finishes a rotation consistently when init adds knock during the library call", async () => {
    const states: RecoveryStatus[] = [];
    let alerts = 0;
    const { r } = await recover(undefined, { onStateChange: (s) => states.push(s), onMismatch: () => alerts++ });
    const rotate = r.host.rotatePin.bind(r.host);
    r.host.rotatePin = async (pin, opts) => {
      await r.mergeInit({ knock: true, latches: { ...CLEAR_LATCHES } });
      return rotate(pin, opts);
    };
    await r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" });
    expect(alerts).toBe(1);
    expect(r.state).toBe("idle");
    expect(r.reconciling).toBe(false);
    expect(r.knockMismatch).toBe(true);
    expect(r.record.latches).toEqual({ locked: true, paused: false, frozen: false });
    expect(states.at(-1)).toEqual({ state: "idle", reconciling: false, knockMismatch: true });
    await expect(r.setLatch("locked", false)).rejects.toMatchObject({ code: "state_mismatch" });
  });

  it("a failed clear cannot undo mismatch latches raised while it waits", async () => {
    const { r, h } = await recover();
    await r.setLatch("paused", true);
    const update = h.storage.update.bind(h.storage);
    let reject!: (e: Error) => void;
    let armed = true;
    h.storage.update = (key, fn) => {
      if (armed && key.startsWith("ps:stream:")) { armed = false; return new Promise((_, no) => { reject = no; }); }
      return update(key, fn);
    };
    const clear = r.setLatch("paused", false);
    const assertion = expect(clear).rejects.toMatchObject({ code: "storage" });
    const rotate = r.host.rotatePin.bind(r.host);
    r.host.rotatePin = async (pin, opts) => {
      await rotate(pin, { ...opts, version: 4 });
      throw new (await import("../src/errors.ts")).E2EEError("state_mismatch", "another writer advanced the fence");
    };
    await expect(r.rotate({ policy: { kind: "passphrase" }, value: "replacement secret" })).rejects.toMatchObject({ code: "state_mismatch" });
    reject(new Error("clear failed"));
    await assertion;
    expect(r.record.latches).toEqual({ paused: true, locked: true, frozen: true });
    await r.tick();
    expect(await h.storage.get(`ps:stream:${STREAM}`)).toMatchObject({ latches: { paused: true, locked: true, frozen: true } });
  });
});
