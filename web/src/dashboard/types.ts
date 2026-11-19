// Wire types for the owner HTTP API (CONTRACT §4, §9), matching
// internal/server and internal/hostview. Fields the server adds beyond the
// contract (provenance, meta) are optional.
export type Ms = number;

export type PinPolicy =
  | { kind: "digits"; length: number }
  | { kind: "passphrase" }
  | { kind: "none" };

export interface Latches {
  locked: boolean;
  paused: boolean;
  frozen: boolean;
}

export interface Access {
  pin: {
    required: boolean;
    kind: "digits" | "passphrase" | "none";
    length: number | null;
  };
  pin_version: number;
  rotation_pending: boolean;
  reconciling: boolean;
  latches: Latches;
  knock: boolean;
  failures: { stream: number; limit: number };
}

export interface E2EEState {
  status: "beta" | "on" | "unknown";
  epoch: number;
  suspended: boolean;
  revocation_not_durable: boolean;
}

export interface Health {
  uplink_kbps: number | null;
  encode_ms: number | null;
  fps: number | null;
  encode_cpu_pct?: number | null;
  width?: number | null;
  height?: number | null;
  quality_limitation: "none" | "cpu" | "bandwidth" | "other" | "unknown";
}

export type Phase =
  | "pending"
  | "starting"
  | "picking"
  | "live"
  | "paused"
  | "ended"
  | "unknown";

export interface Stream {
  id: string;
  created_at: Ms;
  label: string | null;
  phase: Phase;
  ended_at: Ms | null;
  end_reason: string | null;
  expires_at: Ms | null;
  e2ee: boolean;
  sender: {
    online: boolean;
    kind: "local" | "browser" | "whip" | string;
    last_seen_at: Ms | null;
  };
  declared: { pin: PinPolicy; knock: boolean };
  effective: {
    phase: Phase;
    mode: "text" | "motion" | "unknown";
    /** Safe mode: the sender redacts sensitive data before encoding. */
    safe: boolean;
    access: Access;
    e2ee: E2EEState;
    health?: Health | null;
  } | null;
  watching: number;
  knocking: number;
}

export interface Geo {
  country: string | null;
  city: string | null;
  asn: number | null;
  as_org: string | null;
}

export interface Connection {
  connection_id: string;
  joined_at: Ms;
  left_at: Ms | null;
  ip: string | null;
  geo: Geo | null;
  user_agent: string | null;
  device: string | null;
  path: "udp" | "tcp" | "turn_udp" | "turn_tcp" | "turn_tls" | null;
  stats: {
    rtt_ms: number | null;
    loss_pct: number | null;
    bitrate_kbps: number | null;
    freezes: number;
    temporal_layer: number | null;
  } | null;
  stats_at: Ms | null;
  reported: { label: string | null; tab_hidden: boolean | null };
}

export type ViewerStatus =
  | "watching"
  | "reconnecting"
  | "knocking"
  | "joining"
  | "left";

export interface Viewer {
  row_id: string;
  status: ViewerStatus | string;
  viewer_id: string | null;
  row_code: string | null;
  admission_ids: string[];
  knock_id: string | null;
  sender: {
    connected: boolean;
    via_grant: boolean;
    admitted_at: Ms;
    kick_incomplete: boolean;
  } | null;
  connections: Connection[];
  flags: string[];
  left_at: Ms | null;
  provenance?: Record<string, string>;
}

export interface ViewerList {
  meta?: Record<string, string>;
  source: "local" | "server";
  sender_online: boolean;
  sender_stale: boolean;
  state_at: Ms | null;
  built_at: Ms;
  watching: number;
  knocking: number;
  viewers: Viewer[];
}

export type Alert =
  | { kind: "failed_pin"; count: number; sources: number; from_at: Ms; to_at: Ms }
  | { kind: "frozen"; failures: number }
  | { kind: "unfrozen"; pin_rotated: boolean }
  | { kind: "revocation_durability"; not_durable: boolean }
  | { kind: "state_mismatch" }
  | { kind: "joined"; viewer_ids: string[] }
  | { kind: "left"; viewer_ids: string[] }
  | { kind: "knock"; knock_id: string; row_code: string }
  | { kind: "knock_ended"; knock_id: string; reason: string };

export interface HistoryEntry {
  row_id: string;
  viewer_id: string | null;
  row_code: string | null;
  admission_ids: string[];
  first_at: Ms;
  last_at: Ms;
  end: string;
  connections: {
    connection_id: string;
    joined_at: Ms;
    left_at: Ms | null;
    ip: string | null;
    geo: Geo | null;
    device: string | null;
  }[];
}

export interface AuditEntry {
  id: string;
  at: Ms;
  stream_id: string | null;
  actor: { kind: "key" | "session" | "server"; key_id: string | null };
  op: string;
  target: Record<string, string>;
  result: "ok" | "error";
  error_code: string | null;
  idempotency_key: string | null;
}

export interface Settings {
  defaults: {
    pin: PinPolicy;
    knock: boolean;
    max_viewers: number;
    expires_in_s: number | null;
  };
  retention: { keep_ip: boolean };
}

export interface Key {
  id: string;
  label: string;
  scope: "read" | "streams" | "owner";
  created_at: Ms;
  expires_at: Ms | null;
  last_used_at: Ms | null;
}

export interface SessionInfo {
  scope: "read" | "streams" | "owner";
  expires_at: Ms;
}

export interface Page<T> {
  entries: T[];
  next_cursor: string | null;
}
