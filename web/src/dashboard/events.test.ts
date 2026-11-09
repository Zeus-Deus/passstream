import { describe, expect, it, vi } from "vitest";
import { backoff, BACKOFF_MAX_MS, EventFeed, type FeedDeps } from "./events";

class FakeES {
  static all: FakeES[] = [];
  listeners = new Map<string, ((e: Event) => void)[]>();
  closed = false;
  constructor(
    public url: string,
    public init?: EventSourceInit,
  ) {
    FakeES.all.push(this);
  }
  addEventListener(t: string, f: (e: Event) => void) {
    this.listeners.set(t, [...(this.listeners.get(t) ?? []), f]);
  }
  close() {
    this.closed = true;
  }
  emit(t: string, data?: unknown) {
    const e = data === undefined ? new Event(t) : new MessageEvent(t, { data: JSON.stringify(data) });
    this.listeners.get(t)?.forEach((f) => f(e));
  }
}

function deps() {
  FakeES.all = [];
  const timers: { fn: () => void; ms: number }[] = [];
  const d: Partial<FeedDeps> = {
    EventSource: FakeES as unknown as typeof EventSource,
    setTimeout: (fn, ms) => timers.push({ fn, ms }),
    clearTimeout: () => undefined,
    random: () => 1,
  };
  return { d, timers };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

describe("EventFeed", () => {
  it("delivers snapshots and alerts", () => {
    const { d } = deps();
    const stream = vi.fn(),
      viewers = vi.fn(),
      alert = vi.fn();
    new EventFeed("/e", { stream, viewers, alert }, d);
    const es = FakeES.all[0];
    expect(es.init?.withCredentials).toBe(true);
    es.emit("stream", { id: "s" });
    es.emit("viewers", { watching: 2 });
    es.emit("alert", { kind: "failed_pin", count: 3 });
    expect(stream).toHaveBeenCalledWith({ id: "s" });
    expect(viewers).toHaveBeenCalledWith({ watching: 2 });
    expect(alert).toHaveBeenCalledWith({ kind: "failed_pin", count: 3 });
  });

  it("drops a malformed event instead of throwing", () => {
    const { d } = deps();
    const viewers = vi.fn();
    new EventFeed("/e", { viewers }, d);
    FakeES.all[0].listeners.get("viewers")![0](new MessageEvent("viewers", { data: "{nope" }));
    expect(viewers).not.toHaveBeenCalled();
  });

  it("reconnects with growing backoff after errors, and resets after open", async () => {
    const { d, timers } = deps();
    const status = vi.fn();
    new EventFeed("/e", { status }, d);
    FakeES.all[0].emit("error");
    expect(FakeES.all[0].closed).toBe(true);
    expect(timers[0].ms).toBe(1000);
    expect(status).toHaveBeenLastCalledWith("retrying", 1000);
    timers[0].fn();
    await flush();
    expect(FakeES.all).toHaveLength(2);
    FakeES.all[1].emit("error");
    expect(timers[1].ms).toBe(2000);
    timers[1].fn();
    await flush();
    FakeES.all[2].emit("open");
    expect(status).toHaveBeenLastCalledWith("open");
    FakeES.all[2].emit("error");
    expect(timers[2].ms).toBe(1000);
  });

  it("stops reconnecting when beforeRetry says the session is gone", async () => {
    const { d, timers } = deps();
    new EventFeed("/e", { beforeRetry: async () => false }, d);
    FakeES.all[0].emit("error");
    timers[0].fn();
    await flush();
    expect(FakeES.all).toHaveLength(1);
  });

  it("stops on ended and after stop()", () => {
    const { d, timers } = deps();
    const ended = vi.fn();
    new EventFeed("/e", { ended }, d);
    FakeES.all[0].emit("ended", { reason: "stopped" });
    expect(ended).toHaveBeenCalledWith({ reason: "stopped" });
    expect(FakeES.all[0].closed).toBe(true);
    FakeES.all[0].emit("error");
    expect(timers).toHaveLength(0);
  });
});

describe("backoff", () => {
  it("is jittered within [base/2, base] and capped", () => {
    expect(backoff(1, () => 0)).toBe(500);
    expect(backoff(1, () => 1)).toBe(1000);
    expect(backoff(3, () => 1)).toBe(4000);
    expect(backoff(30, () => 1)).toBe(BACKOFF_MAX_MS);
  });
});
