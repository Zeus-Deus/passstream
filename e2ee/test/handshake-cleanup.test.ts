import { afterEach, describe, expect, it, vi } from "vitest";
import { MsgType, peekType } from "../src/wire.ts";
import { makeHost, viewer } from "./harness.ts";

afterEach(() => vi.restoreAllMocks());

describe("handshake cleanup ownership", () => {
  it("ordinary timeout cleanup preserves a concurrent retry on the same peer", async () => {
    const h = await makeHost();
    await h.host.receive("p", viewer(h).start());
    h.take("p"); // The original CHALLENGE was lost.
    h.clock.advance(30_001);
    const timeout = h.host.tick();
    const next = viewer(h);
    await Promise.all([timeout, h.host.receive("p", next.start())]);
    const challenge = h.take("p");
    expect(challenge.map(peekType)).toEqual([MsgType.Challenge]);
    const [response] = await next.handle(challenge[0]!);
    await h.host.receive("p", response!);
    await next.handle(h.take("p")[0]!);
    expect(next.state).toBe("admitted");
    expect(h.errors).toEqual([]);
    expect((await h.host.budget.status()).streamFailures).toBe(1);
  });

  it.each(["timeout", "unlock"] as const)("%s finishing an old settlement preserves a replacement RESPONSE", async (action) => {
    const h = await makeHost();
    await h.host.receive("p", viewer(h).start());
    h.take("p");
    if (action === "timeout") h.clock.advance(30_001);
    let release!: () => void, entered!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const ready = new Promise<void>((resolve) => { entered = resolve; });
    const settle = h.host.budget.settle.bind(h.host.budget);
    vi.spyOn(h.host.budget, "settle").mockImplementationOnce(async (...args) => {
      entered(); await gate;
      return settle(...args);
    });
    const cleanup = action === "timeout" ? h.host.tick() : h.host.unlock({ keepPin: true });
    await ready;
    const next = viewer(h);
    await h.host.receive("p", next.start());
    const challenge = h.take("p");
    expect(challenge.map(peekType)).toEqual([MsgType.Challenge]);
    const [response] = await next.handle(challenge[0]!);
    release(); await cleanup;
    expect(h.take("p")).toEqual([]);
    await h.host.receive("p", response!);
    const replies = h.take("p");
    expect(replies.map(peekType)).toEqual([MsgType.Admit]);
    await next.handle(replies[0]!);
    expect(next.state).toBe("admitted");
    expect(h.errors).toEqual([]);
    expect((await h.host.budget.status()).streamFailures).toBe(action === "timeout" ? 1 : 0);
  });

  it("queued peer-close cleanup does not abort a newer HELLO", async () => {
    const h = await makeHost();
    await h.host.receive("p", viewer(h).start());
    h.take("p");
    const closing = h.host.peerClosed("p");
    const next = viewer(h);
    await Promise.all([closing, h.host.receive("p", next.start())]);
    const challenge = h.take("p");
    expect(challenge.map(peekType)).toEqual([MsgType.Challenge]);
    const [response] = await next.handle(challenge[0]!);
    await h.host.receive("p", response!);
    await next.handle(h.take("p")[0]!);
    expect(next.state).toBe("admitted");
    expect(h.errors).toEqual([]);
    expect((await h.host.budget.status()).streamFailures).toBe(1);
  });
});
