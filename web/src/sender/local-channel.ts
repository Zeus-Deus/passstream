// The local sender page's link to `passstream host` (CONTRACT §11): the
// mutual HMAC exchange, the launch-key → attachment-key handover, and
// reconnects. The page never sends a key: it only proves it holds one.

import { registerPublishReceiver } from "./publish-approval";

const LAUNCH = "ps:launch";
const ATTACH = "ps:attachment";
const LABEL = new TextEncoder().encode("PassStream v1 local auth");

type Key = { key_id: string; key: string };

export type PinPolicyWire = { kind: "digits"; length: number } | { kind: "passphrase" } | { kind: "none" };
export type LatchesWire = { locked: boolean; paused: boolean; frozen: boolean };
export interface Init {
  type: "init";
  attachment?: Key;
  server_url: string;
  signal_url: string;
  stream: { id: string; publish_token: string; view_link_base: string };
  policy: { pin_policy: PinPolicyWire; pin: string | null; knock: boolean; mode: "text" | "motion" };
  latches: LatchesWire;
  active_stream_ids: string[];
}
export interface Command {
  id: string;
  op: string;
  args: Record<string, unknown>;
}
export interface PublishPrompt {
 request_id: string; kind: "snapshot"|"recording"; service: string; destination: string;
 bytes: number; duration_ms: number; preview: string|null; expires_at: number;
}
export interface ChannelHandlers {
 demand?(until: number): void;
  /** Authenticated. `viaLaunchKey` is true when this init came from the launch-key exchange. */
  init(init: Init, viaLaunchKey: boolean): Promise<void>;
  command(c: Command): void;
  view(list: unknown): void;
  ended(reason: string): void;
  /** Terminal for this page: superseded, or never attached. */
  terminal(kind: "superseded" | "closed", detail: string): void;
  status(state: "connecting" | "attached", detail?: string): void;
}

function b64d(s: string): Uint8Array<ArrayBuffer> {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(b, (c) => c.charCodeAt(0));
}

function b64e(b: Uint8Array): string {
  return btoa(String.fromCharCode(...b))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

async function proof(key: Uint8Array<ArrayBuffer>, role: number, keyID: Uint8Array, np: Uint8Array, nd: Uint8Array) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const t = new Uint8Array(1 + LABEL.length + 16 + 32 + 32);
  t.set([role], 0);
  t.set(LABEL, 1);
  t.set(keyID, 1 + LABEL.length);
  t.set(np, 1 + LABEL.length + 16);
  t.set(nd, 1 + LABEL.length + 48);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, t));
}

function equal(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i];
  return d === 0;
}

function read(name: string): Key | null {
  try {
    const v = JSON.parse(sessionStorage.getItem(name) ?? "null");
    return v && typeof v.key_id === "string" && typeof v.key === "string" ? v : null;
  } catch {
    return null;
  }
}

/** Reads the fragment once, keeps the launch key for a reload during the handover, and clears the URL. */
export function takeLaunchFragment() {
  const q = new URLSearchParams(location.hash.slice(1));
  const i = q.get("i");
  const k = q.get("k");
  if (i && k) sessionStorage.setItem(LAUNCH, JSON.stringify({ key_id: i, key: k }));
  if (location.hash) history.replaceState(null, "", location.pathname + location.search);
}

