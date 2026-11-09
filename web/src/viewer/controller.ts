/**
 * The viewer: link fragment → signed CHALLENGE → PIN (or grant, or no-PIN)
 * admission over the server's E2EE relay → epoch keys → SFrame decryption on
 * the receiver. Fails closed: without RTCRtpScriptTransform it never starts.
 */
import {
  E2EEError,
  ViewerAdmission,
  attachTransform,
  detectE2EESupport,
  fromBase64Url,
  isE2EEError,
  MsgType,
  parseFragment,
  peekType,
  postReceiverEpoch,
  toBase64Url,
  type EpochKey,
  type LinkFragment,
  type ViewerAdmissionResult,
  type ViewerSession,
} from "@passstream/e2ee";
import TransformWorker from "@passstream/e2ee/transform-worker?worker";
import { AUDIO_TRACK, addAudioReceiver, withAudioNack } from "../audio";
import { ViewerAnnotations } from "../annotations/viewer";

const NO_AUDIO_TEST = typeof navigator !== "undefined" && navigator.webdriver && typeof location !== "undefined" && new URLSearchParams(location.search).has("noaudio");
import { Session } from "../session";
import { NAME_KEY, type PinPolicy } from "../viewer-ui/PinScreen";
import { missingFeatures } from "../viewer-ui/states";
import { isTerminal, type Screen, type Snapshot, type View } from "./machine";

export type { View };


type Stored = { sid: string; grant?: { id: string; secret: string; expiresAt: number }; instance?: string | null; rowCode?: string; lockedUntil?: number };

const MAX_AUTO_RETRIES = 5;
const ADMISSION_PROGRESS_MS = 5000;

export class ViewerController {
  view: View = { kind: "verifying" };
  session?: Session;
  rowCode: string | null = null;
  pinError: string | null = null;
  triesLeft: number | null = null;
  lockedUntil: number | null = null;
  busy = false;
  admitted = false;
  /** Diagnostics for the HUD and the torture suite. */
  admissions = 0;
  drops: Record<string, number> = {};
  readonly events: { at: number; kind: string; detail?: string }[] = [];
  /** E2EE annotations (PROTOCOL §20): sealed and opened with this admission's session keys. */
  readonly annotations = new ViewerAnnotations({
    seal: async (b) => {
      const channel = this.channel;
      if (!channel || !this.admitted) throw new E2EEError("state", "not admitted");
      const bytes = await channel.sealAnnotation(b);
      if (channel !== this.channel || this.disposed) throw new E2EEError("state", "admission ended");
      return bytes;
    },
    send: (b) => this.send(b),
    rowCode: () => this.rowCode,
  });
  /** Annotation messages that failed to open (forged, replayed, stale epoch). */
  annotationRejects = 0;
  /** Newest epoch handed to the decrypt worker (HUD). */
  get epoch(): number | null {
    return this.epochs.size ? Math.max(...this.epochs) : null;
  }
  private frag?: LinkFragment;
  private worker?: Worker;
  private streamId = "";
  private senderOnline = false;
  private senderInstance: string | null = null;
  private admittedInstance: string | null = null;
  private connectionId = "";
  private admittedOn = "";
  private pinHint: PinPolicy | { kind: "none" } = { kind: "digits", length: 4 };
  private stored: Stored | null = null;
  private adm: ViewerAdmission | null = null;
  private admMode: "pin" | "grant" | "none" = "pin";
  private admStarted = 0;
  private channel: ViewerSession | null = null;
  private typedPin: string | null = null;
  private retries = 0;
  private readmitTimer?: ReturnType<typeof setTimeout>;
  private missingKeySince = 0;
  private lastMissingAt = 0;
  private everAdmitted = false;
  private listeners = new Set<() => void>();
  private timer?: ReturnType<typeof setInterval>;
  /** Worker decrypt reports (diagnostics, torture suite): posted when a frame decrypts, ≥ 200 ms apart per track. */
  readonly decrypted: { at: number; track: number; count: number }[] = [];
  /** Diagnostics (torture suite): per-frame decrypt timings from the worker. */
  private traceReply?: (frames: unknown[]) => void;
  trace(on: boolean) {
    this.worker?.postMessage({ type: "trace", on });
  }
  dumpTrace(): Promise<unknown[]> {
    return new Promise((resolve) => {
      if (!this.worker) return resolve([]);
      this.traceReply = resolve;
      this.worker.postMessage({ type: "trace-dump" });
    });
  }
  /** Last time an audio frame decrypted (worker report): "Tap to unmute" only while audio is live. */
  private lastAudioAt = 0;
  get audioLive(): boolean {
    return this.admitted && Date.now() - this.lastAudioAt < 1500;
  }
  /** The screen last drawn (diagnostics: tests, torture suite). */
  screen: Screen | null = null;
  /** When the page started connecting, for the "still connecting" hint. */
  readonly startedAt = Date.now();
  /** The PIN screen's policy: the sender's hint, else 4 digits. */
  get pinPolicy(): PinPolicy {
    return this.pinHint.kind === "none" ? { kind: "digits", length: 4 } : this.pinHint;
  }
  /** Everything the screen state machine (./machine.ts) reads. */
  snapshot(now = Date.now()): Snapshot {
    const s = this.session;
    return {
      view: this.view,
      checkingPin: this.busy && this.admMode === "pin" && !this.admitted,
      pinPolicy: this.pinPolicy,
      lockedUntil: this.lockedUntil,
      hostAway: this.admitted && !this.senderOnline,
      readmitting: this.admitted && this.adm?.state === "hello_sent",
      session: s ? { state: s.state, marker: s.marker, firstFrame: s.firstFrame, hasStream: !!s.remoteStream, signalingOpen: s.signalingOpen } : null,
      startedAt: this.startedAt,
      now,
    };
  }
  /** Epochs handed to the worker, so dispose() can drop them. */
  private epochs = new Set<number>();

