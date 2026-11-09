// Owner HTTP API client (CONTRACT §7, §9). The dashboard authenticates with
// the HttpOnly session cookie; the only bearer uses are the login exchange
// and owner-only key management, where the typed key is held in memory for
// that request alone.
import type {
  AuditEntry,
  HistoryEntry,
  Key,
  Page,
  SessionInfo,
  Settings,
  Stream,
  ViewerList,
} from "./types";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details?: Record<string, unknown>,
  ) {
    super(message);
  }
}

const signedOut = new Set<() => void>();
/** Subscribe to "the server no longer accepts this session". */
export function onSignedOut(fn: () => void) {
  signedOut.add(fn);
  return () => {
    signedOut.delete(fn);
  };
}

type Init = RequestInit & { bearer?: string; quiet401?: boolean };

export async function request<T>(path: string, init: Init = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.bearer) headers.set("Authorization", `Bearer ${init.bearer}`);
  if (init.body !== undefined && !headers.has("Content-Type"))
    headers.set("Content-Type", "application/json");
  let r: Response;
  try {
    r = await fetch(path, {
      ...init,
      headers,
      credentials: "same-origin",
    });
  } catch {
    throw new ApiError(0, "network", "The server can't be reached.");
  }
  if (r.status === 204) return undefined as T;
  const text = await r.text();
  let body: unknown = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    /* non-JSON error page */
  }
  if (!r.ok) {
    const e = (body as { error?: { code?: string; message?: string; details?: Record<string, unknown> } })?.error;
    const code = e?.code ?? `http_${r.status}`;
    const err = new ApiError(r.status, code, errorText(code, e?.message), e?.details);
    // A bearer request failing proves nothing about the cookie session.
    if (r.status === 401 && !init.bearer && !init.quiet401)
      signedOut.forEach((fn) => fn());
    throw err;
  }
  return body as T;
}

/** Human text for the contract's error codes (CONTRACT §14). */
export function errorText(code: string, fallback?: string): string {
  switch (code) {
    case "unauthorized":
      return "That key or session isn't valid.";
    case "forbidden_scope":
      return "This key or session doesn't have the scope for that.";
    case "origin_rejected":
      return "The request came from an unexpected origin.";
    case "local_owner_required":
      return "Only the host machine can do that (bar plugin, passstream CLI or the sender window).";
    case "sender_offline":
      return "The sender is offline, so it can't apply that right now.";
    case "sender_timeout":
      return "The sender didn't answer within 5 s. It may still apply; check the viewer list.";
    case "unknown_admission":
      return "That viewer is already gone.";
    case "unknown_knock":
      return "That knock has already ended.";
    case "stream_ended":
      return "The stream has ended.";
    case "rate_limited":
      return "Too many requests. Wait a moment and try again.";
    case "not_found":
      return "Not found.";
    case "conflict":
      return "That conflicts with the current state.";
    case "storage":
      return "The sender couldn't save that. It retries by itself; check the viewer list.";
    case "invalid_argument":
      return "The server rejected that input.";
    case "network":
      return "The server can't be reached.";
    default:
      return fallback ? fallback[0].toUpperCase() + fallback.slice(1) + "." : `Request failed (${code}).`;
  }
}

const json = (v: unknown) => JSON.stringify(v);

export const api = {
  login: (key: string) =>
    request<void>("/api/v1/session", { method: "POST", bearer: key }),
  session: () => request<SessionInfo>("/api/v1/session", { quiet401: true }),
  logout: () => request<void>("/api/v1/session", { method: "DELETE" }),

  streams: (state: "live" | "ended" | "all", cursor?: string | null) =>
    request<{ streams: Stream[]; next_cursor: string | null }>(
      `/api/v1/streams?state=${state}&limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
    ),
  stream: (id: string) =>
    request<Stream>(`/api/v1/streams/${encodeURIComponent(id)}`),
  viewers: (id: string) =>
    request<ViewerList>(`/api/v1/streams/${encodeURIComponent(id)}/viewers`),
  history: (id: string, cursor?: string | null) =>
    request<Page<HistoryEntry>>(
      `/api/v1/streams/${encodeURIComponent(id)}/history?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
    ),
  audit: (streamId?: string, cursor?: string | null) =>
    request<Page<AuditEntry>>(
      `/api/v1/audit?limit=100${streamId ? `&stream_id=${encodeURIComponent(streamId)}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
    ),
  command: (
    streamId: string,
    body: { id: string; op: string; args: Record<string, unknown> },
  ) =>
    request<{ id: string; ok: true; result: Record<string, unknown> }>(
      `/api/v1/streams/${encodeURIComponent(streamId)}/commands`,
      { method: "POST", body: json(body) },
    ),

  settings: () => request<Settings>("/api/v1/settings"),
  putSettings: (s: Settings, bearer?: string) =>
    request<Settings>("/api/v1/settings", {
      method: "PUT",
      body: json(s),
      bearer,
    }),

  keys: (bearer: string) =>
    request<{ keys: Key[] }>("/api/v1/keys", { bearer }),
  createKey: (
    bearer: string,
    v: { label: string; scope: Key["scope"]; expires_in_s: number | null },
  ) =>
    request<Key & { key: string }>("/api/v1/keys", {
      method: "POST",
      body: json(v),
      bearer,
    }),
  deleteKey: (bearer: string, id: string) =>
    request<void>(`/api/v1/keys/${encodeURIComponent(id)}`, {
      method: "DELETE",
      bearer,
    }),
};

/** A command idempotency id: 1–64 chars of [A-Za-z0-9_-] (CONTRACT §9.6). */
export function commandId(): string {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return "dash_" + [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}
