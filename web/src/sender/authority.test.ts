import { describe, expect, it } from "vitest";
import { checkLocal, checkRemote } from "./authority";

const refused = (op: string, args: Record<string, unknown> = {}) => {
  const r = checkRemote(op, args);
  return r && !r.ok ? r.error.code : null;
};

describe("remote commands (server control channel)", () => {
  it("runs only the restrictive ops", () => {
    expect(refused("stop")).toBeNull();
    expect(refused("pause")).toBeNull();
    expect(refused("mode", { mode: "motion" })).toBeNull();
    expect(refused("lock", { locked: true })).toBeNull();
    expect(refused("safe", { on: true })).toBeNull();
    expect(refused("kick", { admission_id: "a".repeat(22) })).toBeNull();
    expect(refused("kick", { admission_id: "a".repeat(22), lock: true })).toBeNull();
    expect(refused("knock.deny", { knock_id: "k".repeat(22) })).toBeNull();
  });
  it("refuses every loosening op with local_owner_required", () => {
    for (const op of ["resume", "unfreeze", "pin.rotate", "pin.set", "knock.approve", "frame", "capture.auth", "record.start", "record.stop", "publish", "something.new"]) expect(refused(op)).toBe("local_owner_required");
    expect(refused("lock", { locked: false })).toBe("local_owner_required");
    expect(refused("safe", { on: false })).toBe("local_owner_required");
  });
  it("refuses a local-only argument even on a restrictive op, so nothing is applied", () => {
    expect(refused("kick", { admission_id: "a".repeat(22), rotate_pin: true })).toBe("local_owner_required");
    expect(refused("kick", { admission_id: "a".repeat(22), pin: "1234" })).toBe("local_owner_required");
    expect(refused("pause", { policy: { kind: "none" } })).toBe("local_owner_required");
    expect(refused("knock.deny", { knock_id: "k", row_code: "K7P" })).toBe("local_owner_required");
  });
  it("closes the argument schema", () => {
    expect(refused("pause", { force: true })).toBe("invalid_argument");
    expect(refused("lock", {})).toBe("invalid_argument");
    expect(refused("mode", { mode: "fast" })).toBe("invalid_argument");
    expect(refused("safe", {})).toBe("invalid_argument");
    expect(refused("safe", { on: "yes" })).toBe("invalid_argument");
    expect(refused("safe", { on: true, extra: 1 })).toBe("invalid_argument");
    expect(refused("kick", { admission_id: 7 })).toBe("invalid_argument");
  });
});

describe("local commands (daemon and window)", () => {
  it("accepts the §10 ops with closed args", () => {
    expect(checkLocal("resume", {})).toBe(true);
    expect(checkLocal("lock", { locked: false })).toBe(true);
    expect(checkLocal("safe", { on: false })).toBe(true);
    expect(checkLocal("safe", { on: true })).toBe(true);
    expect(checkLocal("unfreeze", { keep_pin: true })).toBe(true);
    expect(checkLocal("kick", { admission_id: "a", rotate_pin: true, pin: "123456" })).toBe(true);
    expect(checkLocal("pin.set", { policy: { kind: "digits", length: 6 } })).toBe(true);
    expect(checkLocal("knock.approve", { knock_id: "k", row_code: "K7P" })).toBe(true);
  });
  it("refuses unknown ops, unknown args and inconsistent combinations", () => {
    expect(checkLocal("pin.reveal", {})).toBe(false);
    expect(checkLocal("pause", { now: true })).toBe(false);
    expect(checkLocal("safe", {})).toBe(false);
    expect(checkLocal("safe", { on: 1 })).toBe(false);
    expect(checkLocal("kick", { admission_id: "a", pin: "1234" })).toBe(false); // pin only with rotate_pin
    expect(checkLocal("unfreeze", { keep_pin: true, pin: "1234" })).toBe(false);
    expect(checkLocal("pin.set", { policy: { kind: "digits", length: 3 } })).toBe(false);
    expect(checkLocal("pin.set", { policy: { kind: "pin" } })).toBe(false);
  });
});
