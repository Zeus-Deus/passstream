/**
 * The one sender: capture → SenderHost (PIN, identity, link secret, page
 * recovery in IndexedDB) → SFrame on the publisher PC, with the E2EE relay
 * on the signaling WebSocket and the host-control command rules.
 *
 * Mounted twice: at /sender behind the local daemon (`kind: "local"`, the
 * production Omarchy path), and at /host as the server-served "trusted
 * server" browser sender (HOST-CONTROL §2.1), which the torture suite drives.
 */
import { SENDER_TEST_HOOKS } from "./test-hooks";
import {
  AlertBatcher,
  E2EEError,
  HostRecovery,
  SenderIdentity,
  assertE2EESupported,
  attachTransform,
  buildFragment,
  fromBase64Url,
  generateLinkSecret,
  generatePin,
  isE2EEError,
  postSenderCutoff,
  postSenderEpoch,
  postSenderFence,
  postSenderFenceOpen,
  postSenderSuspend,
  toBase64Url,
  type KnockInfo,
  type Latches,
  type PinPolicy,
  type ViewerInfo,
} from "@passstream/e2ee";
import TransformWorker from "@passstream/e2ee/transform-worker?worker";
import { AUDIO_TRACK, addAudioSender, audioTransceiver } from "../audio";
import { Session } from "../session";
import { statusSource, stopTestSource } from "../source";
import { IndexedDBStorage } from "./idb-storage";
import { checkLocal, checkRemote, fail, parsePolicy, type Result } from "./authority";
import { CommandCache } from "./command-cache";
import { senderHealth } from "./health";
import { HostAnnotations } from "../annotations/host";
import type { LocalChannel } from "./local-channel";
import { OwnerCapture } from "./owner-capture";
import { Redactor, safeModeSupported } from "../redact/redactor";

export type Phase = "starting" | "picking" | "live" | "paused" | "ended";
export type Channel = "local" | "remote" | "window";
export type { Result };

export interface SenderConfig {
  kind: "local" | "browser";
  stream: { id: string; publish_token: string; view_link_base: string };
  signalUrl: string;
  serverUrl: string;
  /** Used only to create the recovery record on a first launch. */
  policy: { pin_policy: PinPolicy; pin: string | null; knock: boolean; mode: "text" | "motion"; safe?: boolean };
  latches: Latches;
  /** False when the record must already exist (an attachment-key init). */
  mayCreate: boolean;
  activeStreamIds?: string[];
  channel?: LocalChannel;
}

type End =
  | { kind: "admission"; admission_id: string; viewer_id: string; cause: "kicked" | "revoked" | "stream_ended"; at: number }
  | { kind: "knock"; knock_id: string; reason: string; at: number };

const ID22 = /^[A-Za-z0-9_-]{22}$/;
/** A sender record unseen this long is abandoned and pruned (stream end deletes it at once). */
export const ABANDONED_MS = 7 * 24 * 3600_000;
const ROW = /^[ACDEFHJKMNPRTUVWXY3479]{3}$/;
const KNOCK_ENDS = new Set(["denied", "timeout", "peer_closed", "replaced", "pin_rotated", "stopped", "commit_failed", "queue_full"]);

function id22(): string {
  return toBase64Url(crypto.getRandomValues(new Uint8Array(16)));
}
function errorResult(e: unknown, details?: Record<string, unknown>): Result {
  if (isE2EEError(e)) {
    const known = ["invalid_argument", "passphrase_required", "unknown_admission", "unknown_knock", "code_mismatch", "rotation_pending", "conflict", "storage"];
    const code = known.includes(e.code) ? e.code : e.code === "state_mismatch" ? "conflict" : "internal";
    const d = { ...(e.details ?? {}), ...(details ?? {}) } as Record<string, unknown>;
    // Only allowlisted, non-secret detail fields leave the page.
    const safe: Record<string, unknown> = {};
    for (const k of ["pending", "applied", "locked", "reason"]) if (k in d) safe[k] = d[k];
    return fail(code, e.message.replace(/^\w+: /, ""), Object.keys(safe).length ? safe : undefined);
  }
  return fail("internal", "sender error", details);
}
export class SenderCore {
  features?: OwnerCapture;
  readonly instance = id22();
  phase: Phase = "starting";
  mode: "text" | "motion";
  /** Safe mode (FEATURES.md "Safe mode"): redact sensitive data before encoding. On unless turned off locally. */
  safe: boolean;
  /** The redaction engine while safe mode is on and a capture exists. */
  redactor?: Redactor;
  /** Set when safe mode is on but can't run: nothing but the paused picture is sent. */
  safeError = "";
  recovery!: HostRecovery;
  session?: Session;
  capture: MediaStream | null = null;
  link = "";
  error = "";
  endedReason = "";
  drops: Record<string, number> = {};
  /** E2EE annotations router (FEATURES.md "Track D2" §3): host-only until a local command allows viewers. */
  annotations!: HostAnnotations;
  /**
   * Curtain timing (diagnostics, e2e): Date.now() when a pause command
   * reached this page and when the capture track was disabled.
   */
  readonly curtain = { commandAt: 0, appliedAt: 0, fenceAt: 0, fenceWorkerAt: 0, fenceOpenAt: 0 };
  private storage!: IndexedDBStorage;
  private worker!: Worker;
  /** SFrame tracks with an encrypt transform in the current worker (0 native video, 1 audio, 2 thumbnail). */
  private attached = new Set<number>();
  /** Captured tab/system audio, if the browser provided it. */
  audio: MediaStreamTrack | null = null;
  /**
   * Test-only (`?test&audio_mline` under automation): negotiate the silent
   * audio transceiver even without captured audio, for the bad-Wi-Fi A/B.
   */
  private readonly forceAudioMline =
    SENDER_TEST_HOOKS && new URLSearchParams(location.search).has("audio_mline");
  private seq = 0;
  private ends: End[] = [];
  private lastAdmissions = new Map<string, string>(); // admission id → viewer id
  private kickedIds = new Set<string>();
  private failures = 0;
  private results = { local: new CommandCache(), remote: new CommandCache() };
  private timers: ReturnType<typeof setInterval>[] = [];
  private stateTimer?: ReturnType<typeof setTimeout>;
  private lastStateAt = 0;
  private paused?: MediaStream;
  private listeners = new Set<() => void>();
  private releaseLock?: () => void;
  private alerts: AlertBatcher;
  private framesEncoded = 0;
  /**
   * Test-only: every epoch secret this page activated, so the e2e suite can
   * check SFrame ciphertexts and scan logs for real keys. Only under browser
   * automation on a `?test` page in a `--mode e2e` build; null otherwise.
   */
  readonly testSecrets: { epochs: { epoch: number; secret: string }[] } | null =
    SENDER_TEST_HOOKS ? { epochs: [] } : null;