  /** Hand an epoch to the worker, then wipe the main thread's copy. */
  private install(k: EpochKey) {
    if (!this.worker) return;
    postReceiverEpoch(this.worker, k);
    this.epochs.add(k.epoch);
    k.secret.fill(0);
  }

  subscribe(f: () => void) {
    this.listeners.add(f);
    return () => void this.listeners.delete(f);
  }
  private changed() {
    for (const f of this.listeners) f();
  }
  private event(kind: string, detail?: string) {
    this.events.push({ at: Date.now(), kind, detail });
    if (this.events.length > 300) this.events.shift();
  }
  private setView(v: View) {
    // No transition out of a terminal view (ended, removed, …).
    if (isTerminal(this.view.kind)) return;
    this.view = v;
    this.changed();
  }

  /** The join message's extra fields; Session spreads it on every (re)join. */
  private join: Record<string, unknown> = { protocol: 1 };
  /** The label the current signaling connection joined with. */
  private joinedLabel: unknown = undefined;
  private started = false;
  start(video: HTMLVideoElement) {
    if (this.started) return;
    this.started = true;
    // Fail closed before anything touches the network.
    const support = detectE2EESupport();
    const missing = missingFeatures();
    if (!support.supported || missing.length) {
      this.setView({ kind: "unsupported", missing: missing.length ? missing : [support.supported ? "" : support.reason] });
      return;
    }
    try {
      this.frag = parseFragment(location.hash);
    } catch {
      this.setView({ kind: "invalid" });
      return;
    }
    this.worker = new TransformWorker();
    this.worker.onmessage = (ev: MessageEvent) => {
      const m = ev.data as { type: string; reason?: string; count?: number; track?: number };
      if (m.type === "trace") this.traceReply?.((m as unknown as { frames: unknown[] }).frames);
      if (m.type === "decrypted") {
        if (m.track === AUDIO_TRACK) this.lastAudioAt = Date.now();
        this.decrypted.push({ at: Date.now(), track: m.track ?? -1, count: m.count ?? 0 });
        if (this.decrypted.length > 4000) this.decrypted.shift();
      }
      if (m.type !== "drop" || !m.reason) return;
      this.drops[m.reason] = (this.drops[m.reason] ?? 0) + (m.count ?? 1);
      if (m.reason === "unknown_key" && this.admitted) {
        const now = Date.now();
        if (!this.missingKeySince || now - this.lastMissingAt > 1500) this.missingKeySince = now;
        this.lastMissingAt = now;
      }
    };
    let label: string | null = null;
    try {
      label = localStorage.getItem(NAME_KEY);
    } catch {
      /* storage blocked */
    }
    this.join = { protocol: 1, ...(label ? { label: label.slice(0, 64) } : {}) };
    this.session = new Session("view", "", this.frag.capability, video, () => this.changed(), undefined, {
      deferMedia: true,
      join: this.join,
      onMessage: (m) => this.onSignal(m),
      decrypt: (r) => attachTransform(r as unknown as { transform: unknown }, this.worker!, { role: "decrypt", kind: "video", track: 0, tracks: [0, 2], codec: "video/vp9" }),
      // E2EE audio on track 1, decrypt transform attached before negotiation.
      // Test-only `?noaudio` under automation omits the m-line (bad-Wi-Fi A/B).
      extend: (pc) =>
        NO_AUDIO_TEST
          ? undefined
          : addAudioReceiver(pc, (r) => attachTransform(r as unknown as { transform: unknown }, this.worker!, { role: "decrypt", kind: "audio", track: AUDIO_TRACK, codec: "audio/opus" })),
      munge: withAudioNack,
      onTerminal: (code) => this.onTerminal(code),
    });
    window.passstream = this.session;
    this.timer = setInterval(() => this.tick(), 500);
  }

