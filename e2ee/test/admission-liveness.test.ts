import { describe, expect, it, vi } from "vitest";
import { randomBytes } from "../src/bytes.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { expectReject, join, makeHost, viewer } from "./harness.ts";

describe("authenticated owner cancellation of unfinished admissions", () => {
  it.each(["kick", "rotate", "kick_rotate"] as const)("%s rejects a held grant RESPONSE and keeps revocation effective", async (action) => {
    const h = await makeHost(), sid = randomBytes(16);
    const first = await join(h, "old", viewer(h, { viewerSid: sid }));
    const id = h.idOf("old");
    await h.host.peerClosed("old");
    const pending = viewer(h, { viewerSid: sid, grant: first.grant });
    await h.host.receive("replacement", pending.start());
    const [response] = await pending.handle(h.take("replacement")[0]!);
    if (action === "kick") await h.host.kick(id);
    else if (action === "rotate") await h.host.rotatePin("1357");
    else await h.host.kickAndRotatePin(id, "1357");
    const replies = h.take("replacement");
    expect(replies.map(peekType)).toEqual([MsgType.Reject]);
    await expectReject(pending.handle(replies[0]!), "grant_invalid");
    expect(pending._retainedSecrets()).toEqual([]);
    await h.host.receive("replacement", response!);
    expect(h.host.viewers().some((v) => v.peerId === "replacement")).toBe(false);
    await expectReject(join(h, "revoked", viewer(h, { viewerSid: sid, grant: first.grant })), "grant_invalid");
    if (action !== "kick") await expectReject(join(h, "old-pin", viewer(h)), "wrong_pin");
    expect((await join(h, "fresh", viewer(h, { pin: action === "kick" ? "4821" : "1357" }))).session).toBeTruthy();
  });

  it("rotation signs expired for a challenged PIN and settles its reservation before the budget reset", async () => {
    const h = await makeHost(), pending = viewer(h);
    await h.host.receive("pending", pending.start());
    const [response] = await pending.handle(h.take("pending")[0]!);
    const settle = vi.spyOn(h.host.budget, "settle");
    await h.host.rotatePin("1357");
    await expectReject(pending.handle(h.take("pending")[0]!), "expired");
    expect(settle.mock.calls.map((c) => c[1])).toEqual(["abandoned"]);
    expect((await h.host.budget.status()).streamFailures).toBe(0);
    await h.host.receive("pending", response!);
    expect(h.host.viewers()).toEqual([]);
    await expectReject(join(h, "old-pin", viewer(h)), "wrong_pin");
    await join(h, "new-pin", viewer(h, { pin: "1357" }));
  });

  it("an old cancelled HELLO finishing storage cannot abort a new handshake on the same peer", async () => {
    const h = await makeHost(), sid = randomBytes(16);
    const first = await join(h, "old", viewer(h, { viewerSid: sid }));
    await h.host.peerClosed("old");
    const lookup = h.host.grants.lookup.bind(h.host.grants);
    let release!: () => void, entered!: () => void;
    const gate = new Promise<void>((r) => { release = r; });
    const ready = new Promise<void>((r) => { entered = r; });
    vi.spyOn(h.host.grants, "lookup").mockImplementationOnce(async (...args) => {
      const grant = await lookup(...args); entered(); await gate; return grant;
    });
    const old = viewer(h, { viewerSid: sid, grant: first.grant });
    const held = h.host.receive("same", old.start());
    await ready;
    await h.host.rotatePin("1357");
    await expectReject(old.handle(h.take("same")[0]!), "grant_invalid");
    const next = viewer(h, { pin: "1357" });
    await h.host.receive("same", next.start());
    const [response] = await next.handle(h.take("same")[0]!);
    release(); await held;
    await h.host.receive("same", response!);
    await next.handle(h.take("same")[0]!);
    expect(next.state).toBe("admitted");
    expect(h.idOf("same")).toBeTruthy();
  });
});