  private constructor(readonly cfg: SenderConfig) {
    this.mode = cfg.policy.mode;
    // Test-only (`?test&safe=0` in an e2e build): synthetic timing sources change
    // every frame and would stay covered (fail-closed), breaking frame decoding.
    this.safe = (cfg.policy.safe ?? true) && !(SENDER_TEST_HOOKS && new URLSearchParams(location.search).get("safe") === "0");
    this.alerts = new AlertBatcher({
      onFlush: (b) => {
        if (b.failures > 0) this.alert({ kind: "failed_pin", count: b.failures, sources: b.sources, from_at: b.firstAt, to_at: b.lastAt });
      },
    });
  }

  get streamId() {
    return this.cfg.stream.id;
  }
  get host() {
    return this.recovery.host;
  }
  get latches(): Latches {
    return this.recovery.record.latches;
  }
  get policy(): PinPolicy {
    return this.recovery.record.pin_state?.policy ?? { kind: "digits", length: 4 };
  }
  get pin(): string | null {
    return this.recovery.record.pin_state?.pin ?? null;
  }
  get pendingPin(): string | null {
    return this.recovery.record.pending_rotation?.pin ?? null;
  }
  viewers(): ViewerInfo[] {
    return this.host.viewers();
  }
  knocks(): KnockInfo[] {
    return this.host.knocks();
  }
  subscribe(f: () => void): () => void {
    this.listeners.add(f);
    return () => void this.listeners.delete(f);
  }
  private changed() {
    this.features?.sync();
    for (const f of this.listeners) f();
    this.scheduleState();
  }

  /** The startup order (HOST-CONTROL §5.3), from loading the record to joining /signal. */
  static async start(cfg: SenderConfig): Promise<SenderCore> {
    const core = new SenderCore(cfg);
    await core.boot();
    if (cfg.channel) core.features = new OwnerCapture(
      () => ({ live: core.phase === "live", paused: core.latches.paused, suspended: core.host.keys.suspended, attached: cfg.channel!.attached }),
      (v) => cfg.channel!.sendCapture(v),
      (v) => cfg.channel!.sendArtifact(v),
    );
    return core;
  }