  // ---------------------------------------------------------------- signaling

  private onSignal(m: Record<string, unknown>) {
    switch (m.type) {
      case "joined": {
        this.streamId = String(m.stream_id ?? "");
        this.connectionId = String(m.connection_id ?? "");
        this.senderOnline = m.sender_online === true;
        this.senderInstance = typeof m.sender_instance === "string" ? m.sender_instance : null;
        const hint = m.pin_hint as { kind?: string; length?: number } | undefined;
        if (hint?.kind === "none") this.pinHint = { kind: "none" };
        else if (hint?.kind === "passphrase") this.pinHint = { kind: "passphrase" };
        else if (hint?.kind === "digits" && typeof hint.length === "number") this.pinHint = { kind: "digits", length: hint.length };
        this.joinedLabel = this.join.label;
        this.load();
        // Every signaling connection is a new relay peer: the sender knows
        // this viewer only after it (re-)admits on it (CONTRACT §8.3).
        this.channel?.dispose();
        this.channel = null;
        this.annotations.beginAdmission();
        this.clearAdmission();
        this.event("joined", this.senderOnline ? "sender online" : "sender away");
        this.admit();
        break;
      }
      case "e2ee":
        void this.receive(String(m.b ?? ""));
        break;
      case "e2ee.sender_back": {
        const instance = typeof m.instance === "string" ? m.instance : null;
        const was = this.senderOnline;
        this.senderOnline = true;
        this.senderInstance = instance;
        this.event("sender_back", instance ?? "");
        if (this.admitted && (instance !== this.admittedInstance || this.admittedOn !== this.connectionId)) {
          // The sender reloaded (its sessions are gone: 0–2 s jitter, CONTRACT §8.3), or
          // this connection never admitted (we re-joined while it was away, e.g. after a
          // server restart): re-admit with the grant, jittered so viewers don't stampede.
          clearTimeout(this.readmitTimer);
          this.readmitTimer = setTimeout(() => this.admit(), Math.random() * (instance !== this.admittedInstance ? 2000 : 300));
        } else if (!this.admitted && (!was || this.adm?.state === "hello_sent")) this.admit();
        break;
      }
      case "e2ee.sender_away":
        this.senderOnline = false;
        this.event("sender_away");
        if (!this.admitted && this.view.kind !== "pin") this.setView({ kind: "waiting" });
        this.changed();
        break;
    }
  }

  private onTerminal(code: string) {
    if (isTerminal(this.view.kind)) return;
    this.event("terminal", code);
    this.dispose(code === "unauthorized" && this.everAdmitted);
    if (code === "unsupported_codec") this.setView({ kind: "unsupported", missing: ["VP9 profile 0 video codec (required by PassStream)"] });
    else if (code === "unauthorized") this.setView({ kind: "removed", reason: this.everAdmitted ? "kicked" : "full" });
    else this.setView({ kind: "ended", reason: code === "stream_ended" ? "stopped" : code });
  }

  /**
   * Every terminal state goes through here: the worker (and every epoch key
   * in it) is terminated, the channel keys and in-flight admission are
   * dropped, timers stop, and a revoked grant is forgotten.
   */
  disposed = false;
  private dispose(forgetGrant: boolean) {
    this.disposed = true;
    this.admitted = false;
    this.busy = false;
    this.channel?.dispose();
    this.channel = null;
    this.clearAdmission();
    this.typedPin = null;
    clearTimeout(this.readmitTimer);
    clearInterval(this.timer);
    if (this.worker) {
      for (const e of this.epochs) this.worker.postMessage({ type: "receiver-drop-epoch", epoch: e });
      this.worker.terminate();
      this.worker = undefined;
    }
    this.epochs.clear();
    this.annotations.dispose();
    if (forgetGrant && this.stored?.grant) {
      delete this.stored.grant;
      this.save();
    }
    this.session?.stop();
  }