export class LocalChannel {
  #ws?: WebSocket;
  private authed = false;
  private done = false;
  private backoff = 100;
  private readonly started = Date.now();
  private acks = new Map<number, () => void>();
  private ownerReplies = new Map<string, (r: { ok: boolean; data?: Record<string, unknown>; error?: { code: string } }) => void>();
  private prompt: PublishPrompt|null = null;
  private promptListeners = new Set<() => void>();
  getPublishPrompt = () => this.prompt;
  subscribePublish = (f:()=>void) => {this.promptListeners.add(f);return ()=>{this.promptListeners.delete(f);};};
  refusePublish(id: string) { this.#send({ type: "publish_decision", request_id: id, approved: false }); }
  private recordingReplies = new Map<string, (ok: boolean) => void>();

  constructor(private readonly h: ChannelHandlers) {
    registerPublishReceiver(this, () => this.prompt?.request_id, (id) =>
      this.#send({ type: "publish_decision", request_id: id, approved: true }));
  }

  get attached(): boolean {
    return this.authed && this.#ws?.readyState === WebSocket.OPEN;
  }

  start() {
    this.connect();
  }

  #send(v: unknown): boolean {
    if (!this.attached) return false;
    this.#ws!.send(JSON.stringify(v));
    return true;
  }
  /** Plaintext artifacts have exactly one local authenticated sink. */
  sendCapture(v: unknown): boolean {
    if (typeof v !== "object" || v === null || !("type" in v) || v.type !== "capture") return false;
    if (!("status" in v) || !["paused", "idle", "too_large", "stale"].includes(String(v.status))) return false;
    // Privacy markers must not wait behind a large PNG. Construct the envelope
    // here: callers cannot smuggle an approval through type getters/toJSON.
    return this.#send({ type: "capture", status: String(v.status) });
  }
  sendArtifact(v: unknown): Promise<boolean> {
    if (typeof v !== "object" || v === null) return Promise.resolve(false);
    const value = v as Record<string, unknown>;
    const type = value.type;
    if (type !== "capture" && type !== "recording") return Promise.resolve(false);
    const artifact = type === "capture"
      ? { type, status: "frame", id: value.id, width: value.width, height: value.height, data: value.data }
      : { type, id: value.id, data: value.data, final: value.final, duration_ms: value.duration_ms };
    const ack = crypto.randomUUID();
    return new Promise((resolve) => {
      const timer = setTimeout(() => { this.recordingReplies.delete(ack); resolve(false); }, 3000);
      this.recordingReplies.set(ack, (ok) => { clearTimeout(timer); resolve(ok); });
      if (!this.#send({ ...artifact, ack })) {
        this.recordingReplies.get(ack)?.(false); this.recordingReplies.delete(ack);
      }
    });
  }
  ownerAction(op: string, args: Record<string, unknown> = {}): Promise<{ ok: boolean; data?: Record<string, unknown>; error?: { code: string } }> {
    const id = crypto.randomUUID();
    return new Promise((resolve) => {
      const timer = setTimeout(() => { this.ownerReplies.delete(id); resolve({ ok: false, error: { code: "sender_timeout" } }); }, 55_000);
      this.ownerReplies.set(id, (r) => { clearTimeout(timer); resolve(r); });
      if (!this.#send({ type: "owner_action", id, op, args })) {
        this.ownerReplies.get(id)?.({ ok: false, error: { code: "sender_offline" } });
        this.ownerReplies.delete(id);
      }
    });
  }

  /** Report state; resolves when the daemon acked that seq (fsynced its latch mirror), or after 1 s. */
  sendState(state: { seq: number }): Promise<boolean> {
    if (!this.#send({ type: "state", state })) return Promise.resolve(false);
    return new Promise((resolve) => {
      const t = setTimeout(() => {
        this.acks.delete(state.seq);
        resolve(false);
      }, 1000);
      this.acks.set(state.seq, () => {
        clearTimeout(t);
        resolve(true);
      });
    });
  }
  sendSecrets(s: { pin: string | null; pending_pin: string | null; link: string }) {
    this.#send({ type: "secrets", pin: s.pin, pending_pin: s.pending_pin, link: s.link });
  }
  sendResult(id: string, r: { ok: true; data: unknown } | { ok: false; error: unknown }) {
    this.#send(r.ok ? { type: "result", id, ok: true, data: r.data } : { type: "result", id, ok: false, error: r.error });
  }
  sendAlert(alert: unknown) {
    this.#send({ type: "alert", alert });
  }
  latchCleared(latch: "locked" | "paused" | "frozen") {
    this.#send({ type: "latch_cleared", latch });
  }
  sendEnded(reason: string) {
    this.#send({ type: "ended", reason });
  }
  stop() {
    this.done = true;
    this.#ws?.close();
  }

  private retry(why: string) {
    if (this.done) return;
    if (!read(ATTACH) && Date.now() - this.started > 60_000) {
      this.done = true;
      this.h.terminal("closed", "Open PassStream from the bar or the CLI.");
      return;
    }
    this.h.status("connecting", why);
    setTimeout(() => this.connect(), this.backoff);
    this.backoff = Math.min(this.backoff * 2, 1000);
  }

  private connect() {
    if (this.done) return;
    const viaLaunch = !read(ATTACH);
    const k = read(ATTACH) ?? read(LAUNCH);
    if (!k) {
      this.done = true;
      this.h.terminal("closed", "Open PassStream from the bar or the CLI.");
      return;
    }
    const keyID = b64d(k.key_id);
    const key = b64d(k.key);
    const np = crypto.getRandomValues(new Uint8Array(32));
    const ws = new WebSocket(`ws://${location.host}/local/v1/sender`);
    this.#ws = ws;
    this.authed = false;
    let verified = false;
    let initDone = false;
    ws.onopen = () => ws.send(JSON.stringify({ type: "hello", protocol: 1, key_id: k.key_id, nonce: b64e(np) }));
    ws.onclose = () => {
      this.prompt=null;this.promptListeners.forEach((f)=>f());
      if (this.#ws === ws) this.authed = false;
      if (!this.done && this.#ws === ws) this.retry(verified ? "reconnecting" : "waiting for the daemon");
    };
    let chain = Promise.resolve();
    ws.onmessage = (ev) => {
      chain = chain.then(() => this.message(ws, ev, keyID, key, np, viaLaunch, () => verified, (v) => (verified = v), () => initDone, (v) => (initDone = v)));
    };
  }

  private async message(
    ws: WebSocket,
    ev: MessageEvent,
    keyID: Uint8Array<ArrayBuffer>,
    key: Uint8Array<ArrayBuffer>,
    np: Uint8Array,
    viaLaunch: boolean,
    verified: () => boolean,
    setVerified: (v: boolean) => void,
    initDone: () => boolean,
    setInitDone: (v: boolean) => void,
  ) {
    if (this.#ws !== ws || this.done) return;
    let m: any;
    try {
      m = JSON.parse(String(ev.data));
    } catch {
      ws.close();
      return;
    }
    if (m.type === "error") {
      // Before proof_d verifies, anyone holding the port could send this.
      if (verified() && ["auth_failed", "superseded", "protocol_mismatch"].includes(m.code)) {
        this.done = true;
        this.h.terminal("superseded", "This window is no longer the sender.");
      }
      ws.close();
      return;
    }
    if (!verified()) {
      if (m.type !== "challenge") return ws.close();
      const nd = b64d(String(m.nonce ?? ""));
      const want = await proof(key, 1, keyID, np, nd);
      if (nd.length !== 32 || !equal(want, b64d(String(m.proof ?? "")))) return ws.close();
      setVerified(true);
      ws.send(JSON.stringify({ type: "auth", proof: b64e(await proof(key, 2, keyID, np, nd)) }));
      return;
    }
    if (!initDone()) {
      if (m.type !== "init") return ws.close();
      setInitDone(true);
      this.backoff = 100;
      if (m.attachment) {
        // Store the attachment key before acking; the launch key is retired on the ack.
        sessionStorage.setItem(ATTACH, JSON.stringify(m.attachment));
        ws.send(JSON.stringify({ type: "init_ack" }));
        sessionStorage.removeItem(LAUNCH);
      }
      this.authed = true;
      this.h.status("attached", m.stream?.id);
      try {
        await this.h.init(m as Init, viaLaunch || !!m.attachment);
      } catch (e) {
        console.error("sender init", String(e));
      }
      return;
    }
    switch (m.type) {
      case "capture_demand": this.h.demand?.(Number(m.until));break;
      case "publish_prompt": this.prompt=m.request;this.promptListeners.forEach((f)=>f());break;
      case "artifact_ack":
        this.recordingReplies.get(String(m.ack))?.(m.ok === true);
        this.recordingReplies.delete(String(m.ack));
        break;
      case "owner_result":
        this.ownerReplies.get(String(m.id))?.(m);
        this.ownerReplies.delete(String(m.id));
        break;
      case "command":
        this.h.command({ id: String(m.id), op: String(m.op), args: m.args && typeof m.args === "object" ? m.args : {} });
        break;
      case "state_ack":
        this.acks.get(Number(m.seq))?.();
        this.acks.delete(Number(m.seq));
        break;
      case "view":
        this.h.view(m.list);
        break;
      case "ended":
        this.done = true;
        sessionStorage.removeItem(ATTACH);
        this.h.ended(String(m.reason ?? "stopped"));
        ws.close();
        break;
    }
  }
}
