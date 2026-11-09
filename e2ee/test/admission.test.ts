import { describe, expect, it } from "vitest";
import { ViewerAdmission } from "../src/admission.ts";
import { randomBytes, toHex } from "../src/bytes.ts";
import { GuessBudget } from "../src/budget.ts";
import { E2EEError } from "../src/errors.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { expectReject, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";

describe("PIN admission (CPace over a relay)", () => {
  it("admits with the right PIN and delivers the active epoch and a grant", async () => {
    const h = await makeHost();
    const r = await join(h, "p1", viewer(h));
    expect(r.epochs).toHaveLength(1);
    expect(r.epochs[0]!.epoch).toBe(h.host.keys.activeEpoch);
    expect(toHex(r.epochs[0]!.secret)).toBe(toHex(h.active().secret));
    expect(r.grant.id).toHaveLength(16);
    expect(r.grant.secret).toHaveLength(32);
    expect(h.peers()).toEqual(["p1"]);
    expect((await h.host.budget.status()).streamFailures).toBe(0);
  });

  it("rejects a wrong PIN, signed, with tries left, and consumes budget", async () => {
    const h = await makeHost();
    const failures: string[] = [];
    h.host.budget.on("failure", (e) => failures.push(e.outcome));
    const e = await expectReject(join(h, "p1", viewer(h, { pin: "0000" })), "wrong_pin");
    expect(e.details?.triesLeft).toBe(4);
    expect(failures).toEqual(["wrong_pin"]);
    expect((await h.host.budget.status()).streamFailures).toBe(1);
    expect(h.peers()).toEqual([]);
  });

  it("normalises the PIN (NFC) on both sides", async () => {
    const h = await makeHost({ pin: "café tables" });
    await join(h, "p1", viewer(h, { pin: "café tables" }));
  });

  it("counts an abandoned handshake (peer closed after CHALLENGE)", async () => {
    const h = await makeHost();
    const outcomes: string[] = [];
    h.host.budget.on("failure", (e) => outcomes.push(e.outcome));
    const v = viewer(h);
    await h.host.receive("p1", v.start());
    expect(peekType(h.take("p1")[0]!)).toBe(MsgType.Challenge);
    await h.host.peerClosed("p1");
    expect(outcomes).toEqual(["abandoned"]);
    expect((await h.host.budget.status()).streamFailures).toBe(1);
  });

  it("counts a timed-out handshake and refuses its late RESPONSE", async () => {
    const h = await makeHost();
    const v = viewer(h);
    await h.host.receive("p1", v.start());
    const [challenge] = h.take("p1");
    const [response] = await v.handle(challenge!);
    h.clock.advance(31_000);
    await h.host.tick();
    expect((await h.host.budget.status()).streamFailures).toBe(1);
    await h.host.receive("p1", response!);
    expect(h.take("p1")).toEqual([]); // nothing: the handshake is gone
    expect(h.errors.at(-1)?.code).toBe("unexpected_message");
    expect((await h.host.budget.status()).streamFailures).toBe(1); // not double-counted
  });

  it("counts reservations left behind by a sender crash", async () => {
    const h = await makeHost();
    await h.host.receive("p1", viewer(h).start());
    expect((await h.host.budget.status()).inflight).toBe(1);
    // Sender reloads: a new budget over the same storage finds the stale reservation.
    const reborn = new GuessBudget({ storage: h.storage, streamId: STREAM, now: h.clock.now });
    h.clock.advance(30_000);
    const st = await reborn.status();
    expect(st).toMatchObject({ streamFailures: 1, inflight: 0 });
  });

  it("backs off a viewer session after 5 failures for 60 s, then 120 s", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const backoffs: number[] = [];
    h.host.budget.on("backoff", (e) => backoffs.push(e.durationMs));
    for (let i = 0; i < 4; i++) await expectReject(join(h, "p1", viewer(h, { viewerSid: sid, pin: "1111" })), "wrong_pin");
    const fifth = await expectReject(join(h, "p1", viewer(h, { viewerSid: sid, pin: "1111" })), "wrong_pin");
    expect(fifth.details).toMatchObject({ triesLeft: 0, retryAfterMs: 60_000 });
    const e = await expectReject(join(h, "p1", viewer(h, { viewerSid: sid })), "backoff");
    expect(e.details?.retryAfterMs).toBe(60_000);
    h.clock.advance(60_000);
    // Stream-wide pacing (PROTOCOL §6) spaces these out; wait it out each time.
    for (let i = 0; i < 5; i++) {
      if (i) h.clock.advance(h.host.budget.limits.paceMaxMs);
      await expectReject(join(h, "p1", viewer(h, { viewerSid: sid, pin: "1111" })), "wrong_pin");
    }
    const e2 = await expectReject(join(h, "p1", viewer(h, { viewerSid: sid })), "backoff");
    expect(e2.details?.retryAfterMs).toBe(120_000);
    expect(backoffs).toEqual([60_000, 120_000]);
    h.clock.advance(120_000);
    await join(h, "p1", viewer(h, { viewerSid: sid })); // right PIN still works afterwards
  });

  it("does not let parallel handshakes overshoot the per-session limit", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const vs = Array.from({ length: 10 }, () => viewer(h, { viewerSid: sid, pin: "1111" }));
    await Promise.all(vs.map((v, i) => h.host.receive(`p${i}`, v.start())));
    const types = vs.map((_, i) => peekType(h.take(`p${i}`)[0]!));
    expect(types.filter((t) => t === MsgType.Challenge)).toHaveLength(5);
    expect(types.filter((t) => t === MsgType.Reject)).toHaveLength(5);
  });

  it("freezes new PIN admissions at 20 stream failures; admitted viewers and grants keep working", async () => {
    const h = await makeHost();
    const frozen: number[] = [];
    h.host.budget.on("frozen", (e) => frozen.push(e.streamFailures));
    const ok = await join(h, "good", viewer(h));
    const goodSid = randomBytes(16);
    const ok2 = await join(h, "good2", viewer(h, { viewerSid: goodSid }));
    // Each wrong PIN after the 5th waits out the stream-wide pacing (PROTOCOL §6).
    for (let i = 0; i < 19; i++) {
      h.clock.advance(h.host.budget.limits.paceMaxMs);
      await expectReject(join(h, `bad${i}`, viewer(h, { pin: "9999" })), "wrong_pin");
    }
    h.clock.advance(h.host.budget.limits.paceMaxMs);
    // The 20th failure trips the freeze; its REJECT already says so.
    await expectReject(join(h, "bad19", viewer(h, { pin: "9999" })), "frozen");
    expect(frozen).toEqual([20]);
    await expectReject(join(h, "late", viewer(h)), "frozen"); // even with the right PIN
    // Already-admitted sessions still get rekeys.
    expect(h.peers()).toEqual(["good", "good2"]);
    // A grant re-admission is not a new admission.
    await h.host.peerClosed("good2");
    await join(h, "good2", viewer(h, { viewerSid: goodSid, grant: ok2.grant, pin: undefined }));
    // Owner unlocks; by default that rotates the PIN.
    await h.host.unlock({ newPin: "5555" });
    await expectReject(join(h, "late", viewer(h)), "wrong_pin");
    await join(h, "late", viewer(h, { pin: "5555" }));
    expect(ok.epochs[0]!.epoch).toBeGreaterThan(0);
  });

  it("re-admits with a grant without the PIN; kick and PIN rotation revoke grants", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    await h.host.peerClosed("p1");
    const again = await join(h, "p1", viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined }));
    expect(toHex(again.grant.id)).toBe(toHex(first.grant.id));
    // The grant is bound to its viewer session.
    await expectReject(join(h, "p2", viewer(h, { grant: first.grant, pin: undefined })), "grant_invalid");
    await h.host.kick(h.idOf("p1"));
    await expectReject(join(h, "p1", viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined })), "grant_invalid");
    // Rotation revokes everyone's grants.
    const sid2 = randomBytes(16);
    const g2 = await join(h, "p3", viewer(h, { viewerSid: sid2 }));
    await h.host.rotatePin("7777");
    expect(g2.grant.id).toHaveLength(16);
    await expectReject(join(h, "p3b", viewer(h, { viewerSid: sid2, grant: g2.grant, pin: undefined })), "grant_invalid");
    await expectReject(join(h, "p4", viewer(h)), "wrong_pin");
    await join(h, "p5", viewer(h, { pin: "7777" }));
    // Grant failures do not consume the PIN budget.
    expect((await h.host.budget.status()).streamFailures).toBe(1);
  });

  it("expires grants", async () => {
    const h = await makeHost({ grantTtlMs: 60_000 });
    const sid = randomBytes(16);
    const r = await join(h, "p1", viewer(h, { viewerSid: sid }));
    h.clock.advance(60_001);
    await expectReject(join(h, "p1", viewer(h, { viewerSid: sid, grant: r.grant, pin: undefined })), "grant_invalid");
  });

  it("locks the stream against new PIN admissions only", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const r = await join(h, "p1", viewer(h, { viewerSid: sid }));
    h.host.lock(true);
    await expectReject(join(h, "p2", viewer(h)), "locked");
    await join(h, "p1", viewer(h, { viewerSid: sid, grant: r.grant, pin: undefined }));
  });

  it("supports pin \"none\": the link secret alone admits", async () => {
    const h = await makeHost({ pin: null });
    await join(h, "p1", viewer(h, { pin: null }));
    // Without the link secret (e.g. the relay itself) key confirmation fails.
    await expectReject(join(h, "p2", viewer(h, { pin: null, linkSecret: randomBytes(32) })), "wrong_pin");
    expect(() => viewer(h, { pin: null, linkSecret: undefined as unknown as Uint8Array })).toThrow(E2EEError);
  });

  it("requires a PIN when the signed challenge says so, without answering", async () => {
    const h = await makeHost();
    const v = new ViewerAdmission({ streamId: STREAM, senderKey: h.identity.publicKey, viewerSid: randomBytes(16), pin: null, linkSecret: h.linkSecret });
    await h.host.receive("p1", v.start());
    await expect(v.handle(h.take("p1")[0]!)).rejects.toMatchObject({ code: "rejected", details: { reason: "pin_required" } });
    expect(PIN).toBe("4821");
  });
});
