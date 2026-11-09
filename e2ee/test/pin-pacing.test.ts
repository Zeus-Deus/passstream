// Stream-wide PIN pacing (PROTOCOL §6): rotating the viewer-chosen session id
// gains no attempts; the wait is capped, and the owner's unlock clears it.
import { describe, expect, it } from "vitest";
import { randomBytes } from "../src/bytes.ts";
import { GuessBudget } from "../src/budget.ts";
import { MemoryStorage } from "../src/storage.ts";
import { expectReject, FakeClock, join, makeHost, viewer } from "./harness.ts";

function budget(clock = new FakeClock(), storage = new MemoryStorage()) {
  return { b: new GuessBudget({ storage, streamId: "pace", now: clock.now }), clock, storage };
}

async function fail(b: GuessBudget, sid: string) {
  const r = await b.reserve(sid);
  if (!r.ok) return r;
  return { ...(await b.settle(r.ticket, "wrong_pin")), ok: true as const };
}

describe("stream-wide PIN pacing", () => {
  it("a fresh session id after 5 failures still waits (the reviewer's rotation probe)", async () => {
    const { b } = budget();
    for (let i = 0; i < 5; i++) expect((await fail(b, "old-sid")).ok).toBe(true);
    expect(await b.reserve("old-sid")).toMatchObject({ ok: false, reason: "backoff" });
    // Before: a new id got a fresh set of attempts at once.
    expect(await b.reserve("new-sid")).toEqual({ ok: false, reason: "backoff", retryAfterMs: 1000, triesLeft: 0 });
  });

  it("rotating ids gains nothing: an attacker hammering for 60 s gets 10 guesses, not 20", async () => {
    const { b, clock } = budget();
    let accepted = 0;
    for (let t = 0; t < 60_000; t += 100) {
      const r = await fail(b, `sid-${t}`);
      if (r.ok) accepted++;
      clock.advance(100);
    }
    // 5 at once, then at +1, +2, +4, +8, +16 s after each previous attempt (+32 s lands past 60 s).
    expect(accepted).toBe(10);
    expect((await b.status()).frozen).toBe(false);
  });

  it("the wait doubles per failure, is capped at 60 s, and the 20-failure freeze still holds", async () => {
    const { b, clock } = budget();
    const waits: number[] = [];
    for (let i = 0; i < 20; i++) {
      const r = await fail(b, `sid-${i}`);
      expect(r.ok).toBe(true);
      if (r.ok && !r.frozen) {
        waits.push(r.retryAfterMs);
        clock.advance(r.retryAfterMs);
      }
    }
    expect(waits.slice(0, 4)).toEqual([0, 0, 0, 0]);
    expect(waits.slice(4, 11)).toEqual([1000, 2000, 4000, 8000, 16000, 32000, 60000]);
    expect(Math.max(...waits)).toBe(60_000);
    expect(await b.reserve("anyone")).toMatchObject({ ok: false, reason: "frozen" });
  });

  it("parallel handshakes can't slip in together when a wait ends", async () => {
    const { b, clock } = budget();
    for (let i = 0; i < 5; i++) await fail(b, `sid-${i}`);
    clock.advance(1000);
    const rs = await Promise.all(["a", "b", "c"].map((s) => b.reserve(s)));
    expect(rs.filter((r) => r.ok)).toHaveLength(1);
    expect(rs.filter((r) => !r.ok).map((r) => (r.ok ? 0 : r.retryAfterMs))).toEqual([2000, 2000]);
  });

  it("an in-flight attempt that succeeds stops counting", async () => {
    const { b, clock } = budget();
    for (let i = 0; i < 5; i++) await fail(b, `sid-${i}`);
    clock.advance(1000);
    const r = await b.reserve("legit");
    expect(r.ok).toBe(true);
    if (r.ok) await b.settle(r.ticket, "success");
    clock.advance(1000);
    expect((await b.reserve("legit2")).ok).toBe(true);
  });

  it("the owner's unlock clears the pacing; a quiet window lets it lapse", async () => {
    const { b, clock } = budget();
    for (let i = 0; i < 8; i++) {
      const r = await fail(b, `sid-${i}`);
      if (r.ok) clock.advance(r.retryAfterMs);
    }
    expect((await b.reserve("x")).ok).toBe(true);
    const { b: b2, clock: c2 } = budget();
    for (let i = 0; i < 6; i++) {
      const r = await fail(b2, `s${i}`);
      if (r.ok && i < 5) c2.advance(r.retryAfterMs);
    }
    expect(await b2.reserve("y")).toMatchObject({ ok: false, retryAfterMs: 2000 });
    await b2.unlock();
    expect((await b2.reserve("y")).ok).toBe(true);
    const { b: b3, clock: c3 } = budget();
    for (let i = 0; i < 6; i++) await fail(b3, `s${i}`);
    c3.advance(600_000);
    expect((await b3.reserve("z")).ok).toBe(true);
  });

  it("survives a sender reload; state saved before pacing loads unpaced", async () => {
    const clock = new FakeClock();
    const storage = new MemoryStorage();
    const { b } = budget(clock, storage);
    for (let i = 0; i < 5; i++) await fail(b, `sid-${i}`);
    const reborn = new GuessBudget({ storage, streamId: "pace", now: clock.now });
    expect(await reborn.reserve("fresh")).toMatchObject({ ok: false, reason: "backoff", retryAfterMs: 1000 });
    const old = new MemoryStorage();
    await old.update("budget:old", () => ({ v: 1, streamFailures: 3, frozen: false, sessions: {}, tickets: {} }));
    const legacy = new GuessBudget({ storage: old, streamId: "old", now: clock.now });
    expect((await legacy.reserve("a")).ok).toBe(true);
    await old.update("budget:bad", () => ({ v: 1, streamFailures: 0, frozen: false, sessions: {}, tickets: {}, recent: ["x"] }));
    expect(await new GuessBudget({ storage: old, streamId: "bad", now: clock.now }).reserve("a")).toMatchObject({ ok: false, reason: "frozen" });
  });
});

