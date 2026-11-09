// A scripted sender and viewers over the real /signal WebSocket (protocol 1),
// so the dashboard runs against a real server with realistic rows. It speaks
// only the relay-visible parts of the protocol: SenderState, alerts and
// control results. No media and no E2EE.
import { randomBytes } from "node:crypto";

export const id22 = () => randomBytes(16).toString("base64url");
const ALPHABET = "ACDEFHJKMNPRTUVWXY3479";
export const rowCode = () => [0, 1, 2].map(() => ALPHABET[randomBytes(1)[0] % 22]).join("");

export const UA = {
  iphone: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
  android: "Mozilla/5.0 (Linux; Android 15; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
  mac: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
  linux: "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
};

export interface Created {
  id: string;
  view_link_base: string;
  publish_token: string;
}

type Admission = { admission_id: string; viewer_id: string; row_code: string; peer_id: string; connected: boolean; via_grant: boolean; admitted_at: number; kick_incomplete: boolean };
type Knock = { knock_id: string; peer_id: string; row_code: string; at: number; expires_at: number };
type End = Record<string, string>;

function open(url: string, headers: Record<string, string>, hello: unknown): Promise<{ ws: WebSocket; joined: Record<string, unknown> }> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url, { headers } as never);
    const t = setTimeout(() => reject(new Error("join timeout")), 5000);
    ws.onopen = () => ws.send(JSON.stringify(hello));
    ws.onerror = () => reject(new Error("websocket error"));
    ws.addEventListener("message", (e) => {
      const m = JSON.parse(String(e.data));
      if (m.type === "joined") {
        clearTimeout(t);
        resolve({ ws, joined: m });
      } else if (m.type === "error") {
        clearTimeout(t);
        reject(new Error(`join refused: ${m.code}`));
      }
    });
  });
}

