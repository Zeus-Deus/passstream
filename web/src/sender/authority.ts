/**
 * The sender's command rules (HOST-CONTROL §4, CONTRACT §8.5 and §10).
 * Restrictive commands may come from anywhere; permissive ones only from the
 * local daemon or the sender window. Arguments are closed per channel. The
 * server filters remote calls as a courtesy; this is the check that counts.
 */
import type { PinPolicy } from "@passstream/e2ee";

export type Result = { ok: true; data: Record<string, unknown> } | { ok: false; error: { code: string; message: string; details?: Record<string, unknown> } };

export function fail(code: string, message = code, details?: Record<string, unknown>): Result {
  return { ok: false, error: { code, message, ...(details ? { details } : {}) } };
}

const LOCAL_ONLY_ARGS = ["rotate_pin", "pin", "keep_pin", "row_code", "policy"];

function closedArgs(args: Record<string, unknown>, allowed: string[]): boolean {
  return Object.keys(args).every((k) => allowed.includes(k));
}

export function parsePolicy(v: unknown): PinPolicy | null {
  if (!v || typeof v !== "object") return null;
  const p = v as Record<string, unknown>;
  if (p.kind === "none" && Object.keys(p).length === 1) return { kind: "none" };
  if (p.kind === "passphrase" && Object.keys(p).length === 1) return { kind: "passphrase" };
  if (p.kind === "digits" && Number.isInteger(p.length) && (p.length as number) >= 4 && (p.length as number) <= 12 && Object.keys(p).length === 2)
    return { kind: "digits", length: p.length as number };
  return null;
}

/** A `control` from the server: null if it may run, else the refusal. Nothing is applied on refusal. */
export function checkRemote(op: string, args: Record<string, unknown>): Result | null {
  if (Object.keys(args).some((k) => LOCAL_ONLY_ARGS.includes(k))) return fail("local_owner_required");
  switch (op) {
    case "stop":
    case "pause":
      return closedArgs(args, []) ? null : fail("invalid_argument");
    case "mode":
      return closedArgs(args, ["mode"]) && (args.mode === "text" || args.mode === "motion") ? null : fail("invalid_argument");
    case "lock":
      if (args.locked === false) return fail("local_owner_required");
      return closedArgs(args, ["locked"]) && args.locked === true ? null : fail("invalid_argument");
    case "kick":
      return closedArgs(args, ["admission_id", "lock"]) && typeof args.admission_id === "string" && (args.lock === undefined || typeof args.lock === "boolean") ? null : fail("invalid_argument");
    case "knock.deny":
      return closedArgs(args, ["knock_id"]) && typeof args.knock_id === "string" ? null : fail("invalid_argument");
    case "annotations":
      // Viewer drawing on is permissive (local only); off is restrictive.
      if (args.viewers === true) return fail("local_owner_required");
      return closedArgs(args, ["viewers"]) && args.viewers === false ? null : fail("invalid_argument");
    case "annotations.clear":
      return closedArgs(args, []) ? null : fail("invalid_argument");
  }
  // resume, unfreeze, pin.*, knock.approve, annotations {viewers: true} and anything unknown.
  return fail("local_owner_required");
}

/** A command from the daemon or the window: are its arguments valid (closed)? */
export function checkLocal(op: string, args: Record<string, unknown>): boolean {
  const ok = (allowed: string[]) => closedArgs(args, allowed);
  const str = (k: string) => args[k] === undefined || typeof args[k] === "string";
  const bool = (k: string) => args[k] === undefined || typeof args[k] === "boolean";
  switch (op) {
    case "stop":
    case "pause":
    case "resume":
      return ok([]);
    case "mode":
      return ok(["mode"]) && (args.mode === "text" || args.mode === "motion");
    case "lock":
      return ok(["locked"]) && typeof args.locked === "boolean";
    case "unfreeze":
      return ok(["keep_pin", "pin"]) && bool("keep_pin") && str("pin") && !(args.keep_pin === true && args.pin !== undefined);
    case "kick":
      return ok(["admission_id", "lock", "rotate_pin", "pin"]) && typeof args.admission_id === "string" && bool("lock") && bool("rotate_pin") && str("pin") && (args.pin === undefined || args.rotate_pin === true);
    case "pin.rotate":
      return ok(["pin"]) && str("pin");
    case "pin.set":
      return ok(["policy", "pin"]) && parsePolicy(args.policy) !== null && str("pin");
    case "knock.approve":
      return ok(["knock_id", "row_code"]) && typeof args.knock_id === "string" && typeof args.row_code === "string";
    case "knock.deny":
      return ok(["knock_id"]) && typeof args.knock_id === "string";
    case "annotations":
      return ok(["viewers"]) && typeof args.viewers === "boolean";
    case "annotations.clear":
      return ok([]);
    case "record.start":
      return ok(["duration", "max_bytes"]);
    case "record.stop":
    case "record.status":
      return ok([]);
  }
  return false;
}