  private send(bytes: Uint8Array) {
    this.session?.sendSignal({ type: "e2ee", b: toBase64Url(bytes) });
  }

  private clearAdmission() {
    const adm = this.adm;
    this.adm = null;
    adm?.abort();
  }

  // ---------------------------------------------------------------- admission

  private key() {
    return `ps:viewer:${this.streamId}`;
  }
  private load() {
    try {
      const v = JSON.parse(sessionStorage.getItem(this.key()) ?? "null") as Stored | null;
      if (v && typeof v.sid === "string" && fromBase64Url(v.sid).length === 16) {
        this.stored = v;
        // A reload doesn't skip a PIN backoff the host already imposed.
        if (typeof v.lockedUntil === "number" && v.lockedUntil > Date.now() && !this.lockedUntil) this.lockedUntil = v.lockedUntil;
        return;
      }
    } catch {
      /* corrupt: start over */
    }
    this.stored = { sid: toBase64Url(crypto.getRandomValues(new Uint8Array(16))) };
    this.save();
  }
  private save() {
    try {
      sessionStorage.setItem(this.key(), JSON.stringify(this.stored));
    } catch {
      /* the grant just isn't remembered across a reload */
    }
  }

  /** Choose how to (re-)admit: grant, no-PIN, a typed PIN, or ask for one. */
  private admit() {
    if (!this.streamId || !this.frag) return;
    const g = this.stored?.grant;
    if (g && g.expiresAt > Date.now()) return this.handshake("grant");
    if (this.pinHint.kind === "none") return this.handshake("none");
    if (this.typedPin !== null) return this.handshake("pin");
    if (this.admitted) {
      // Admitted without a usable grant: nothing to re-admit with.
      this.lose("kicked");
      return;
    }
    this.setView({ kind: "pin", policy: this.pinHint });
  }

  /**
   * The PIN screen's submit. The PIN is checked by the host's machine. The
   * name is viewer-reported and travels only in the join message, so a name
   * that differs from the one this connection joined with rejoins signaling
   * once before the HELLO: the host has it before the knock. The typed PIN
   * is kept across that rejoin and answered on the new connection.
   */
  submitPin(pin: string, name?: string) {
    if (this.lockedUntil && Date.now() < this.lockedUntil) return;
    if (name !== undefined) {
      if (name) this.join.label = name.slice(0, 64);
      else delete this.join.label;
    }
    this.typedPin = pin;
    this.pinError = null;
    if (!this.admitted && this.join.label !== this.joinedLabel && this.session?.rejoin()) {
      this.event("rejoin", "name");
      this.admMode = "pin";
      this.busy = true;
      this.clearAdmission();
      this.setView({ kind: "verifying" });
      return;
    }
    this.handshake("pin");
  }

  private handshake(mode: "pin" | "grant" | "none") {
    if (!this.frag || !this.stored) return;
    if (!this.senderOnline) {
      // Host offline: the check runs when the sender connects.
      if (!this.admitted) this.setView({ kind: "waiting" });
      return;
    }
    const g = this.stored.grant;
    try {
      this.clearAdmission();
      this.adm = new ViewerAdmission({
        streamId: this.streamId,
        senderKey: this.frag.senderKey,
        linkSecret: this.frag.linkSecret,
        viewerSid: fromBase64Url(this.stored.sid),
        ...(mode === "grant" && g ? { grant: { id: fromBase64Url(g.id), secret: fromBase64Url(g.secret) } } : { pin: mode === "pin" ? this.typedPin : null }),
      });
    } catch (e) {
      this.pinError = "That PIN isn't valid.";
      this.typedPin = null;
      this.setView({ kind: "pin", policy: this.pinHint.kind === "none" ? { kind: "digits", length: 4 } : this.pinHint });
      void e;
      return;
    }
    this.annotations.beginAdmission();
    this.channel?.dispose();
    this.channel = null;
    this.admMode = mode;
    this.admStarted = Date.now();
    this.busy = true;
    this.event("hello", mode);
    this.send(this.adm.start());
    // Media starts in parallel with the handshake; frames stay encrypted until keys arrive.
    this.session?.startMedia();
    if (!this.admitted) this.setView({ kind: "verifying" });
    else this.changed();
  }