  private async boot() {
    assertE2EESupported(); // the sender fails closed too
    this.storage = await IndexedDBStorage.open();
    await this.takeLock();
    const id = this.streamId;
    if (this.cfg.activeStreamIds) await this.cleanup(this.cfg.activeStreamIds);
    // Abandoned records (a tab closed without ending its stream) go after a week unseen.
    await this.storage.pruneIdle(this.streamId, ABANDONED_MS).catch(() => undefined);
    // A first launch writes the whole record at once: secrets, PIN state, policy.
    const key = `ps:stream:${id}`;
    const existing = await this.storage.get(key);
    if (existing === undefined) {
      if (!this.cfg.mayCreate) throw new E2EEError("state_mismatch", "state_lost");
      const p = this.cfg.policy.pin_policy;
      let pin: string | null = null;
      if (p.kind === "digits") pin = generatePin(p.length);
      else if (p.kind === "passphrase") {
        if (!this.cfg.policy.pin) throw new E2EEError("passphrase_required", "a passphrase stream needs a locally supplied passphrase");
        pin = this.cfg.policy.pin.normalize("NFC");
      }
      const capability = new URL(this.cfg.stream.view_link_base).hash.replace(/^#c=/, "");
      const identity = SenderIdentity.generate();
      await this.storage.update(key, (cur) =>
        cur !== undefined
          ? cur
          : {
              v: 1,
              stream_id: id,
              server_url: this.cfg.serverUrl,
              view_capability: capability,
              identity_seed: toBase64Url(identity.exportSecretKey()),
              link_secret: toBase64Url(generateLinkSecret()),
              created_at: Date.now(),
              pin_state: { version: 0, policy: p, pin },
              pending_rotation: null,
              knock: this.cfg.policy.knock,
              latches: { ...this.cfg.latches },
            },
      );
    }
    const rec = (await this.storage.get(key)) as Record<string, unknown> | undefined;
    if (!rec || typeof rec.identity_seed !== "string" || typeof rec.link_secret !== "string" || typeof rec.view_capability !== "string")
      throw new E2EEError("state_mismatch", "state_lost");
    const identity = SenderIdentity.fromSecretKey(fromBase64Url(rec.identity_seed));
    const linkSecret = fromBase64Url(rec.link_secret);
    const base = new URL(this.cfg.stream.view_link_base);
    this.link = `${base.origin}/v#${buildFragment(rec.view_capability, identity.publicKey, linkSecret)}`;
    this.worker = this.newWorker();
    // Latches are merged (page OR init OR library freeze) and applied by
    // HostRecovery before the host exists, so nothing is admitted or sent first.
    this.recovery = await HostRecovery.create({
      storage: this.storage,
      streamId: id,
      init: { knock: this.cfg.policy.knock, latches: this.cfg.latches },
      host: {
        identity,
        linkSecret,
        send: (peer, bytes) => {
          this.session?.sendSignal({ type: "e2ee", to: peer, b: toBase64Url(bytes) });
        },
        onEpoch: (k) => {
          this.testSecrets?.epochs.push({ epoch: k.epoch, secret: toBase64Url(k.secret) });
          postSenderEpoch(this.worker, k);
        },
        onSuspend: () => { postSenderSuspend(this.worker); this.features?.reset(); },
        onKeyframeNeeded: () => this.worker.postMessage({ type: "keyframe" }),
        // Synchronous at kick time: the worker enforces the 300 ms cutoff itself.
        onCutoff: (c) => postSenderCutoff(this.worker, c),
      },
      onLatches: (l) => this.applyLatches(l),
      onMismatch: () => this.alert({ kind: "state_mismatch" }),
      onStateChange: () => this.changed(),
    });
    this.wireHost();
    this.annotations = new HostAnnotations({
      onAnnotation: (f) => this.host.on("annotation", f),
      onAdmitted: (f) => this.host.on("admitted", f),
      onRevoked: (f) => this.host.on("annotationRevoked", f),
      allowed: (id) => this.host.annotationAllowed(id),
      recipients: () => this.viewers().filter((v) => v.connected).map((v) => ({ admissionId: v.admissionId, rowCode: v.rowCode })),
      sendAnnotations: (id, records, current) => this.host.sendAnnotations(id, records, current),
    });
    this.annotations.start();
    void this.storage.touch(id).catch(() => undefined);
    this.timers.push(setInterval(() => void this.storage.touch(id).catch(() => undefined), 600_000));
    this.timers.push(setInterval(() => void this.recovery.tick().catch(() => undefined), 250));
    this.timers.push(setInterval(() => void this.host.keys.tick(), 50));
    this.timers.push(setInterval(() => this.heartbeat(), 1000));
    this.applyLatches(this.latches);
    this.joinSignal();
  }

  private async takeLock() {
    if (!navigator.locks) return;
    await new Promise<void>((resolve, reject) => {
      void navigator.locks.request(`ps:sender:${this.streamId}`, { ifAvailable: true }, (lock) => {
        if (!lock) {
          reject(new E2EEError("conflict", "another window is already the sender"));
          return;
        }
        resolve();
        return new Promise<void>((release) => (this.releaseLock = release));
      });
    });
  }

  private async cleanup(active: string[]) {
    for (const s of await this.storage.streams()) if (s !== this.streamId && !active.includes(s)) await this.storage.deleteStream(s);
  }

  private newWorker(): Worker {
    const w = new TransformWorker();
    w.onmessage = (ev: MessageEvent) => {
      const m = ev.data as { type: string; reason?: string; count?: number; message?: string };
      if (m.type === "drop") this.drops[m.reason ?? "?"] = (this.drops[m.reason ?? "?"] ?? 0) + (m.count ?? 1);
      if (m.type === "error") console.error("transform worker", m.message);
      if (m.type === "trace") this.traceReply?.((m as unknown as { frames: unknown[] }).frames);
      if (m.type === "fence") this.curtain.fenceWorkerAt = Number((m as unknown as { at: number }).at);
    };
    if (this.tracing) w.postMessage({ type: "trace", on: true });
    return w;
  }

  /** Diagnostics (torture suite): per-frame encrypt timings and sizes from the worker. */
  private tracing = false;
  private traceReply?: (frames: unknown[]) => void;
  trace(on: boolean) {
    this.tracing = on;
    this.worker?.postMessage({ type: "trace", on });
  }
  dumpTrace(): Promise<unknown[]> {
    return new Promise((resolve) => {
      this.traceReply = resolve;
      this.worker.postMessage({ type: "trace-dump" });
    });
  }

  private wireHost() {
    const host = this.host;
    host.on("kicked", (e) => {
      for (const peer of e.peerIds) this.session?.sendSignal({ type: "e2ee.close", to: peer });
      for (const a of e.admissionIds) {
        this.kickedIds.add(a);
        this.addEnd({ kind: "admission", admission_id: a, viewer_id: this.lastAdmissions.get(a) ?? "", cause: "kicked", at: Date.now() });
      }
      this.changed();
    });
    host.on("knock", () => this.changed());
    host.on("knockEnded", (e) => {
      const reason = KNOCK_ENDS.has(e.reason) ? e.reason : "commit_failed";
      this.addEnd({ kind: "knock", knock_id: e.knockId, reason, at: Date.now() });
      this.alert({ kind: "knock_ended", knock_id: e.knockId, reason });
      this.changed();
    });
    host.on("admitted", () => this.changed());
    host.on("disconnected", () => this.changed());
    host.on("rejected", () => this.changed());
    host.on("revocationDurability", (e) => {
      this.alert({ kind: "revocation_durability", not_durable: e.notDurable });
      this.changed();
    });
    host.budget.on("failure", (e) => {
      this.failures = e.streamFailures;
      this.alerts.failure(e.viewerSid, e.at);
      this.changed();
    });
    host.budget.on("frozen", (e) => {
      this.failures = e.streamFailures;
      this.alerts.frozen(e.at);
      this.alert({ kind: "frozen", failures: e.streamFailures });
      this.changed();
    });
    host.budget.on("unlocked", () => {
      this.failures = 0;
      this.changed();
    });
    void host.budget.status().then((s) => {
      this.failures = s.streamFailures;
    });
  }

  private addEnd(e: End) {
    this.ends.unshift(e);
    const cutoff = Date.now() - 300_000;
    this.ends = this.ends.filter((x) => x.at >= cutoff).slice(0, 100);
  }

  private joinSignal() {
    const video = document.createElement("video");
    this.session = new Session(
      "publish",
      this.streamId,
      this.cfg.stream.publish_token,
      video,
      () => this.onMedia(),
      undefined,
      {
        signalUrl: this.cfg.signalUrl,
        join: { protocol: 1, sender_kind: this.cfg.kind, instance: this.instance },
        deferMedia: true,
        onMessage: (m) => this.onSignal(m),
        encrypt: (sender, track) => this.encrypt(sender, "video", track),
        extend: (pc) => this.addAudio(pc),
        onTerminal: (code) => void this.end(code === "stream_ended" ? "deleted" : "state_lost", false),
      },
    );
    window.passstream = this.session;
  }

  /**
   * Each new publisher PC gets encrypt transforms (native, thumbnail, then audio) on one
   * worker; a track attached twice means a new PC, which needs a fresh worker
   * and a fresh epoch (PROTOCOL §7.1: never re-post an epoch, nonce reuse).
   */
  private encrypt(sender: RTCRtpSender, kind: "video" | "audio", track = kind === "audio" ? AUDIO_TRACK : 0) {
    let replaced = false;
    if (this.attached.has(track)) {
      this.worker.terminate();
      this.worker = this.newWorker();
      this.attached.clear();
      replaced = true;
    }
    attachTransform(sender as unknown as { transform: unknown }, this.worker, { role: "encrypt", kind, track, codec: kind === "audio" ? "audio/opus" : "video/vp9" });
    this.attached.add(track);
    // Never re-post an old epoch: the new worker gets a fresh one within 300 ms.
    if (replaced) void this.host.replaceEncryptor().catch(() => undefined);
  }

  /**
   * The audio transceiver of a publisher PC, encrypted before any track: on
   * a new PC (session `extend`) and when audio first appears on a recapture
   * (`extendCurrent`, renegotiated). A capture without audio (the usual Linux
   * screen share) negotiates no audio m-line at all.
   */
  private async addAudio(pc: RTCPeerConnection) {
    if ((!this.liveAudio() && !this.forceAudioMline) || audioTransceiver(pc)) return;
    await addAudioSender(pc, this.capture ?? new MediaStream(), this.liveAudio(), (s) => this.encrypt(s, "audio"));
  }

  /** The current PC's audio sender, only if its encrypt transform is attached. */
  private audioSender(): RTCRtpSender | undefined {
    const pc = this.session?.pc;
    const s = pc ? audioTransceiver(pc)?.sender : undefined;
    return s && (s as unknown as { transform?: unknown }).transform ? s : undefined;
  }

  /**
   * The audio track on the wire. Pause mutes it by disabling the track
   * (Chrome sends tiny silence frames, ~9 kbit/s with SFrame), not by
   * detaching it: Chrome restarting a stopped audio sender broke the SFU's
   * receive stream for good (measured: packets sent, none received after
   * resume, same SSRC).
   */
  private liveAudio(): MediaStreamTrack | null {
    return this.audio && this.audio.readyState === "live" && this.phase !== "ended" ? this.audio : null;
  }

  private onSignal(m: Record<string, unknown>) {
    switch (m.type) {
      case "joined":
        if (this.phase === "starting") {
          this.phase = this.latches.paused ? "paused" : "picking";
          this.changed();
          this.sendSecrets();
        }
        break;
      case "e2ee": {
        let bytes: Uint8Array;
        try {
          bytes = fromBase64Url(String(m.b ?? ""));
        } catch {
          return;
        }
        if (typeof m.peer === "string") void this.host.receive(m.peer, bytes).catch(() => undefined);
        break;
      }
      case "e2ee.peer_closed":
        if (typeof m.peer === "string") void this.host.peerClosed(m.peer).catch(() => undefined);
        break;
      case "e2ee.peers": {
        const live = new Set(Array.isArray(m.peers) ? (m.peers as string[]) : []);
        const known = new Set([...this.viewers().filter((v) => v.connected).map((v) => v.peerId), ...this.knocks().map((k) => k.peerId)]);
        for (const p of known) if (!live.has(p)) void this.host.peerClosed(p).catch(() => undefined);
        break;
      }
      case "control": {
        const id = String(m.id ?? "");
        const op = String(m.op ?? "");
        const args = m.args && typeof m.args === "object" && !Array.isArray(m.args) ? (m.args as Record<string, unknown>) : {};
        // control.result carries `result` (CONTRACT §8.5); the daemon socket uses `data`.
        void this.command("remote", id, op, args).then((r) =>
          this.session?.sendSignal(r.ok ? { type: "control.result", id, ok: true, result: r.data } : { type: "control.result", id, ok: false, error: r.error }),
        );
        break;
      }
      case "stream.ended":
        void this.end(m.reason === "expired" ? "expired" : "deleted", true);
        break;
    }
  }

  private onMedia() {
    const q = this.session?.quality;
    const encoded = Number(q?.stats?.framesEncoded ?? 0);
    if (encoded > this.framesEncoded) {
      this.framesEncoded = encoded;
      if (this.phase === "picking" && this.capture && !this.latches.paused) {
        this.phase = "live";
        this.changed();
      }
    }
  }

  // ---------------------------------------------------------------- capture

  /** Start capture (the window's Share or Test-source button: a user gesture). */
  async share(stream: MediaStream) {
    if (this.phase === "ended") {
      stream.getTracks().forEach((t) => t.stop());
      return;
    }
    // Tab/system audio travels E2EE on track 1; extra audio tracks are dropped.
    for (const a of stream.getAudioTracks().slice(1)) {
      stream.removeTrack(a);
      a.stop();
    }
    this.capture?.getTracks().forEach((t) => t.stop());
    this.capture = stream;
    this.redact(false);
    this.audio = stream.getAudioTracks()[0] ?? null;
    // Muted before it is attached: a paused stream never sends a frame of it.
    if (this.audio) this.audio.enabled = !this.latches.paused;
    const audioSender = this.audioSender();
    if (audioSender) void audioSender.replaceTrack(this.liveAudio()).catch(() => undefined);
    else if (this.liveAudio()) void this.session?.extendCurrent((pc) => this.addAudio(pc));
    const track = stream.getVideoTracks()[0];
    track.contentHint = this.mode === "motion" ? "motion" : "text";
    if (this.redactor) this.redactor.track.contentHint = track.contentHint;
    track.addEventListener("ended", () => {
      if (this.capture === stream) void this.end("stopped", true);
    });
    // Latches were restored before capture: a paused stream sends the paused picture.
    this.applyLatches(this.latches);
    this.session?.startMedia(stream);
    if (this.mode === "motion") void this.session?.mode(true);
    if (!this.latches.paused && this.phase === "paused") this.phase = "picking";
    this.changed();
  }

  /**
   * The video that leaves this page: the redacted track while safe mode is
   * on (FEATURES.md "Safe mode"), else the capture. Safe mode on but unable
   * to run sends the paused picture: never the unredacted screen.
   */
  private live(): MediaStreamTrack | null {
    const raw = this.capture?.getVideoTracks()[0] ?? null;
    if (!raw || !this.safe) return raw;
    if (this.redactor) return this.redactor.track;
    this.blocked ??= statusSource("paused", raw.getSettings());
    return this.blocked.getVideoTracks()[0];
  }
  /** The picture sent while safe mode is on but can't run. */
  private blocked?: MediaStream;

  /** (Re)build redaction for the current capture and policy, and switch every consumer to `live()`. */
  private redact(apply = true) {
    const raw = this.capture?.getVideoTracks()[0];
    const want = this.safe && !!raw && raw.readyState === "live";
    if (this.redactor && (!want || this.redactor.source !== raw)) {
      this.redactor.stop();
      this.redactor = undefined;
    }
    this.safeError = "";
    if (this.blocked) {
      stopTestSource(this.blocked);
      this.blocked.getTracks().forEach((t) => t.stop());
      this.blocked = undefined;
    }
    if (want && !this.redactor) {
      try {
        if (!safeModeSupported()) throw new Error("unsupported");
        this.redactor = new Redactor(raw!);
        this.redactor.track.contentHint = raw!.contentHint;
        this.redactor.track.enabled = !this.latches.paused;
        this.redactor.on((m) => m.type === "stats" && this.changed());
      } catch {
        this.safeError = "Safe mode can't run in this browser. Turn it off to share.";
      }
    }
    // The host's preview, local snapshots and recordings see what viewers see.
    const live = this.live();
    this.preview = this.capture && live ? (live === raw ? this.capture : new MediaStream([live, ...this.capture.getAudioTracks()])) : null;
    if (this.preview) this.features?.source(this.preview);
    if (apply) this.applyLatches(this.latches);
    this.changed();
  }

  /** What the host's own preview shows: what viewers get. */
  preview: MediaStream | null = null;

  /** Pause covers the screen and audio independently of encryption (HOST-CONTROL §5.6). */
  private applyLatches(l: Latches) {
    this.features?.sync();
    const track = this.capture?.getVideoTracks()[0] ?? null;
    // Pause mutes audio (HOST-CONTROL §5.6): a disabled track sends silence.
    if (this.audio) this.audio.enabled = !l.paused;
    if (l.paused) {
      if (track) track.enabled = false;
      if (this.redactor) this.redactor.track.enabled = false;
      let fencing = false;
      if (track && !this.curtain.appliedAt) {
        this.curtain.appliedAt = Date.now();
        this.curtain.fenceAt = performance.timeOrigin + performance.now();
        // Capture/audio are disabled above at the accepted command. The worker
        // fences enqueue only once it handles this message; pre-command frames
        // can finish encryption before receipt, and downstream packets remain.
        // An armed fence also rejects encryption already awaiting WebCrypto.
        if (this.worker) postSenderFence(this.worker);
        fencing = true;
      }
      // Sized so it never matches the capture's encoded size: the swap forces a keyframe.
      this.paused ??= statusSource("paused", track?.getSettings());
      const swap = this.session?.setOutgoing(this.paused.getVideoTracks()[0]);
      if (fencing)
        void Promise.resolve(swap).finally(() => {
          this.curtain.fenceOpenAt = performance.timeOrigin + performance.now();
          if (this.worker) postSenderFenceOpen(this.worker);
        });
      if (this.phase === "live" || this.phase === "picking") this.phase = "paused";
    } else {
      this.curtain.appliedAt = 0;
      const live = this.live();
      // The capture is enabled only while it is what gets sent. In safe mode it
      // is disabled before the swap: a sender not yet switched (or refusing the
      // switch) sends black, never the screen. The redactor reads its own copy.
      if (track) track.enabled = live === track;
      if (this.redactor) this.redactor.track.enabled = true;
      void this.session?.setOutgoing(live).then(async (ok) => {
        if (ok || live === track) return;
        this.safeError = "Couldn't switch the stream to safe mode, so the screen is hidden.";
        await this.session?.setOutgoing(null);
        this.changed();
      });
      if (this.paused) {
        stopTestSource(this.paused);
        this.paused.getTracks().forEach((t) => t.stop());
        this.paused = undefined;
      }
      if (this.phase === "paused") this.phase = this.capture && this.framesEncoded > 0 ? "live" : "picking";
    }
    for (const f of this.listeners) f();
    this.scheduleState();
  }

  // ---------------------------------------------------------------- commands

  /**
   * Restrictive commands run from any channel; permissive ones only from the
   * local daemon or this window (HOST-CONTROL §4). Arguments are closed.
   */
  async command(channel: Channel, id: string, op: string, args: Record<string, unknown>): Promise<Result> {
    if (op === "pause" && !this.latches.paused) this.curtain.commandAt = Date.now();
    const exec = async (): Promise<Result> => {
      try {
        return channel === "remote" ? await this.remote(op, args) : await this.local(op, args, channel);
      } catch (e) {
        return errorResult(e);
      }
    };
    // Ids are keyed with the op and arguments, and only terminal outcomes stay (command-cache.ts).
    const cache = channel === "window" ? null : this.results[channel];
    return cache && id ? cache.run(id, op, args, exec) : exec();
  }

  private async remote(op: string, args: Record<string, unknown>): Promise<Result> {
    const refused = checkRemote(op, args);
    return refused ?? this.run(op, args, "remote");
  }

  private async local(op: string, args: Record<string, unknown>, channel: Channel): Promise<Result> {
    if (!checkLocal(op, args)) return fail("invalid_argument");
    if (op === "record.start" || op === "record.stop" || op === "record.status") {
      return this.features?.command(op, args) ?? fail("local_owner_required");
    }
    return this.run(op, args, channel);
  }

  private async run(op: string, args: Record<string, unknown>, channel: Channel): Promise<Result> {
    const pageOriginated = channel !== "local";
    switch (op) {
      case "stop":
        setTimeout(() => void this.end("stopped", channel !== "local"), 0);
        return { ok: true, data: {} };
      case "pause":
        await this.latch("paused", true, pageOriginated);
        return { ok: true, data: {} };
      case "resume":
        await this.latch("paused", false, pageOriginated);
        return { ok: true, data: {} };
      case "mode":
        this.mode = args.mode as "text" | "motion";
        const t = this.capture?.getVideoTracks()[0];
        if (t) t.contentHint = this.mode;
        await this.session?.mode(this.mode === "motion");
        this.changed();
        return { ok: true, data: {} };
      case "safe":
        // Turning safe mode off is permissive: checkRemote refuses it from the server.
        if (this.safe !== (args.on === true)) {
          this.safe = args.on === true;
          this.redact();
        }
        this.changed();
        return { ok: true, data: {} };
      case "lock":
        await this.latch("locked", args.locked as boolean, pageOriginated);
        return { ok: true, data: {} };
      case "kick":
        return this.kick(args, pageOriginated);
      case "unfreeze":
        await this.pinChange(() => this.recovery.unfreeze(args.keep_pin === true ? { keepPin: true } : { policy: this.policy, ...this.value(this.policy, args.pin) }));
        if (args.keep_pin === true || !this.latches.frozen) {
          if (channel === "window") this.cfg.channel?.latchCleared("frozen");
          this.alert({ kind: "unfrozen", pin_rotated: args.keep_pin !== true });
        }
        return { ok: true, data: {} };
      case "pin.rotate":
        await this.pinChange(() => this.recovery.rotate({ policy: this.policy, ...this.value(this.policy, args.pin) }));
        return { ok: true, data: {} };
      case "pin.set": {
        const policy = parsePolicy(args.policy)!;
        await this.pinChange(() => this.recovery.rotate({ policy, ...this.value(policy, args.pin) }));
        return { ok: true, data: {} };
      }
      case "knock.approve":
        return this.approve(String(args.knock_id), String(args.row_code));
      case "annotations":
        // Turning viewer drawing on is permissive: checkRemote refuses it from the server.
        this.annotations.setViewers(args.viewers === true);
        return { ok: true, data: { viewers: this.annotations.viewersEnabled } };
      case "annotations.clear":
        this.annotations.clear();
        return { ok: true, data: {} };
      case "knock.deny":
        if (!ID22.test(String(args.knock_id))) return fail("unknown_knock");
        this.host.deny(String(args.knock_id));
        this.changed();
        return { ok: true, data: {} };
    }
    return fail("invalid_argument");
  }

  private value(policy: PinPolicy, pin: unknown): { value?: string | null } {
    if (policy.kind === "none") return { value: null };
    if (typeof pin === "string") return { value: pin };
    return {};
  }

  /** Set or clear a latch. Setting applies synchronously, then persists; page-originated sets wait for the daemon's mirror. */
  private async latch(name: "locked" | "paused", value: boolean, pageOriginated: boolean) {
    const write = this.recovery.setLatch(name, value);
    this.changed();
    await write;
    if (value && pageOriginated) await this.reportNow();
    if (!value && pageOriginated) this.cfg.channel?.latchCleared(name);
    this.changed();
  }

  private async kick(args: Record<string, unknown>, pageOriginated: boolean): Promise<Result> {
    const admissionId = String(args.admission_id);
    const lock = args.lock === true;
    // Kick + lock: the lock is set before anything is awaited and stays set
    // whatever the kick does (HOST-CONTROL §4.3).
    const latch = lock ? this.recovery.setLatch("locked", true) : Promise.resolve();
    const before = this.viewers();
    const target = before.find((v) => v.admissionId === admissionId);
    let kick: Promise<string[]>;
    if (args.rotate_pin === true) {
      kick = (async () => {
        if (!target) throw new E2EEError("unknown_admission", "no such admission");
        await this.pinChange(() => this.recovery.rotate({ policy: this.policy, ...this.value(this.policy, args.pin), op: "kick", admissionId }));
        return before.filter((v) => v.viewerId === target.viewerId).map((v) => v.admissionId);
      })();
    } else kick = ID22.test(admissionId) ? this.host.kick(admissionId) : Promise.reject(new E2EEError("unknown_admission", "no such admission"));
    const [l, k] = await Promise.allSettled([latch, kick]);
    if (lock && pageOriginated) await this.reportNow();
    this.changed();
    if (k.status === "rejected") return errorResult(k.reason, lock ? { locked: true } : undefined);
    if (l.status === "rejected") return errorResult(l.reason, { locked: true });
    return { ok: true, data: { admission_ids: k.value, locked: this.latches.locked } };
  }

  /** PIN changes send secrets and state before their result (CONTRACT-NOTES-HOST #11). */
  private async pinChange(f: () => Promise<void>) {
    try {
      await f();
    } finally {
      this.sendSecrets();
      await this.reportNow();
    }
  }

  private async approve(knockId: string, rowCode: string): Promise<Result> {
    if (!ROW.test(rowCode)) return fail("invalid_argument", "row_code must be three row-code characters");
    const knock = this.knocks().find((k) => k.knockId === knockId);
    let admissionId = "";
    const off = this.host.on("admitted", (e) => {
      if (knock && e.peerId === knock.peerId) admissionId = e.admissionId;
    });
    try {
      await this.host.approve(knockId, { rowCode });
    } finally {
      off();
    }
    this.changed();
    return { ok: true, data: { admission_id: admissionId } };
  }

  /** Bytes the audio sender has sent (sender HUD, ≤ 1/s while it's open). */
  async audioBytes(): Promise<{ bytes: number; at: number } | null> {
    const s = this.audioSender();
    if (!s) return null;
    for (const v of (await s.getStats()).values()) if (v.type === "outbound-rtp") return { bytes: Number(v.bytesSent ?? 0), at: Number(v.timestamp ?? 0) };
    return null;
  }

  // ---------------------------------------------------------------- reporting

  state(): Record<string, unknown> {
    const policy = this.policy;
    const viewers = this.phase === "ended" ? [] : this.viewers();
    const current = new Map(viewers.map((v) => [v.admissionId, v.viewerId]));
    for (const [a, v] of this.lastAdmissions)
      if (!current.has(a) && !this.kickedIds.has(a)) this.addEnd({ kind: "admission", admission_id: a, viewer_id: v, cause: this.phase === "ended" ? "stream_ended" : "revoked", at: Date.now() });
    this.lastAdmissions = current;
    const health = senderHealth(this.capture, this.session?.quality);
    const s: Record<string, unknown> = {
      v: 1,
      stream_id: this.streamId,
      instance: this.instance,
      seq: ++this.seq,
      phase: this.phase,
      mode: this.mode,
      safe: this.safe,
      e2ee: { status: "beta", epoch: this.host.keys.activeEpoch, suspended: this.host.keys.suspended, revocation_not_durable: this.host.revocationNotDurable },
      access: {
        pin: { required: policy.kind !== "none", kind: policy.kind, length: policy.kind === "digits" ? policy.length : null },
        pin_version: this.recovery.record.pin_state?.version ?? 0,
        rotation_pending: this.recovery.record.pending_rotation !== null,
        reconciling: this.recovery.reconciling,
        latches: this.latches,
        knock: this.recovery.record.knock,
        failures: { stream: this.failures, limit: this.host.budget.limits.perStream },
      },
      admissions: viewers.slice(0, 500).map((v) => ({
        admission_id: v.admissionId,
        viewer_id: v.viewerId,
        row_code: v.rowCode,
        peer_id: v.peerId,
        connected: v.connected,
        via_grant: v.viaGrant,
        admitted_at: v.admittedAt,
        kick_incomplete: v.kickIncomplete,
      })),
      knocks: (this.phase === "ended" ? [] : this.knocks()).slice(0, 50).map((k) => ({ knock_id: k.knockId, peer_id: k.peerId, row_code: k.rowCode, at: k.at, expires_at: k.expiresAt })),
      ends: this.ends.map(({ at: _at, ...e }) => e),
      health,
    };
    if (this.phase === "ended") s.end_reason = this.endedReason;
    return s;
  }

  private scheduleState() {
    if (this.stateTimer || !this.recovery) return;
    const wait = Math.max(0, 250 - (Date.now() - this.lastStateAt));
    this.stateTimer = setTimeout(() => {
      this.stateTimer = undefined;
      void this.reportNow();
    }, wait);
  }

  /** Send state to the server and the daemon; waits for the daemon's state_ack (≤ 1 s). */
  async reportNow(): Promise<void> {
    if (!this.recovery) return;
    this.lastStateAt = Date.now();
    const s = this.state();
    this.session?.sendSignal({ type: "sender.state", state: s });
    await this.cfg.channel?.sendState(s as { seq: number });
  }

  sendSecrets() {
    if (!this.recovery || this.phase === "starting") return;
    this.cfg.channel?.sendSecrets({ pin: this.pin, pending_pin: this.pendingPin, link: this.link });
  }

  private heartbeat() {
    if (Date.now() - this.lastStateAt >= 5000) void this.reportNow();
    this.onMedia();
  }

  private alert(a: Record<string, unknown>) {
    this.cfg.channel?.sendAlert(a);
    this.session?.sendSignal({ type: "alert", alert: a });
  }

  /** Reconnect steps (same JS context, HOST-CONTROL §5.3): only set latches, never clear. */
  async reattach(init: { stream: { id: string }; policy: { knock: boolean }; latches: Latches; active_stream_ids: string[] }) {
    this.features?.reset();
    if (init.stream.id !== this.streamId) {
      await this.end("state_lost", true);
      return;
    }
    await this.recovery.mergeInit({ knock: init.policy.knock, latches: init.latches }).catch(() => undefined);
    await this.cleanup(init.active_stream_ids).catch(() => undefined);
    await this.reportNow();
    this.sendSecrets();
  }

  /** End the share: draw "Ended", report, leave, and remove the stream's stored state. */
  async end(reason: string, tellDaemon: boolean) {
    if (this.phase === "ended") return;
    this.phase = "ended";
    this.features?.stop();
    this.endedReason = reason;
    const ended = statusSource("ended", this.capture?.getVideoTracks()[0]?.getSettings());
    await this.audioSender()?.replaceTrack(null).catch(() => undefined);
    if (this.session?.mediaStarted) await this.session.setOutgoing(ended.getVideoTracks()[0]);
    this.capture?.getTracks().forEach((t) => {
      t.enabled = false;
    });
    await this.reportNow().catch(() => undefined);
    if (tellDaemon) this.cfg.channel?.sendEnded(reason === "expired" ? "expired" : reason);
    this.changed();
    // Let a few "Ended" frames reach viewers before leaving.
    await new Promise((r) => setTimeout(r, this.session?.mediaStarted ? 800 : 0));
    this.session?.stop();
    stopTestSource(ended);
    ended.getTracks().forEach((t) => t.stop());
    this.capture?.getTracks().forEach((t) => t.stop());
    this.redactor?.stop();
    if (this.capture) stopTestSource(this.capture);
    if (this.paused) stopTestSource(this.paused);
    if (this.blocked) stopTestSource(this.blocked);
    this.timers.forEach(clearInterval);
    this.annotations?.stop();
    await this.host.close().catch(() => undefined);
    this.worker.terminate();
    await this.storage.deleteStream(this.streamId).catch(() => undefined);
    this.releaseLock?.();
    this.changed();
  }

  /** Superseded or closed: stop capture and drop the host without ending the stream. */
  async abandon() {
    this.features?.stop();
    this.phase = "ended";
    this.endedReason = "superseded";
    this.session?.stop();
    this.capture?.getTracks().forEach((t) => t.stop());
    this.redactor?.stop();
    this.timers.forEach(clearInterval);
    this.annotations?.stop();
    await this.host?.close().catch(() => undefined);
    this.worker?.terminate();
    this.releaseLock?.();
    this.changed();
  }
}

declare global {
  interface Window {
    passstreamSender?: SenderCore;
  }
}
