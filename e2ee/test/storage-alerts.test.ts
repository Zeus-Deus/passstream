import { describe, expect, it } from "vitest";
import { AlertBatcher, type AlertBatch } from "../src/alerts.ts";
import { GuessBudget } from "../src/budget.ts";
import { LockedLocalStorage } from "../src/storage.ts";

/** A Web Locks stand-in that really serialises async callbacks, plus a Map-backed localStorage. */
function fakeBrowser() {
  const kv = new Map<string, string>();
  const queues = new Map<string, Promise<unknown>>();
  const locks = {
    request<T>(name: string, cb: () => Promise<T> | T): Promise<T> {
      const prev = queues.get(name) ?? Promise.resolve();
      const run = prev.then(async () => {
        await new Promise((r) => setTimeout(r, 1)); // widen any race window
        return cb();
      });
      queues.set(name, run.catch(() => undefined));
      return run;
    },
  };
  return { kv: { getItem: (k: string) => kv.get(k) ?? null, setItem: (k: string, v: string) => void kv.set(k, v) }, locks, raw: kv };
}

describe("LockedLocalStorage", () => {
  it("refuses to run without Web Locks", () => {
    expect(() => new LockedLocalStorage({ kv: fakeBrowser().kv, locks: undefined })).toThrow(/Web Locks/);
  });

  it("serialises concurrent budget reservations from two 'tabs'", async () => {
    const b = fakeBrowser();
    const tabA = new GuessBudget({ storage: new LockedLocalStorage(b), streamId: "s" });
    const tabB = new GuessBudget({ storage: new LockedLocalStorage(b), streamId: "s" });
    const res = await Promise.all(Array.from({ length: 10 }, (_, i) => (i % 2 ? tabA : tabB).reserve("same-viewer")));
    expect(res.filter((r) => r.ok)).toHaveLength(5);
    expect([...b.raw.keys()]).toEqual(["passstream:e2ee:budget:s"]);
  });

  it("treats corrupt stored state as frozen (fail closed)", async () => {
    const b = fakeBrowser();
    b.raw.set("passstream:e2ee:budget:s", JSON.stringify({ v: 1, streamFailures: "lots" }));
    const budget = new GuessBudget({ storage: new LockedLocalStorage(b), streamId: "s" });
    expect(await budget.reserve("x")).toMatchObject({ ok: false, reason: "frozen" });
  });
});

describe("AlertBatcher", () => {
  it("batches failures per window and flushes freezes immediately", () => {
    const batches: AlertBatch[] = [];
    const timers: Array<() => void> = [];
    const a = new AlertBatcher({ onFlush: (x) => batches.push(x), setTimeout: (fn) => timers.push(fn) });
    for (let i = 0; i < 7; i++) a.failure(i < 4 ? "10.0.0.1" : "10.0.0.2", 1000 + i);
    expect(batches).toEqual([]);
    timers.shift()!();
    expect(batches).toEqual([{ failures: 7, sources: 2, frozen: false, firstAt: 1000, lastAt: 1006 }]);
    a.failure("x", 2000);
    a.frozen(2001);
    expect(batches[1]).toMatchObject({ failures: 1, frozen: true });
  });
});