describe("stream-wide pacing over real CPace admissions", () => {
  it("rotating viewer session ids gets a signed backoff; grants and the right PIN after the wait still admit", async () => {
    const h = await makeHost();
    const goodSid = randomBytes(16);
    const good = await join(h, "good", viewer(h, { viewerSid: goodSid }));
    for (let i = 0; i < 5; i++) await expectReject(join(h, `bad${i}`, viewer(h, { pin: "9999" })), "wrong_pin");
    // A new viewer-chosen id: no fresh attempts.
    const e = await expectReject(join(h, "bad5", viewer(h, { pin: "9999" })), "backoff");
    expect(e.details).toMatchObject({ retryAfterMs: 1000, triesLeft: 0 });
    // The right PIN waits too (pacing can't tell), but only the capped wait.
    await expectReject(join(h, "legit", viewer(h)), "backoff");
    // An already-admitted viewer re-admits with its grant, unpaced.
    await h.host.peerClosed("good");
    await join(h, "good", viewer(h, { viewerSid: goodSid, grant: good.grant, pin: undefined }));
    h.clock.advance(1000);
    await join(h, "legit", viewer(h));
  });
  it("unlock voids held reservations: they neither pace newcomers nor time out into failures (review C r2)", async () => {
    const { b, clock } = budget();
    // Hold handshakes open (never settled) until the pacing refuses more.
    let held = 0;
    while ((await b.reserve(`held-${held}`)).ok) held++;
    expect(held).toBeGreaterThanOrEqual(5);
    expect(await b.reserve("legit")).toMatchObject({ ok: false, reason: "backoff" });
    await b.unlock();
    expect((await b.status()).inflight).toBe(0);
    // The next legitimate viewer is not paced by the abandoned holds...
    const legit = await b.reserve("legit");
    expect(legit).toMatchObject({ ok: true });
    if (legit.ok) await b.settle(legit.ticket, "success");
    // ...and the holds' timeouts never repopulate the failures the unlock cleared.
    clock.advance(10 * 60_000);
    const st = await b.status();
    expect(st.streamFailures).toBe(0);
    expect(st.frozen).toBe(false);
  });
});