  private async receive(b: string) {
    let bytes: Uint8Array;
    try {
      bytes = fromBase64Url(b);
    } catch {
      return;
    }
    let type: number | null = null;
    try {
      type = peekType(bytes);
    } catch {
      /* malformed: the admission or session rejects it below */
    }
    if (type === MsgType.Annotations) {
      // Annotations belong to the admitted session, even while a re-admission runs.
      const channel = this.channel;
      if (!channel || this.disposed) return;
      try {
        const { records } = await channel.openAnnotations(bytes);
        if (channel !== this.channel || this.disposed) return;
        this.annotations.receive(records);
      } catch {
        this.annotationRejects++;
      }
      return;
    }
    const adm = this.adm;
    if (adm && adm.state !== "admitted" && adm.state !== "failed") {
      try {
        const before = adm.state;
        const outs = await adm.handle(bytes);
        // Stale: the page was disposed, or a rejoin or a new attempt replaced
        // this admission during the await. No side effect at all: nothing is
        // sent, and the typed PIN belongs to the current attempt.
        if (adm !== this.adm || this.disposed) {
          adm.result?.epochs.forEach((k) => k.secret.fill(0));
          adm.result?.session.dispose();
          adm.result?.grant.secret.fill(0);
          return;
        }
        for (const out of outs) this.send(out);
        if (before === "hello_sent" && adm.state === "response_sent") this.admStarted = Date.now();
        // The typed PIN is kept only until the signed CHALLENGE is answered, so a
        // signaling reconnect before that can resume the same attempt.
        if (adm.state !== "hello_sent") this.typedPin = null;
        if (adm.state === "pending" && adm.pending) {
          this.busy = false;
          this.event("knock", adm.pending.rowCode);
          this.setView({ kind: "knock", code: adm.pending.rowCode });
        }
        if (adm.result) this.admittedWith(adm.result);
      } catch (e) {
        if (adm === this.adm && !this.disposed) this.refused(e);
      }
      return;
    }
    const channel = this.channel;
    if (channel) {
      try {
        const { epoch, ack } = await channel.handle(bytes);
        if (channel !== this.channel || this.disposed) return void epoch.secret.fill(0);
        this.install(epoch);
        this.send(ack);
        this.event("rekey", String(epoch.epoch));
      } catch {
        /* forged, replayed or stale rekeys are dropped */
      }
    }
  }

  private admittedWith(r: ViewerAdmissionResult) {
    this.channel?.dispose();
    this.annotations.beginAdmission();
    this.channel = r.session;
    for (const k of r.epochs) this.install(k);
    for (const a of r.acks) this.send(a);
    this.stored = {
      ...this.stored!,
      grant: { id: toBase64Url(r.grant.id), secret: toBase64Url(r.grant.secret), expiresAt: r.grant.expiresAt },
      instance: this.senderInstance,
      rowCode: r.rowCode,
      lockedUntil: undefined,
    };
    this.save();
    r.grant.secret.fill(0);
    this.admitted = this.everAdmitted = true;
    this.admittedInstance = this.senderInstance;
    this.admittedOn = this.connectionId;
    this.rowCode = r.rowCode;
    this.busy = false;
    this.retries = 0;
    this.missingKeySince = 0;
    this.triesLeft = null;
    this.lockedUntil = null;
    this.pinError = null;
    this.admissions++;
    this.event("admitted", this.admMode);
    this.setView({ kind: "media" });
  }

