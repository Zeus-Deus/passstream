import { readFileSync } from "node:fs";
import { describe, expect, it, vi } from "vitest";
import { _internal, ViewerAdmission } from "../src/admission.ts";
import { randomBytes, toBase64Url } from "../src/bytes.ts";
import { GrantStore } from "../src/grants.ts";
import { SenderHost, type KnockEnd } from "../src/host.ts";
import * as rowCodes from "../src/row-code.ts";
import { COLOURS, ROW_CODE_COUNT, rowCodeColour, rowCodeFromIndex, rowCodeIndex } from "../src/row-code.ts";
import { type Json, MemoryStorage } from "../src/storage.ts";
import { decodeMessage, encodeMessage, expectMessage, MsgType, RejectReason } from "../src/wire.ts";
import { expectReject, type Host, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";

async function pending(h: Host, peer = "a", v = viewer(h)) {
  await h.host.receive(peer, v.start());
  const [response] = await v.handle(h.take(peer)[0]!);
  await h.host.receive(peer, response!);
  const messages = h.take(peer);
  expect(messages).toHaveLength(1);
  const wire = messages[0]!;
  expect(wire.length).toBe(25);
  expect(expectMessage(wire, MsgType.Pending).box.length).toBe(22);
  await v.handle(wire);
  const k = h.host.knocks().find((k) => k.peerId === peer)!;
  return { v, k, wire };
}

async function admitPending(h: Host, peer: string, v: ViewerAdmission) {
  const k = h.host.knocks().find((k) => k.peerId === peer)!;
  await h.host.approve(k.knockId, { rowCode: k.rowCode });
  const messages = h.take(peer);
  for (const m of messages) await v.handle(m);
  return v.result!;
}

async function copyStorage(storage: MemoryStorage) {
  const next = new MemoryStorage();
  for (const [key, raw] of Object.entries(storage.dump())) await next.update(key, () => JSON.parse(raw) as Json);
  return next;
}

describe("host-control knock state machine (CONTRACT §5.3)", () => {
  it("verifies then waits without a grant, key or budget reservation; approval admits the same row code", async () => {
    const h = await makeHost({ knock: true });
    const events: string[] = [];
    h.host.on("knock", (k) => events.push(k.knockId));
    const { v, k } = await pending(h);
    expect(v.state).toBe("pending");
    expect(v.result).toBeNull();
    expect(v.pending?.rowCode).toBe(k.rowCode);
    expect(h.host.viewers()).toEqual([]);
    expect(await h.storage.get(`grants:${STREAM}`)).toEqual({});
    expect(await h.host.budget.status()).toMatchObject({ inflight: 0, streamFailures: 0 });
    expect(events).toEqual([k.knockId]);
    const result = await admitPending(h, "a", v);
    expect(result.rowCode).toBe(k.rowCode);
    expect(h.host.viewers()[0]!.rowCode).toBe(k.rowCode);
    expect(h.host.knocks()).toEqual([]);
    await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
  });

  for (const reason of ["denied", "timeout", "peer_closed", "replaced", "pin_rotated", "stopped"] satisfies KnockEnd[]) {
    it(`pending → ${reason} is terminal; stale approval never releases a key`, async () => {
      const h = await makeHost({ knock: true });
      const { v, k } = await pending(h);
      const ends: KnockEnd[] = [];
      h.host.on("knockEnded", (e) => ends.push(e.reason));
      switch (reason) {
        case "denied": h.host.deny(k.knockId); break;
        case "timeout": h.clock.advance(120_000); await h.host.tick(); break;
        case "peer_closed": await h.host.peerClosed("a"); break;
        case "replaced": await h.host.receive("a", viewer(h).start()); break;
        case "pin_rotated": await h.host.rotatePin("1357", { version: 1 }); break;
        case "stopped": await h.host.close(); break;
      }
      expect(ends).toEqual([reason]);
      expect(h.host.knocks()).toEqual([]);
      await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
      const messages = h.take("a");
      expect(messages.some((m) => m[1] === MsgType.Admit || m[1] === MsgType.Rekey)).toBe(false);
      if (reason === "replaced") {
        // The new transcript owns the peer before the old knock is ended.
        expect(messages.map((m) => m[1])).toEqual([MsgType.Challenge]);
        expect(v.result).toBeNull();
      } else {
        const reject = messages.find((m) => m[1] === MsgType.Reject)!;
        expect(expectMessage(reject, MsgType.Reject).reason).toBe(RejectReason.Denied);
        await expect(v.handle(reject)).rejects.toMatchObject({ code: "rejected", details: { reason: "denied" } });
      }
    });
  }

  it("an old id never binds to a replacement handshake on the same peer", async () => {
    const h = await makeHost({ knock: true });
    const old = await pending(h);
    const v = viewer(h);
    await h.host.receive("a", v.start());
    const messages = h.take("a");
    const [response] = await v.handle(messages.find((m) => m[1] === MsgType.Challenge)!);
    await h.host.receive("a", response!);
    await v.handle(h.take("a")[0]!);
    const fresh = h.host.knocks()[0]!;
    expect(fresh.knockId).not.toBe(old.k.knockId);
    await expect(h.host.approve(old.k.knockId, { rowCode: old.k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
    expect(v.state).toBe("pending");
    await admitPending(h, "a", v);
    expect(v.state).toBe("admitted");
  });

  it("bad canonical input and mismatching codes leave the knock pending", async () => {
    const h = await makeHost({ knock: true });
    const { k } = await pending(h);
    for (const code of ["abc", "AAA·red", "AAAA", " A "]) await expect(h.host.approve(k.knockId, { rowCode: code })).rejects.toMatchObject({ code: "invalid_argument" });
    const wrong = k.rowCode === "AAA" ? "AAC" : "AAA";
    await expect(h.host.approve(k.knockId, { rowCode: wrong })).rejects.toMatchObject({ code: "code_mismatch" });
    expect(h.host.knocks()).toHaveLength(1);
  });

  it("pending → commit_failed rechecks lock and sends signed locked", async () => {
    const h = await makeHost({ knock: true });
    const { k, v } = await pending(h);
    const ends: KnockEnd[] = [];
    h.host.on("knockEnded", (e) => ends.push(e.reason));
    h.host.lock(true);
    await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "conflict" });
    await expect(v.handle(h.take("a")[0]!)).rejects.toMatchObject({ details: { reason: "locked" } });
    expect(ends).toEqual(["commit_failed"]);
    expect(h.host.viewers()).toEqual([]);
  });

  it("approval rechecks reconciling and the freeze budget at commit", async () => {
    for (const gate of ["reconciling", "frozen"] as const) {
      const h = await makeHost({ knock: true, limits: { perStream: 1 } });
      const { k, v } = await pending(h);
      if (gate === "reconciling") h.host.setReconciling(true);
      else {
        const r = await h.host.budget.reserve("attacker");
        if (r.ok) await h.host.budget.settle(r.ticket, "wrong_pin");
      }
      await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "conflict" });
      await expect(v.handle(h.take("a")[0]!)).rejects.toMatchObject({ details: { reason: gate === "frozen" ? "frozen" : "locked" } });
    }
  });

  it("peer close while approval awaits the grant write prevents ADMIT and revokes the orphan grant", async () => {
    const h = await makeHost({ knock: true });
    const { k, v } = await pending(h);
    const update = h.storage.update.bind(h.storage);
    let release!: () => void;
    let entered!: () => void;
    const wait = new Promise<void>((r) => { release = r; });
    const reached = new Promise<void>((r) => { entered = r; });
    h.storage.update = async (key, fn) => {
      if (key.startsWith("grants:")) { entered(); await wait; }
      return update(key, fn);
    };
    const approving = h.host.approve(k.knockId, { rowCode: k.rowCode });
    const assertion = expect(approving).rejects.toMatchObject({ code: "unknown_knock", details: { reason: "expired" } });
    await reached;
    const closed = h.host.peerClosed("a");
    release();
    await assertion;
    await closed;
    // peerClosed removed ownership; an approval continuation cannot send on it.
    expect(h.take("a")).toEqual([]);
    expect(v.result).toBeNull();
    expect(h.host.viewers()).toEqual([]);
    expect(await h.storage.get(`grants:${STREAM}`)).toEqual({});
  });

  it("consumes an approval id once while its commit is in flight", async () => {
    const h = await makeHost({ knock: true });
    const { k, v } = await pending(h);
    const issue = h.host.grants.issue.bind(h.host.grants);
    let release!: () => void;
    let entered!: () => void;
    const wait = new Promise<void>((yes) => { release = yes; });
    const reached = new Promise<void>((yes) => { entered = yes; });
    h.host.grants.issue = async (...args) => { entered(); await wait; return issue(...args); };
    const approving = h.host.approve(k.knockId, { rowCode: k.rowCode });
    await reached;
    expect(h.host.knocks()).toEqual([]);
    expect(h.host.viewers()).toEqual([]);
    await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
    expect(() => h.host.deny(k.knockId)).toThrow(/unknown_knock/);
    release();
    await approving;
    for (const message of h.take("a")) await v.handle(message);
    expect(v.state).toBe("admitted");
    expect(h.host.viewers()).toHaveLength(1);
    expect(await h.host.grants.rowCodes()).toEqual(new Set([k.rowCode]));
  });

  it("approval after expiry is unknown_knock without waiting for tick", async () => {
    const h = await makeHost({ knock: true });
    const { k } = await pending(h);
    h.clock.advance(120_000);
    await expect(h.host.approve(k.knockId, { rowCode: k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
  });

  it("queue_full denies with a 30 s retry, without disturbing the first knock", async () => {
    const h = await makeHost({ knock: true, maxKnocks: 1 });
    const first = await pending(h);
    const ends: KnockEnd[] = [];
    h.host.on("knockEnded", (e) => ends.push(e.reason));
    const v = viewer(h);
    await h.host.receive("b", v.start());
    const [r] = await v.handle(h.take("b")[0]!);
    await h.host.receive("b", r!);
    await expect(v.handle(h.take("b")[0]!)).rejects.toMatchObject({ details: { reason: "denied", retryAfterMs: 30_000 } });
    expect(ends).toEqual(["queue_full"]);
    expect(h.host.knocks()).toEqual([first.k]);
  });

  it.each(["locked", "reconciling", "frozen"] as const)("preparing a knock rechecks %s after awaiting code allocation", async (gate) => {
    const h = await makeHost({ knock: true, limits: { perStream: 1 } });
    const v = viewer(h);
    await h.host.receive("a", v.start());
    const [response] = await v.handle(h.take("a")[0]!);
    const rowCodes = h.host.grants.rowCodes.bind(h.host.grants);
    let release!: () => void;
    let entered!: () => void;
    const wait = new Promise<void>((yes) => { release = yes; });
    const reached = new Promise<void>((yes) => { entered = yes; });
    h.host.grants.rowCodes = async () => { entered(); await wait; return rowCodes(); };
    const preparing = h.host.receive("a", response!);
    await reached;
    if (gate === "locked") h.host.lock(true);
    else if (gate === "reconciling") h.host.setReconciling(true);
    else {
      const ticket = await h.host.budget.reserve("wrong");
      if (ticket.ok) await h.host.budget.settle(ticket.ticket, "wrong_pin");
    }
    release();
    await preparing;
    await expect(v.handle(h.take("a")[0]!)).rejects.toMatchObject({ code: "rejected", details: { reason: gate === "frozen" ? "frozen" : "locked", retryAfterMs: 0 } });
    expect(h.host.knocks()).toEqual([]);
  });

  it("a handshake verified before rotation gets signed expired rather than denied or no reply", async () => {
    const h = await makeHost({ knock: true });
    const v = viewer(h);
    await h.host.receive("a", v.start());
    const [response] = await v.handle(h.take("a")[0]!);
    const settle = h.host.budget.settle.bind(h.host.budget);
    let release!: () => void;
    let entered!: () => void;
    const wait = new Promise<void>((yes) => { release = yes; });
    const reached = new Promise<void>((yes) => { entered = yes; });
    h.host.budget.settle = async (ticket, outcome) => {
      const result = await settle(ticket, outcome);
      if (outcome === "success") { entered(); await wait; }
      return result;
    };
    const responding = h.host.receive("a", response!);
    await reached;
    await h.host.rotatePin("1357", { version: 1 });
    release();
    await responding;
    await expect(v.handle(h.take("a")[0]!)).rejects.toMatchObject({ code: "rejected", details: { reason: "expired", retryAfterMs: 0 } });
    expect(h.host.knocks()).toEqual([]);
  });

  it("a protocol error on a pending handshake ends it as protocol_error and sends signed invalid", async () => {
    const h = await makeHost({ knock: true });
    const { v } = await pending(h);
    const ends: KnockEnd[] = [];
    h.host.on("knockEnded", (e) => ends.push(e.reason));
    await h.host.receive("a", Uint8Array.of(1, 255));
    expect(ends).toEqual(["protocol_error"]);
    expect(h.host.knocks()).toEqual([]);
    await expect(v.handle(h.take("a")[0]!)).rejects.toMatchObject({ code: "rejected", details: { reason: "invalid" } });
  });

  it("no-PIN link admissions knock too, while approved grant reconnects bypass the queue", async () => {
    const h = await makeHost({ knock: true, pin: null });
    const sid = randomBytes(16);
    const { v } = await pending(h, "a", viewer(h, { pin: null, viewerSid: sid }));
    const a = await admitPending(h, "a", v);
    h.host.lock(true);
    const back = await join(h, "b", viewer(h, { viewerSid: sid, grant: a.grant }));
    expect(back.rowCode).toBe(a.rowCode);
    expect(h.host.knocks()).toEqual([]);
    expect(h.host.viewers()[0]!.viewerId).toBe(h.host.viewers()[1]!.viewerId);
  });

  it("viewer timeout is bounded to timeout_ms + 5 s; duplicate PENDING fails closed", async () => {
    const h = await makeHost({ knock: true, knockTimeoutMs: 10 });
    const a = await pending(h, "a", viewer(h, { now: h.clock.now }));
    h.clock.advance(5010);
    expect(a.v.tick()).toEqual({ state: "failed", reason: "knock_timeout" });
    expect(a.v.tick()).toEqual({ state: "failed" });
    expect(a.v.state).toBe("failed");
    expect(a.v._retainedSecrets()).toEqual([]);
    const b = await pending(h, "b");
    await expect(b.v.handle(b.wire)).rejects.toMatchObject({ code: "unexpected_message" });
  });

  it("tampering with the encrypted row code is refused", async () => {
    const h = await makeHost({ knock: true });
    const { wire } = await pending(h);
    const v = viewer(h);
    await h.host.receive("b", v.start());
    const [r] = await v.handle(h.take("b")[0]!);
    await h.host.receive("b", r!);
    const corrupted = wire.slice(); corrupted[10]! ^= 1;
    await expect(v.handle(corrupted)).rejects.toMatchObject({ code: "decrypt_failed" });
  });
});

describe("wire bytes and row identity", () => {
  it("PENDING is exactly 22/25 bytes and ADMIT is exactly 97/113/181 bytes", async () => {
    const h = await makeHost({ knock: true });
    const { k, v } = await pending(h);
    const box = encodeMessage({ type: MsgType.Pending, box: new Uint8Array(22) });
    expect(box.length).toBe(25);
    const bundle = _internal.encodeBundle({ epoch: h.active(), grant: { id: randomBytes(16), secret: randomBytes(32), expiresAt: h.clock.t }, rowCode: k.rowCode });
    expect(bundle.length).toBe(97);
    expect(_internal.decodeBundle(bundle).rowCode).toBe(k.rowCode);
    await h.host.approve(k.knockId, { rowCode: k.rowCode });
    const admit = h.take("a").find((m) => m[1] === MsgType.Admit)!;
    expect(admit.length).toBe(181);
    expect(expectMessage(admit, MsgType.Admit).box.length).toBe(113);
    await v.handle(admit);
    for (const len of [21, 23]) expect(() => decodeMessage(Uint8Array.of(1, 8, len, ...new Uint8Array(len)))).toThrow(/malformed/);
  });

  it("rejects row indices above 10647 in the bundle and authenticated PENDING", async () => {
    const bundle = _internal.encodeBundle({ epoch: { epoch: 1, secret: randomBytes(32) }, grant: { id: randomBytes(16), secret: randomBytes(32), expiresAt: 1 }, rowCode: "999" });
    bundle[95] = 0xff; bundle[96] = 0xff;
    expect(() => _internal.decodeBundle(bundle)).toThrow(/malformed/);
    const h = await makeHost({ knock: true });
    const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
    crypto.subtle.encrypt = async (algorithm, key, data) => {
      const a = algorithm as AesGcmParams;
      if (a.additionalData && new TextDecoder().decode(a.additionalData) === "PassStream v1 pending") {
        const bad = new Uint8Array(6); bad[0] = bad[1] = 255;
        return encrypt(algorithm, key, bad);
      }
      return encrypt(algorithm, key, data);
    };
    try { await expect(pending(h)).rejects.toMatchObject({ code: "malformed" }); }
    finally { crypto.subtle.encrypt = encrypt; }
  });

  it("maps every canonical code one-to-one and derives colour from index only", () => {
    const codes = new Set<string>();
    for (let i = 0; i < ROW_CODE_COUNT; i++) {
      const c = rowCodeFromIndex(i); codes.add(c);
      expect(rowCodeIndex(c)).toBe(i);
      expect(rowCodeColour(c)).toBe(COLOURS[i % 8]);
    }
    expect(codes.size).toBe(10648);
    expect(rowCodeColour("K7P")).toBe("purple"); // normative index formula, not the prose example
  });

  it("keeps the documented K7P colour examples consistent with the code index", () => {
    expect(rowCodeIndex("K7P")).toBe(3838);
    for (const name of ["HOST-CONTROL", "CONTRACT"]) {
      const doc = readFileSync(new URL(`../../docs/${name}.md`, import.meta.url), "utf8");
      const examples = [...doc.matchAll(/K7P\s*·\s*([a-z]+)/g)];
      expect(examples.length).toBeGreaterThan(0);
      for (const example of examples) expect(example[1]).toBe(rowCodeColour("K7P"));
    }
  });

  // Ported from review reproduction B; force the vulnerable interleaving and
  // allocator choice instead of relying on a random collision in 7/12 runs.
  it.each([false, true])("reserves committing codes among over 10,000 grants (refused=%s)", async (refused) => {
    const h = await makeHost({ knock: true });
    const encoded = toBase64Url(new Uint8Array(16));
    await h.storage.update(`grants:${STREAM}`, () => {
      const table: Record<string, Json> = {};
      for (let i = 2; i < ROW_CODE_COUNT; i++) table[`stored-${i}`] = {
        sid: encoded, secret: toBase64Url(new Uint8Array(32)), viewer: encoded,
        code: rowCodeFromIndex(i), exp: h.clock.now() + 3600_000, gen: 0,
      };
      return table;
    });
    const allocator = vi.spyOn(rowCodes, "assignRowCode").mockImplementation((used) => {
      for (let i = 0; i < ROW_CODE_COUNT; i++) if (!used.has(rowCodeFromIndex(i))) return rowCodeFromIndex(i);
      throw new Error("exhausted");
    });
    try {
      const a = await pending(h, "a");
      const b = viewer(h);
      await h.host.receive("b", b.start());
      const [response] = await b.handle(h.take("b")[0]!);
      const get = h.storage.get.bind(h.storage);
      let release!: () => void;
      let entered!: () => void;
      const reached = new Promise<void>((yes) => { entered = yes; });
      let hold = true;
      h.storage.get = (key) => {
        if (hold && key === `grants:${STREAM}`) {
          hold = false;
          return new Promise((yes) => { release = () => { void get(key).then(yes); }; entered(); });
        }
        return get(key);
      };
      const preparing = h.host.receive("b", response!);
      await reached;
      const approving = h.host.approve(a.k.knockId, { rowCode: a.k.rowCode });
      const result = refused ? expect(approving).rejects.toMatchObject({ code: "conflict" }) : approving;
      await expect(h.host.approve(a.k.knockId, { rowCode: a.k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
      if (refused) h.host.lock(true);
      release();
      await preparing;
      await result;
      // B prepared before the lock; its reservation must differ from A's even
      // before A's commit has issued a grant.
      const bk = h.host.knocks().find((k) => k.peerId === "b");
      if (!refused) {
        expect(bk?.rowCode).toBe(rowCodeFromIndex(1));
        expect(h.host.viewers()[0]?.rowCode).toBe(a.k.rowCode);
        await admitPending(h, "b", b);
        expect(new Set(h.host.viewers().map((v) => v.rowCode)).size).toBe(2);
      } else {
        h.host.lock(false);
        const c = await pending(h, "c");
        expect(c.k.rowCode).toBe(a.k.rowCode); // refusal released A's reservation
      }
    } finally { allocator.mockRestore(); }
  });

  it("persists row-code uniqueness through 10,000 grant admissions with bounded live batches", async () => {
    const storage = new MemoryStorage();
    const grants = new GrantStore({ storage, streamId: STREAM });
    let admitted = 0;
    for (let batch = 0; batch < 100; batch++) {
      const live = new Set<string>();
      for (let n = 0; n < 100; n++) {
        const grant = await grants.issue(randomBytes(16));
        expect(live.has(grant.rowCode)).toBe(false);
        live.add(grant.rowCode);
        admitted++;
      }
      expect((await grants.rowCodes()).size).toBe(100);
      await grants.revokeAll();
    }
    expect(admitted).toBe(10_000);
  });

  it("stores grant-scoped viewer_id and row code across reconnect and restart; kick blocks that entire grant", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = await join(h, "a", viewer(h, { viewerSid: sid }));
    const first = h.host.viewers()[0]!;
    expect(first.viewerId.length).toBe(22);
    await h.host.peerClosed("a");
    await join(h, "b", viewer(h, { viewerSid: sid, grant: a.grant }));
    expect(h.host.viewers()[1]!).toMatchObject({ viewerId: first.viewerId, rowCode: first.rowCode });
    const r = await makeHost({ storage: await copyStorage(h.storage), identity: h.identity, linkSecret: h.linkSecret });
    await join(r, "restart", viewer(r, { viewerSid: sid, grant: a.grant }));
    expect(r.host.viewers()[0]!).toMatchObject({ viewerId: first.viewerId, rowCode: first.rowCode });
    await r.host.kick(r.idOf("restart"));
    await expectReject(join(r, "kicked", viewer(r, { viewerSid: sid, grant: a.grant })), "grant_invalid");
    const next = await makeHost({ storage: await copyStorage(r.storage), identity: r.identity, linkSecret: r.linkSecret });
    await expectReject(join(next, "kicked", viewer(next, { viewerSid: sid, grant: a.grant })), "grant_invalid");
  });

  it("persists unique row codes with grants and pending reservations across restart", async () => {
    const h = await makeHost({ knock: true });
    const a = await pending(h, "a");
    const b = await pending(h, "b");
    expect(a.k.rowCode).not.toBe(b.k.rowCode);
    const result = await admitPending(h, "a", a.v);
    const stored = await h.host.grants.lookup(result.grant.id, new Uint8Array(16));
    expect(stored).toBeNull(); // binding still enforced
    const table = await h.storage.get(`grants:${STREAM}`);
    expect(JSON.stringify(table)).toContain(a.k.rowCode);
    const reboot = await makeHost({ storage: await copyStorage(h.storage), identity: h.identity, linkSecret: h.linkSecret, knock: true });
    expect(await reboot.host.grants.rowCodes()).toContain(result.rowCode);
    expect(reboot.host.knocks()).toEqual([]); // old pending handshakes cannot survive a process restart
    const fresh = await pending(reboot, "fresh");
    expect(fresh.k.rowCode).not.toBe(result.rowCode);
    await expect(reboot.host.approve(b.k.knockId, { rowCode: b.k.rowCode })).rejects.toMatchObject({ code: "unknown_knock" });
    expect(toBase64Url(result.grant.id)).toHaveLength(22);
  });
});
