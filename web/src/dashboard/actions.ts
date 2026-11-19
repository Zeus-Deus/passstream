// Which commands the dashboard may send (HOST-CONTROL §4, §7). The dashboard
// is a remote channel: it may only tighten. Loosening commands are drawn
// disabled with LOCAL_ONLY and are never sent; the server and the sender
// refuse them anyway (403 local_owner_required).
import { api, ApiError, commandId, errorText } from "./api";
import type { Viewer } from "./types";

export const LOCAL_ONLY =
  "Do this on the host machine (bar plugin, passstream CLI, or the sender window).";

export type RemoteOp =
  | "stop"
  | "pause"
  | "mode"
  | "safe"
  | "lock"
  | "kick"
  | "knock.deny"
  | "ban";

/** Ops the server forwards from a remote caller, with their closed args. */
export function remoteAllowed(op: string, args: Record<string, unknown> = {}): boolean {
  const keys = Object.keys(args);
  const only = (...allowed: string[]) => keys.every((k) => allowed.includes(k));
  switch (op) {
    case "stop":
    case "pause":
      return keys.length === 0;
    case "mode":
      return only("mode") && (args.mode === "text" || args.mode === "motion");
    case "safe":
      return only("on") && args.on === true;
    case "lock":
      return only("locked") && args.locked === true;
    case "kick":
      return only("admission_id", "lock") && typeof args.admission_id === "string" && (args.lock === undefined || typeof args.lock === "boolean");
    case "knock.deny":
      return only("knock_id") && typeof args.knock_id === "string";
    case "ban":
      return only("connection_id") && typeof args.connection_id === "string";
    default:
      return false; // resume, unfreeze, pin.*, knock.approve, unlock, safe mode off, ...
  }
}

/** What a viewer row offers (CONTRACT Appendix A #14, #16). */
export function rowActions(v: Viewer) {
  const admission = v.admission_ids[0] ?? null;
  const connection = v.connections[0]?.connection_id ?? null;
  const known = ["watching", "reconnecting", "joining", "left", "knocking"].includes(v.status);
  if (v.status === "left" || v.status === "knocking")
    return { kick: null, ban: null, admission, connection };
  if (!known) return { kick: admission, ban: null, admission, connection };
  if (v.status === "joining") return { kick: null, ban: connection, admission, connection };
  return { kick: admission, ban: connection, admission, connection };
}

export type CommandResult =
  | { ok: true; result: Record<string, unknown> }
  | { ok: false; code: string; message: string; details?: Record<string, unknown> };

/**
 * Send one restrictive command. A 504 is retried once with the same
 * idempotency id, so the sender's cache answers it if the first attempt ran.
 */
export async function runCommand(
  streamId: string,
  op: RemoteOp,
  args: Record<string, unknown> = {},
  send = api.command,
): Promise<CommandResult> {
  if (!remoteAllowed(op, args))
    return { ok: false, code: "local_owner_required", message: errorText("local_owner_required") };
  const id = commandId();
  for (let attempt = 0; ; attempt++) {
    try {
      const r = await send(streamId, { id, op, args });
      return { ok: true, result: r.result ?? {} };
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === "sender_timeout" && attempt === 0) continue;
        return { ok: false, code: e.code, message: e.message, details: e.details };
      }
      return { ok: false, code: "network", message: errorText("network") };
    }
  }
}