export async function api<T>(base: string, key: string, path: string, init: RequestInit = {}): Promise<T> {
  const r = await fetch(base + path, { ...init, headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json", ...(init.headers ?? {}) } });
  if (!r.ok) throw new Error(`${init.method ?? "GET"} ${path}: ${r.status} ${await r.text()}`);
  return (r.status === 204 ? undefined : await r.json()) as T;
}

export class FakeSender {
  ws: WebSocket | null = null;
  instance = id22();
  seq = 0;
  phase = "live";
  mode = "text";
  latches = { locked: false, paused: false, frozen: false };
  failures = 0;
  pin: { kind: "digits" | "passphrase" | "none"; length: number | null } = { kind: "digits", length: 4 };
  knockMode = true;
  admissions: Admission[] = [];
  knocks: Knock[] = [];
  ends: End[] = [];
  viewers = new Map<string, WebSocket>();
  controls: { op: string; args: Record<string, unknown> }[] = [];
  private lastSent = 0;
  private heartbeat: ReturnType<typeof setInterval> | null = null;
  private pending: ReturnType<typeof setTimeout> | null = null;

  constructor(
    readonly base: string,
    readonly stream: Created,
  ) {}

  static async create(base: string, key: string, body: Record<string, unknown>) {
    const s = await api<Created>(base, key, "/api/v1/streams", { method: "POST", body: JSON.stringify(body) });
    return new FakeSender(base, s);
  }

  get wsURL() {
    return this.base.replace(/^http/, "ws") + "/signal";
  }

  async connect() {
    const { ws } = await open(this.wsURL, {}, { type: "join", role: "publish", protocol: 1, id: this.stream.id, credential: this.stream.publish_token, instance: this.instance, sender_kind: "browser" });
    this.ws = ws;
    ws.addEventListener("message", (e) => {
      const m = JSON.parse(String(e.data));
      if (m.type === "control") this.onControl(m);
    });
    // SenderState goes out on every change and at least every 5 s (CONTRACT §4.4).
    this.heartbeat = setInterval(() => this.push(), 3000);
    this.push();
    await this.settle();
  }

  /** A viewer socket; returns its server connection id. */
  async viewer(label: string | null, ua: string, ip: string) {
    const c = new URL(this.stream.view_link_base).hash.slice(3); // "#c=…"
    const hello: Record<string, unknown> = { type: "join", role: "view", protocol: 1, credential: c };
    if (label) hello.label = label;
    const { ws, joined } = await open(this.wsURL, { "User-Agent": ua, "X-Forwarded-For": ip }, hello);
    const id = String(joined.connection_id);
    this.viewers.set(id, ws);
    return id;
  }

  admit(peer: string, code = rowCode(), viaGrant = false, ageMs = 0) {
    const a = { admission_id: id22(), viewer_id: id22(), row_code: code, peer_id: peer, connected: true, via_grant: viaGrant, admitted_at: Date.now() - ageMs, kick_incomplete: false };
    this.admissions.push(a);
    this.push();
    return a;
  }

  knock(peer: string, code = rowCode()) {
    const k = { knock_id: id22(), peer_id: peer, row_code: code, at: Date.now(), expires_at: Date.now() + 120_000 };
    this.knocks.push(k);
    this.push();
    return k;
  }

  /** Sender-side kick (as if from the plugin): the row moves to "left". */
  kick(admissionId: string) {
    const a = this.admissions.find((x) => x.admission_id === admissionId);
    if (!a) return false;
    this.admissions = this.admissions.filter((x) => x !== a);
    this.ends.unshift({ kind: "admission", admission_id: a.admission_id, viewer_id: a.viewer_id, cause: "kicked" });
    this.viewers.get(a.peer_id)?.close();
    this.push();
    return true;
  }

  /** Sender-side knock end (e.g. denied in the plugin): goes to history at once. */
  endKnock(knockId: string, reason = "denied") {
    const k = this.knocks.find((x) => x.knock_id === knockId);
    if (!k) return;
    this.knocks = this.knocks.filter((x) => x !== k);
    this.ends.unshift({ kind: "knock", knock_id: k.knock_id, reason });
    this.viewers.get(k.peer_id)?.close();
    this.push();
  }

  failedPins(count: number, sources: number) {
    this.failures += count;
    this.ws?.send(JSON.stringify({ type: "alert", alert: { kind: "failed_pin", count, sources, from_at: Date.now() - 40_000, to_at: Date.now() - 5000 } }));
    this.push();
  }

  private onControl(m: { id: string; op: string; args: Record<string, unknown> }) {
    this.controls.push({ op: m.op, args: m.args });
    const reply = (result: Record<string, unknown>) => this.ws?.send(JSON.stringify({ type: "control.result", id: m.id, ok: true, result }));
    const fail = (code: string, details?: Record<string, unknown>) => this.ws?.send(JSON.stringify({ type: "control.result", id: m.id, ok: false, error: { code, message: code.replace(/_/g, " "), details } }));
    switch (m.op) {
      case "pause":
        this.latches.paused = true;
        this.phase = "paused";
        reply({});
        break;
      case "lock":
        this.latches.locked = true;
        reply({});
        break;
      case "mode":
        this.mode = String(m.args.mode);
        reply({});
        break;
      case "kick": {
        if (m.args.lock) this.latches.locked = true;
        const ok = this.kick(String(m.args.admission_id));
        if (ok) reply({ admission_ids: [m.args.admission_id], locked: !!m.args.lock });
        else fail("unknown_admission", m.args.lock ? { locked: true } : undefined);
        break;
      }
      case "knock.deny": {
        const k = this.knocks.find((x) => x.knock_id === m.args.knock_id);
        if (!k) {
          fail("unknown_knock");
          break;
        }
        this.knocks = this.knocks.filter((x) => x !== k);
        this.ends.unshift({ kind: "knock", knock_id: k.knock_id, reason: "denied" });
        this.viewers.get(k.peer_id)?.close();
        reply({});
        break;
      }
      default:
        fail("local_owner_required");
    }
    this.push();
  }

  state() {
    return {
      v: 1,
      stream_id: this.stream.id,
      instance: this.instance,
      seq: ++this.seq,
      phase: this.phase,
      mode: this.mode,
      e2ee: { status: "beta", epoch: 1 + this.ends.filter((e) => e.kind === "admission").length, suspended: false, revocation_not_durable: false },
      access: {
        pin: { required: this.pin.kind !== "none", kind: this.pin.kind, length: this.pin.length },
        pin_version: 1,
        rotation_pending: false,
        reconciling: false,
        latches: this.latches,
        knock: this.knockMode,
        failures: { stream: this.failures, limit: 20 },
      },
      admissions: this.admissions,
      knocks: this.knocks,
      ends: this.ends.slice(0, 100),
      health: { uplink_kbps: 2400, encode_ms: 6.4, fps: 15, quality_limitation: "none" },
    };
  }

  /** Coalesce to the server's 4/s SenderState budget; the latest state wins. */
  push() {
    if (this.pending) return;
    const wait = Math.max(0, this.lastSent + 300 - Date.now());
    this.pending = setTimeout(() => {
      this.pending = null;
      this.lastSent = Date.now();
      this.ws?.send(JSON.stringify({ type: "sender.state", state: this.state() }));
    }, wait);
  }

  async settle() {
    while (this.pending) await new Promise((r) => setTimeout(r, 50));
    await new Promise((r) => setTimeout(r, 1200)); // SSE viewer snapshots are at most 1/s
  }

  close() {
    if (this.heartbeat) clearInterval(this.heartbeat);
    for (const ws of this.viewers.values()) ws.close();
    this.ws?.close();
  }
}