  private refused(e: unknown) {
    this.clearAdmission();
    this.busy = false;
    this.typedPin = null;
    const reason = isE2EEError(e, "rejected") ? String(e.details?.reason ?? "") : "";
    const d = isE2EEError(e) ? e.details : undefined;
    this.event("refused", reason || (isE2EEError(e) ? e.code : "error"));
    const pinPolicy: PinPolicy = this.pinHint.kind === "none" ? { kind: "digits", length: 4 } : this.pinHint;
    switch (reason) {
      case "wrong_pin":
        this.triesLeft = typeof d?.triesLeft === "number" ? d.triesLeft : null;
        this.pinError = "That PIN didn't match.";
        if (typeof d?.retryAfterMs === "number" && d.retryAfterMs > 0) this.lockOut(d.retryAfterMs);
        return this.setView({ kind: "pin", policy: pinPolicy });
      case "backoff":
        this.triesLeft = typeof d?.triesLeft === "number" ? d.triesLeft : null;
        this.lockOut(typeof d?.retryAfterMs === "number" ? d.retryAfterMs : 60_000);
        this.pinError = "Too many wrong PINs. Wait, then try again.";
        return this.setView({ kind: "pin", policy: pinPolicy });
      case "pin_required":
        if (this.pinHint.kind === "none") this.pinHint = { kind: "digits", length: 4 };
        return this.setView({ kind: "pin", policy: pinPolicy });
      case "frozen":
        return this.lose("frozen");
      case "denied":
        return this.lose("denied");
      case "grant_invalid":
        delete this.stored!.grant;
        this.save();
        // Kicked, or the PIN changed: an admitted page is out; a reload with a stale grant asks again.
        if (this.admitted) return this.lose("kicked");
        return this.admit();
      case "locked":
        if (this.admitted || this.admMode === "grant") return this.retry("locked");
        return this.lose("locked");
      case "expired":
      case "invalid":
        if (this.admMode !== "pin") return this.retry(reason);
        this.pinError = "The host couldn't finish the check. Try again.";
        return this.setView({ kind: "pin", policy: pinPolicy });
    }
    // Unsigned or garbled traffic (a relay can inject it): retry, never weaken.
    if (this.admMode !== "pin") return this.retry(isE2EEError(e) ? e.code : "error");
    this.pinError = "Couldn't verify the host's answer. Try again.";
    this.setView({ kind: "pin", policy: pinPolicy });
  }

  private lockOut(ms: number) {
    this.lockedUntil = Date.now() + ms;
    if (this.stored) {
      this.stored.lockedUntil = this.lockedUntil;
      this.save();
    }
  }

  private retry(why: string) {
    if (++this.retries > MAX_AUTO_RETRIES && (!this.admitted || why === "admission_timeout")) {
      this.dispose(false);
      this.setView(why === "admission_timeout" ? { kind: "ended", reason: why } : { kind: "removed", reason: why === "locked" ? "locked" : "denied" });
      return;
    }
    clearTimeout(this.readmitTimer);
    this.readmitTimer = setTimeout(() => this.admit(), Math.min(4000, 250 * 2 ** this.retries) * (0.5 + Math.random()));
  }

  private lose(reason: "kicked" | "locked" | "denied" | "frozen") {
    // Black, not frozen: drop every key and the picture.
    this.dispose(reason === "kicked" || reason === "frozen");
    this.setView({ kind: "removed", reason });
  }

  private tick() {
    // Signaling closed for good without a terminal message (close 1008: the
    // stream ended): the same disposal as any terminal state.
    if (this.session?.state === "ended" && !this.disposed) return this.onTerminal("stream_ended");
    const adm = this.adm;
    if (adm && adm.state === "pending") {
      const t = adm.tick();
      if (t.state === "failed") this.lose("denied");
    }
    // Bound both unsigned transport loss and a lost authenticated cancellation.
    // PENDING has its own signed approval deadline and is deliberately excluded.
    // Before answering CHALLENGE, a typed PIN waits for a known-offline host.
    // sender_back starts a fresh HELLO/phase clock; an answered PIN is never reused.
    const waitingForHost = !this.senderOnline && this.admMode === "pin" && adm?.state === "hello_sent" && this.typedPin !== null;
    if (adm && !waitingForHost && (adm.state === "hello_sent" || adm.state === "response_sent") && Date.now() - this.admStarted > ADMISSION_PROGRESS_MS) {
      this.clearAdmission();
      this.busy = false;
      this.typedPin = null;
      this.event("admission_timeout", this.admMode);
      if (this.admMode !== "pin") this.retry("admission_timeout");
      else {
        this.pinError = "The host couldn't finish the check. Try again.";
        this.setView({ kind: "pin", policy: this.pinPolicy });
      }
      this.changed();
      return;
    }
    // Frames of an epoch we have no key for, for over 2 s: re-admit with the grant.
    const now = Date.now();
    if (this.admitted && this.missingKeySince && now - this.lastMissingAt < 1500 && now - this.missingKeySince > 2000 && this.adm?.state !== "hello_sent") {
      this.missingKeySince = 0;
      this.event("readmit", "missing epoch");
      this.admit();
    }
    this.changed();
  }
}

export { E2EEError };

declare global {
  interface Window {
    passstreamViewer?: ViewerController;
  }
}
