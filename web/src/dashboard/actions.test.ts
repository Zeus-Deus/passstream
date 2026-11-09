import { describe, expect, it, vi } from "vitest";
import { remoteAllowed, rowActions, runCommand } from "./actions";
import { ApiError } from "./api";
import { viewer } from "./fixtures";

describe("remoteAllowed: the dashboard may only tighten", () => {
  it.each([
    ["stop", {}],
    ["pause", {}],
    ["lock", { locked: true }],
    ["kick", { admission_id: "a" }],
    ["kick", { admission_id: "a", lock: true }],
    ["knock.deny", { knock_id: "k" }],
    ["ban", { connection_id: "c_x" }],
    ["mode", { mode: "motion" }],
  ])("allows %s %j", (op, args) => expect(remoteAllowed(op, args)).toBe(true));

  it.each([
    ["resume", {}],
    ["unfreeze", {}],
    ["pin.rotate", {}],
    ["pin.set", { pin: { kind: "none" } }],
    ["knock.approve", { knock_id: "k", row_code: "K7P" }],
    ["lock", { locked: false }],
    ["kick", { admission_id: "a", rotate_pin: true }],
    ["pause", { extra: 1 }],
  ])("refuses %s %j", (op, args) => expect(remoteAllowed(op, args)).toBe(false));
});

describe("runCommand", () => {
  it("never sends a loosening command", async () => {
    const send = vi.fn();
    // @ts-expect-error resume isn't a RemoteOp; a caller bug must still not send it
    const r = await runCommand("s", "resume", {}, send);
    expect(send).not.toHaveBeenCalled();
    expect(r).toMatchObject({ ok: false, code: "local_owner_required" });
  });

  it("handles a 403 local_owner_required from the server gracefully", async () => {
    const send = vi.fn().mockRejectedValue(new ApiError(403, "local_owner_required", "x"));
    const r = await runCommand("s", "kick", { admission_id: "a" }, send);
    expect(r).toMatchObject({ ok: false, code: "local_owner_required", message: "x" });
  });

  it("retries a 504 once with the same idempotency id", async () => {
    const send = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(504, "sender_timeout", "t"))
      .mockResolvedValueOnce({ id: "x", ok: true, result: { admission_ids: ["a"], locked: true } });
    const r = await runCommand("s", "kick", { admission_id: "a", lock: true }, send);
    expect(r).toEqual({ ok: true, result: { admission_ids: ["a"], locked: true } });
    expect(send).toHaveBeenCalledTimes(2);
    expect(send.mock.calls[0][1].id).toBe(send.mock.calls[1][1].id);
    expect(send.mock.calls[0][1].id).toMatch(/^[A-Za-z0-9_-]{1,64}$/);
  });

  it("passes kick failure details through (lock still set)", async () => {
    const send = vi.fn().mockRejectedValue(new ApiError(404, "unknown_admission", "gone", { locked: true }));
    const r = await runCommand("s", "kick", { admission_id: "a", lock: true }, send);
    expect(r).toMatchObject({ ok: false, details: { locked: true } });
  });
});

describe("rowActions (Appendix A #14, #16)", () => {
  it("watching rows can be kicked and banned", () => {
    expect(rowActions(viewer())).toMatchObject({ kick: "adm00000000000000000000A", ban: "c_conn0000000000000000000A" });
  });
  it("joining rows can't be kicked, only banned", () => {
    expect(rowActions(viewer({ status: "joining", admission_ids: [] }))).toMatchObject({ kick: null, ban: "c_conn0000000000000000000A" });
  });
  it("rows without connections can't be banned", () => {
    expect(rowActions(viewer({ connections: [] })).ban).toBeNull();
  });
  it("unknown statuses offer only admission kick", () => {
    expect(rowActions(viewer({ status: "mystery" }))).toMatchObject({ kick: "adm00000000000000000000A", ban: null });
  });
  it("left rows offer nothing", () => {
    expect(rowActions(viewer({ status: "left" }))).toMatchObject({ kick: null, ban: null });
  });
});
