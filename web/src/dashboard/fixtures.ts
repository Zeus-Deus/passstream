// Test fixtures shaped like internal/server's real JSON.
import type { Connection, Stream, Viewer, ViewerList } from "./types";

export const NOW = 1_800_000_000_000;

export function stream(p: Partial<Stream> = {}): Stream {
  return {
    id: "s_abcdefghijklmnopqrstuA",
    created_at: NOW - 12 * 60_000,
    label: "Design review",
    phase: "live",
    ended_at: null,
    end_reason: null,
    expires_at: null,
    e2ee: true,
    sender: { online: true, kind: "browser", last_seen_at: NOW - 2000 },
    declared: { pin: { kind: "digits", length: 4 }, knock: true },
    effective: {
      phase: "live",
      mode: "text",
      access: {
        pin: { required: true, kind: "digits", length: 4 },
        pin_version: 1,
        rotation_pending: false,
        reconciling: false,
        latches: { locked: false, paused: false, frozen: false },
        knock: true,
        failures: { stream: 3, limit: 20 },
      },
      e2ee: { status: "beta", epoch: 2, suspended: false, revocation_not_durable: false },
    },
    watching: 1,
    knocking: 1,
    ...p,
  };
}

export function connection(p: Partial<Connection> = {}): Connection {
  return {
    connection_id: "c_conn0000000000000000000A",
    joined_at: NOW - 11 * 60_000,
    left_at: null,
    ip: "203.0.113.20",
    geo: { country: "DE", city: "Berlin", asn: 3320, as_org: "Deutsche Telekom AG" },
    user_agent: "Mozilla/5.0",
    device: "iPhone · Safari",
    path: "udp",
    stats: { rtt_ms: 95, loss_pct: 0.4, bitrate_kbps: 3200, freezes: 1, temporal_layer: 2 },
    stats_at: NOW - 1000,
    reported: { label: "Ana", tab_hidden: false },
    ...p,
  };
}

const PROV = {
  viewer_id: "sender",
  row_code: "sender",
  admission_ids: "sender",
  knock_id: "sender",
  sender: "sender",
  "connections.*": "server-observed",
  "connections.*.reported.*": "viewer-reported",
  status: "derived",
  flags: "derived",
  left_at: "derived",
  row_id: "derived",
};

export function viewer(p: Partial<Viewer> = {}): Viewer {
  return {
    row_id: "v:viewer00000000000000000A",
    status: "watching",
    viewer_id: "viewer00000000000000000A",
    row_code: "PFT",
    admission_ids: ["adm00000000000000000000A"],
    knock_id: null,
    sender: { connected: true, via_grant: false, admitted_at: NOW - 11 * 60_000, kick_incomplete: false },
    connections: [connection()],
    flags: [],
    left_at: null,
    provenance: PROV,
    ...p,
  };
}

export function list(viewers: Viewer[] = defaultViewers()): ViewerList {
  return {
    meta: { geoip_attribution: "IP Geolocation by DB-IP" },
    source: "server",
    sender_online: true,
    sender_stale: false,
    state_at: NOW - 2000,
    built_at: NOW,
    watching: viewers.filter((v) => v.status === "watching").length,
    knocking: viewers.filter((v) => v.status === "knocking").length,
    viewers,
  };
}

export function defaultViewers(): Viewer[] {
  return [
    viewer(),
    viewer({
      row_id: "k:knock000000000000000000A",
      status: "knocking",
      viewer_id: null,
      row_code: "UPV",
      admission_ids: [],
      knock_id: "knock000000000000000000A",
      sender: null,
      connections: [connection({ connection_id: "c_conn1111111111111111111A", reported: { label: "Hana", tab_hidden: null }, geo: { country: "NL", city: "Amsterdam", asn: null, as_org: null } })],
    }),
    viewer({
      row_id: "v:viewer11111111111111111A",
      status: "left",
      viewer_id: "viewer11111111111111111A",
      row_code: "K7P",
      admission_ids: ["adm11111111111111111111A"],
      left_at: NOW - 90_000,
      sender: { connected: false, via_grant: true, admitted_at: NOW - 30 * 60_000, kick_incomplete: false },
      connections: [connection({ connection_id: "c_conn2222222222222222222A", left_at: NOW - 90_000, ip: null, reported: { label: "Bo", tab_hidden: true } })],
    }),
  ];
}
