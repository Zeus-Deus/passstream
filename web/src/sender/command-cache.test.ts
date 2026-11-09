import { describe, expect, it, vi } from "vitest";
import { fail, type Result } from "./authority";
import { sha256, toBase64Url } from "@passstream/e2ee";
import { CommandCache } from "./command-cache";

const ok: Result = { ok: true, data: {} };

describe("CommandCache", () => {
  it("answers a retried command from the cache without running it again", async () => {
    const c = new CommandCache();
    let runs = 0;
    const exec = async () => (runs++, ok);
    expect(await c.run("d1", "pause", {}, exec)).toEqual(ok);
    expect(await c.run("d1", "pause", {}, exec)).toEqual(ok);
    expect(runs).toBe(1);
  });

  it("refuses an id reused for another op (daemon restart: resume=d1, then pause=d1)", async () => {
    const c = new CommandCache();
    let paused = false;
    await c.run("d1", "resume", {}, async () => ((paused = false), ok));
    const r = await c.run("d1", "pause", {}, async () => ((paused = true), ok));
    expect(r.ok).toBe(false);
    expect(!r.ok && r.error.code).toBe("conflict");
    expect(paused).toBe(false);
  });

  it("refuses an id reused with other arguments, whatever the key order", async () => {
    const c = new CommandCache();
    await c.run("x", "kick", { admission_id: "a", lock: true }, async () => ok);
    expect(await c.run("x", "kick", { lock: true, admission_id: "a" }, async () => fail("internal"))).toEqual(ok);
    const r = await c.run("x", "kick", { admission_id: "b", lock: true }, async () => ok);
    expect(!r.ok && r.error.code).toBe("conflict");
  });

  it("shares an in-flight execution", async () => {
    const c = new CommandCache();
    let runs = 0;
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const exec = async () => {
      runs++;
      await gate;
      return ok;
    };
    const a = c.run("d2", "lock", { locked: true }, exec);
    const b = c.run("d2", "lock", { locked: true }, exec);
    release();
    expect(await a).toEqual(ok);
    expect(await b).toEqual(ok);
    expect(runs).toBe(1);
  });

  it("lets storage and internal failures retry, keeps terminal failures", async () => {
    const c = new CommandCache();
    let runs = 0;
    const flaky = async () => (++runs === 1 ? fail("storage") : ok);
    expect((await c.run("d3", "pause", {}, flaky)).ok).toBe(false);
    await Promise.resolve();
    expect(await c.run("d3", "pause", {}, flaky)).toEqual(ok);
    expect(runs).toBe(2);

    let internal = 0;
    const boom = async (): Promise<Result> => {
      internal++;
      throw new Error("x");
    };
    expect((await c.run("d4", "pause", {}, boom)).ok).toBe(false);
    await Promise.resolve();
    expect((await c.run("d4", "pause", {}, boom)).ok).toBe(false);
    expect(internal).toBe(2);

    let terminal = 0;
    const refused = async () => (terminal++, fail("unknown_admission"));
    await c.run("d5", "kick", { admission_id: "z" }, refused);
    await c.run("d5", "kick", { admission_id: "z" }, refused);
    expect(terminal).toBe(1);
  });

  it("never keeps a PIN argument in the clear", async () => {
    const c = new CommandCache();
    await c.run("d6", "pin.rotate", { pin: "123456" }, async () => ok);
    expect(JSON.stringify([...c.keys()])).not.toContain("123456");
  });

  it("does not cache a retryable rotation_pending (review round 1 repro)", async () => {
    const c = new CommandCache();
    let runs = 0;
    await c.run("retry", "pin.rotate", {}, async () => (runs++, fail("rotation_pending", "pending")));
    await Promise.resolve();
    expect(await c.run("retry", "pin.rotate", {}, async () => (runs++, ok))).toEqual(ok);
    expect(runs).toBe(2);
  });

  it("never evicts an in-flight command: 256 later ids can't make a replay run twice", async () => {
    const c = new CommandCache();
    let runs = 0;
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const exec = async () => {
      runs++;
      await gate;
      return ok;
    };
    const a = c.run("inflight", "kick", { admission_id: "same" }, exec);
    for (let i = 0; i < 256; i++) await c.run("other" + i, "pause", {}, async () => ok);
    const b = c.run("inflight", "kick", { admission_id: "same" }, exec);
    release();
    expect(await a).toEqual(ok);
    expect(await b).toEqual(ok);
    expect(runs).toBe(1);
    expect(c.size).toBeLessThanOrEqual(256);
  });

  it("refuses a new id with rate_limited while every slot is in flight", async () => {
    const c = new CommandCache(3);
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const slow = async () => (await gate, ok);
    const running = [c.run("a", "pause", {}, slow), c.run("b", "pause", {}, slow), c.run("c", "pause", {}, slow)];
    let ran = false;
    const r = await c.run("d", "pause", {}, async () => ((ran = true), ok));
    expect(!r.ok && r.error.code).toBe("rate_limited");
    expect(ran).toBe(false);
    release();
    await Promise.all(running);
    expect(await c.run("d", "pause", {}, async () => ((ran = true), ok))).toEqual(ok);
    expect(ran).toBe(true);
    expect(c.size).toBe(3);
  });
});

it("uses HMAC-SHA-256 with a random context key instead of a guessable PIN digest", async () => {
 const original = crypto.getRandomValues.bind(crypto);
 const random = vi.spyOn(crypto, "getRandomValues").mockImplementation((a) => {
   if (a instanceof Uint8Array) { a.fill(0x0b); return a; }
   return original(a);
 });
 try {
   vi.resetModules();
   const {commandFingerprint} = await import("./command-cache");
   const input = JSON.stringify(["pin.rotate", {pin:"4821"}]);
   const key = await crypto.subtle.importKey("raw", new Uint8Array(32).fill(0x0b), {name:"HMAC", hash:"SHA-256"}, false, ["sign"]);
   const expected = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(input));
   expect(commandFingerprint("pin.rotate",{pin:"4821"})).toBe(toBase64Url(new Uint8Array(expected)));
   expect(commandFingerprint("pin.rotate",{pin:"4821"})).not.toBe(toBase64Url(sha256(new TextEncoder().encode(input))));
 } finally { random.mockRestore(); vi.resetModules(); }
});
