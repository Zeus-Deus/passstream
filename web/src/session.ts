import { E2EEError, isE2EEError } from "@passstream/e2ee";
import { RecoveryPolicy, selectedPair, videoFreshnessLimit, transportBaselineRTT } from "./recovery";
import { lossEvidence, PlayoutPolicy } from "./playout";
import { decodeFrame, stopTestSource, publisherEncoding, type Marker } from "./source";
import { CatchupGrace, VideoFreshness, decoderLiveness, heartbeatStartupExpired } from "./freshness";
import { addPublisherThumbnail, publisherOffer, publisherStats } from "./thumbnail";
export type State =
  "waiting" | "connecting" | "live" | "reconnecting" | "ended";
export type Quality = {
  rtt: number;
  loss: number;
  bitrate: number;
  path: string;
  fps: number;
  layer: string;
  freezes: number;
  freezeSeconds: number;
  frames: number;
  g2g: number;
  timestamp: number;
  ingressFrames: number;
  forwardedPackets: number;
  ingressPackets: number;
  keyframes: number;
  stats?: Record<string, number | string>;
  playoutTarget?: number;
  width?: number;
  height?: number;
  repair?: unknown;
  ingress?: Record<string, number>;
};
type Signal = {
  type: string;
  freshFrames?: number;
  at?: number;
  id?: string;
  stream_id?: string;
  code?: string;
  session?: string;
  resumed?: boolean;
  iceServers?: RTCIceServer[];
  sdp?: RTCSessionDescriptionInit;
  candidate?: RTCIceCandidateInit;
};
/**
 * Protocol-1 additions around the phase-1 media session. The media,
 * recovery and playout behaviour is unchanged; these hooks only carry the
 * E2EE relay, the encoded transforms and the sender's outgoing track.
 */
export interface SessionOptions {
  /** Absolute signaling URL; defaults to this origin's /signal. */
  signalUrl?: string;
  /** Extra join fields (CONTRACT §8.1): protocol, sender_kind, instance, label. */
  join?: Record<string, unknown>;
  /** Every signaling message that isn't media negotiation, and `joined`. */
  onMessage?: (msg: Record<string, unknown>) => void;
  /** Join signaling only; create the PC when startMedia() is called. */
  deferMedia?: boolean;
  /** Attach the encrypt transform. Called before any track is set (fail closed). */
  encrypt?: (sender: RTCRtpSender, track: number) => void;
  /** Attach the decrypt transform. Called before negotiation. */
  decrypt?: (receiver: RTCRtpReceiver) => void;
  /** A terminal signaling error (stream_ended, unauthorized, upgrade_required). */
  onTerminal?: (code: string) => void;
  /** Add transceivers (E2EE audio) to each new PC, transforms attached, before the offer. */
  extend?: (pc: RTCPeerConnection) => unknown;
  /** Rewrite the local offer (audio NACK). */
  munge?: (sdp: string) => string;
}
export class Session {
  pc?: RTCPeerConnection;
  ws?: WebSocket;
  session = "";
  state: State = "connecting";
  quality: Quality = {
    rtt: 0,
    loss: 0,
    bitrate: 0,
    path: "—",
    fps: 0,
    layer: "L1T3 requested",
    freezes: 0,
    freezeSeconds: 0,
    frames: 0,
    g2g: 0,
    timestamp: 0,
    ingressFrames: 0,
    forwardedPackets: 0,
    ingressPackets: 0,
    keyframes: 0,
  };
  frameTimeline: { at: number; delay: number | null; frame: number | null; rtpTimestamp?: number }[] =
    [];
  samples: Quality[] = [];
  states: { state: State; at: number }[] = [];
  /** Recovery timeline for the HUD and torture suite (wall-clock ms). */
  events: { at: number; kind: string; detail?: string }[] = [];
  firstFrame = 0;
  started = performance.now();
  lastHeartbeat = performance.now();
  suspect = 0;
  replacements = 0;
  private tcpOnly =
    new URLSearchParams(location.search).get("transport") === "tcp";
  private stopped = false;
  private retry = 0;
  private timer: ReturnType<typeof setInterval>;
  private reconnectTimer?: ReturnType<typeof setTimeout>;
  private channel?: RTCDataChannel;
  private heartbeatReady = false;
  private peerStarted = performance.now();
  private peerConnected: number | null = null;
  private canvas = document.createElement("canvas");
  private bytes = 0;
  private stamp = 0;
  private lost = 0;
  private received = 0;
  private force = false;
  private immediateReconnect = false;
  private candidates: RTCIceCandidateInit[] = [];
  private statsBusy = false;
  private offerQueue: Promise<void> = Promise.resolve();
  private generation = 0;
  private lastVideoFrame = performance.now();
  private lastKeyframeRequest = 0;
  private frames = 0;
  private freshness = new VideoFreshness();
  private catchup = new CatchupGrace();
  private recovery = new RecoveryPolicy();
  motion = false;
  private playout = new PlayoutPolicy();
  private receiver?: RTCRtpReceiver & { jitterBufferTarget?: number | null };
  private lastJoined?: Signal;
  private incoming: Promise<void> = Promise.resolve();
  private inJoined = false;
  private mediaDeferred: boolean;
  private videoSender?: RTCRtpSender;
  private thumbnailSender?: RTCRtpSender;
  private unauthorized = 0;
  /** The sender's track on the wire: capture, or a paused/ended canvas, or null. */
  private outgoing: MediaStreamTrack | null = null;
  /** The viewer's current remote stream. */
  remoteStream: MediaStream | null = null;
  /** Sender-drawn paused/ended marker seen in the latest decoded frame. */
  marker: Marker = null;
  constructor(
    public role: "publish" | "view",
    public id: string,
    private credential: string,
    private video: HTMLVideoElement,
    private update: () => void,
    private stream?: MediaStream,
    private options: SessionOptions = {},
  ) {
    this.mediaDeferred = !!options.deferMedia;
    this.outgoing = stream?.getVideoTracks()[0] ?? null;
    this.connect();
    this.timer = setInterval(() => void this.tick(), 500);
  }
  private stateTo(s: State) {
    // "ended" is terminal: a queued frame callback or late ICE event never revives it.
    if (this.state === "ended") return;
    if (this.state !== s) {
      this.state = s;
      this.states.push({ state: s, at: Date.now() });
      this.update();
    }
  }
  private event(kind: string, detail?: string) {
    this.events.push({ at: Date.now(), kind, detail });
    if (this.events.length > 500) this.events.shift();
  }
  private send(v: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(v));
  }
  /** Send a non-media signaling message (E2EE relay, sender state). False if not open. */
  sendSignal(v: Record<string, unknown>): boolean {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
    this.ws.send(JSON.stringify(v));
    return true;
  }
  get signalingOpen(): boolean {
    return this.ws?.readyState === WebSocket.OPEN;
  }
  private connect() {
    if (this.stopped) return;
    const socket = new WebSocket(
      this.options.signalUrl ??
        `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/signal`,
    );
    this.ws = socket;
    socket.onopen = () =>
      this.send({
        type: "join",
        role: this.role,
        id: this.id,
        credential: this.credential,
        session: this.session,
        replace: this.force,
        ...this.options.join,
      });
    // Each socket gets its own ordered message chain, as before.
    this.incoming = Promise.resolve();
    socket.onmessage = (e) => {
      this.incoming = this.incoming
        .then(() => this.signal(JSON.parse(e.data) as Signal))
        .catch((err) => {
          this.signalingError(err);
        });
    };
    socket.onclose = (e) => {
      if (this.ws === socket) this.event("ws-close", String(e.code));
      if (this.stopped || this.ws !== socket) return;
      this.recovery.signalingLost();
      if (e.code === 1008) {
        // The relay closed for good (stream_ended): terminate and notify now,
        // not on the next poll, so nothing queued can bring media back.
        this.terminate("stream_ended");
        return;
      }
      // A deliberate replacement rejoins at once; only failures back off.
      if (this.immediateReconnect) { this.immediateReconnect = false; this.connect(); return; }
      this.retry++;
      this.reconnectTimer = setTimeout(
        () => this.connect(),
        Math.min(3000, 150 * 2 ** Math.min(this.retry, 4)) *
          (0.7 + Math.random() * 0.6),
      );
    };
  }
  private async signal(msg: Signal) {
    if (this.stopped) return;
    if (msg.type === "pong") {
      const now = performance.now();
      this.recovery.pong(now);
      return;
    }
    if (msg.type === "error") {
      // Signaling refused the join. The rate-limited "unauthorized" can be
      // transient (a burst after a server restart), so retry it briefly.
      if (msg.code === "unauthorized" && ++this.unauthorized < 4) {
        this.options.onMessage?.(msg as Record<string, unknown>);
        return;
      }
      this.options.onMessage?.(msg as Record<string, unknown>);
      this.terminate(msg.code ?? "error");
      return;
    }
    if (msg.type !== "joined" && msg.type !== "answer" && msg.type !== "candidate") {
      this.options.onMessage?.(msg as Record<string, unknown>);
      return;
    }
    if (msg.type === "joined") {
      this.retry = 0;
      this.unauthorized = 0;
      this.session = msg.session!;
      this.id = msg.stream_id ?? msg.id ?? this.id;
      this.lastJoined = msg;
      // startMedia() called from this handler only clears the deferral: the
      // PC is created once, below.
      this.inJoined = true;
      try {
        this.options.onMessage?.(msg as Record<string, unknown>);
      } catch (err) {
        console.error("joined handler", String(err));
      } finally {
        this.inJoined = false;
      }
      if (this.mediaDeferred) return;
      if (msg.resumed && this.pc && !this.force) {
        this.recover(performance.now());
        this.renegotiate();
        return;
      }
      await this.peer(msg);
    } else if (msg.type === "answer" && msg.sdp && this.pc) {
      await this.pc.setRemoteDescription(msg.sdp);
      this.event("answer");
      this.watchPair(this.pc);
      for (const c of this.candidates) await this.pc.addIceCandidate(c);
      this.candidates = [];
      this.renegotiate();
    } else if (msg.type === "candidate" && msg.candidate && this.pc) {
      if (this.pc.remoteDescription)
        await this.pc.addIceCandidate(msg.candidate);
      else this.candidates.push(msg.candidate);
    }
  }
  /** Create the PC now (after deferMedia). The sender passes its capture. */
  startMedia(stream?: MediaStream) {
    if (stream) {
      this.stream = stream;
      this.outgoing ??= stream.getVideoTracks()[0] ?? null;
    }
    if (!this.mediaDeferred || this.stopped) return;
    this.mediaDeferred = false;
    if (this.inJoined) return;
    const joined = this.lastJoined;
    if (joined && this.ws?.readyState === WebSocket.OPEN)
      this.incoming = this.incoming
        .then(() => this.peer({ ...joined, resumed: false }))
        .catch((err) => {
          this.signalingError(err);
        });
  }
  get mediaStarted(): boolean {
    return !this.mediaDeferred;
  }
  private negotiate = false;
  /**
   * Add to the current PC (a late audio transceiver) and renegotiate once
   * signaling is stable. If `fn` throws (a transform that can't attach), the
   * PC is replaced: never a transceiver that could send plaintext.
   */
  async extendCurrent(fn: (pc: RTCPeerConnection) => unknown): Promise<void> {
    const pc = this.pc;
    if (!pc || pc.signalingState === "closed") return;
    try {
      await fn(pc);
    } catch (err) {
      console.error("extend", String(err));
      if (pc === this.pc) {
        this.force = false;
        this.replace();
      }
      return;
    }
    if (pc !== this.pc) return;
    this.negotiate = true;
    this.renegotiate();
  }
  private renegotiate() {
    if (!this.negotiate || this.pc?.signalingState !== "stable" || this.ws?.readyState !== WebSocket.OPEN) return;
    this.negotiate = false;
    void this.offer();
  }
  /** Swap the sender's outgoing track (capture, a paused canvas, or null). False if a sender refused it. */
  async setOutgoing(track: MediaStreamTrack | null): Promise<boolean> {
    this.outgoing = track;
    const ok = await Promise.all([this.videoSender, this.thumbnailSender].map(s => s ? s.replaceTrack(track).then(() => true, () => false) : true));
    return ok.every(Boolean);
  }
  /** The viewer renders into this element from now on (it may be remounted). */
  attachVideo(v: HTMLVideoElement | null) {
    if (!v || v === this.video) return;
    const old = this.video;
    this.video = v;
    // The detached placeholder stops rendering once the page's player has the stream.
    if (old && !old.isConnected) old.srcObject = null;
    if (this.remoteStream) {
      if (v.srcObject !== this.remoteStream) v.srcObject = this.remoteStream;
      void v.play().catch(() => {});
      this.watchFrames(this.generation);
    }
  }
  private signalingError(err: unknown) {
    console.error("signaling", String(err));
    if (isE2EEError(err, "unsupported_codec")) this.terminate("unsupported_codec");
    else this.replace();
  }
  private terminate(code: string) {
    if (this.stopped) return;
    this.stopped = true;
    clearInterval(this.timer);
    clearTimeout(this.reconnectTimer);
    this.ws?.close();
    this.pc?.close();
    this.stateTo("ended");
    this.options.onTerminal?.(code);
  }
  private async peer(msg: Signal) {
    {
      this.force = false;
      this.negotiate = false; // the new PC's offer carries everything
      this.pc?.close();
      this.candidates = [];
      this.channel = undefined;
      this.heartbeatReady = false;
      this.peerStarted = performance.now();
      this.peerConnected = null;
      this.generation++;
      const gen = this.generation;
      const servers = this.tcpOnly
        ? msg.iceServers?.map((s) => ({
            ...s,
            urls: (Array.isArray(s.urls) ? s.urls : [s.urls]).filter((u) =>
              u.includes("transport=tcp"),
            ),
          }))
        : msg.iceServers;
      const pc = new RTCPeerConnection({
        iceServers: servers,
        iceTransportPolicy: this.tcpOnly ? "relay" : "all",
        bundlePolicy: "max-bundle",
        iceCandidatePoolSize: 1,
      });
      this.pc = pc;
      this.recovery.reset(performance.now());
      this.freshness = new VideoFreshness();
      this.catchup = new CatchupGrace();
      this.lastHeartbeat = performance.now();
      this.suspect = 0;
      this.bytes = 0;
      this.stamp = 0;
      this.stateTo(this.firstFrame ? "reconnecting" : "connecting");
      pc.onicecandidate = (e) => {
        if (e.candidate && pc === this.pc)
          this.event("candidate", `${e.candidate.type}/${e.candidate.protocol}${e.candidate.relayProtocol ? "/" + e.candidate.relayProtocol : ""}`);
        if (e.candidate)
          this.send({ type: "candidate", candidate: e.candidate.toJSON() });
      };
      pc.oniceconnectionstatechange = () => {
        if (pc === this.pc) this.event("ice", pc.iceConnectionState);
      };
      pc.onconnectionstatechange = () => {
        if (pc !== this.pc) return;
        if (pc.connectionState === "connected") this.peerConnected ??= performance.now();
        if (pc.connectionState === "failed") this.suspect ||= performance.now();
      };
      pc.createDataChannel("control");
      this.channel = pc.createDataChannel("heartbeat", {
        ordered: false,
        maxRetransmits: 0,
      });
      this.channel.onopen = () => {
        this.heartbeatReady = true;
        this.lastHeartbeat = performance.now();
        this.recovery.alive(performance.now());
      };
      this.channel.onmessage = (event) => {
        if (gen !== this.generation) return;
        this.lastHeartbeat = performance.now();
        this.suspect = 0;
        this.recovery.alive(performance.now());
        try {
          const v = JSON.parse(String(event.data)) as {
            repair?: unknown;
            layer?: number;
            spatial?: number;
            thumbnail?: boolean;
            ingressFrames: number;
            forwardedPackets: number;
            ingressPackets: number;
            keyframes: number;
            ingress?: Record<string, number>;
          };
          this.recovery.source(v.ingressFrames, performance.now());
          if (this.catchup.defer(performance.now(), v.repair)) this.recovery.deferVideo(performance.now());
          if (lossEvidence(v.repair)) this.playout.noteLoss(performance.now());
          Object.assign(this.quality, {
            repair: v.repair,
            ingressFrames: v.ingressFrames,
            forwardedPackets: v.forwardedPackets,
            ingressPackets: v.ingressPackets,
            keyframes: v.keyframes,
            ingress: v.ingress,
          });
          if (v.layer !== undefined) this.quality.layer = `${v.thumbnail ? "180p/" : `L${(v.spatial ?? 0) + 1}`}T${v.layer + 1}`;
        } catch {}
        if (this.role === "publish") this.stateTo("live");
      };
      if (this.role === "publish" && this.stream) {
        const track = this.stream.getVideoTracks()[0];
        // The transceiver starts without a track; the transform is attached
        // first, so no frame can leave unencrypted.
        const t = pc.addTransceiver("video", {
          direction: "sendonly",
          streams: [this.stream],
          sendEncodings: [publisherEncoding(track)],
        });
        this.preferVP9(t);
        this.options.encrypt?.(t.sender, 0);
        this.videoSender = t.sender;
        await t.sender.replaceTrack(this.outgoing);
        // Registered before its track is attached: a setOutgoing() meanwhile reaches it too.
        await addPublisherThumbnail(pc, this.stream, t => this.preferVP9(t), s => { this.options.encrypt?.(s, 2); this.thumbnailSender = s; }, this.outgoing);
        await this.mode(track.contentHint === "motion");
      } else {
        const t = pc.addTransceiver("video", { direction: "recvonly" });
        this.preferVP9(t);
        this.options.decrypt?.(t.receiver);
        pc.ontrack = (e) => {
          if (e.track.kind !== "video") return; // audio shares the video's MediaStream (same msid)
          this.remoteStream = e.streams[0] ?? new MediaStream([e.track]);
          this.video.srcObject = this.remoteStream;
          void this.video.play().catch(() => {});
          this.receiver = e.receiver;
          this.playout = new PlayoutPolicy();
          this.receiver.jitterBufferTarget = this.playout.target;
          this.watchFrames(gen);
        };
      }
      await this.options.extend?.(pc);
      await this.offer();
    }
  }
  private preferVP9(t: RTCRtpTransceiver) {
    const codecs = RTCRtpReceiver.getCapabilities("video")?.codecs ?? [];
    const vp9 = codecs.filter(c => c.mimeType.toLowerCase() === "video/vp9" && /(?:^|;)profile-id=0(?:;|$)/.test(c.sdpFmtpLine ?? ""));
    if (!vp9.length) throw new E2EEError("unsupported_codec", "PassStream requires VP9 profile 0 (non-flexible L1T3)");
    t.setCodecPreferences([...vp9, ...codecs.filter(c => this.role === "view" && ["video/red", "video/ulpfec"].includes(c.mimeType.toLowerCase()))]);
  }

  private offer() {
    this.offerQueue = this.offerQueue
      .catch(() => {})
      .then(async () => {
        const pc = this.pc;
        if (
          !pc ||
          pc.signalingState !== "stable" ||
          this.ws?.readyState !== WebSocket.OPEN
        )
          return;
        const created = await pc.createOffer();
        const offer = this.role === "publish" ? publisherOffer(created) : created;
		if (this.options.munge && offer.sdp) offer.sdp = this.options.munge(offer.sdp);
        await pc.setLocalDescription(offer);
        if (pc === this.pc) {
          this.send({ type: "offer", sdp: offer });
          this.event("offer");
        }
      });
    return this.offerQueue;
  }
  /**
   * Reconnect signaling once, now, so a changed `join` (the viewer's name)
   * reaches the server. Only before media starts: it doesn't touch a PC.
   */
  rejoin() {
    if (this.stopped || this.pc || this.ws?.readyState !== WebSocket.OPEN) return false;
    this.immediateReconnect = true;
    this.ws.close();
    return true;
  }
  private replace() {
    if (this.force || this.stopped) return;
    this.recovery.replaced(performance.now());
    if (this.video.videoWidth) {
      const c = document.createElement("canvas");
      c.width = this.video.videoWidth;
      c.height = this.video.videoHeight;
      c.getContext("2d")?.drawImage(this.video, 0, 0);
      this.video.poster = c.toDataURL();
      this.video.srcObject = null;
    }
    this.force = true;
    this.immediateReconnect = true;
    this.replacements++;
    this.event("replace");
    this.stateTo("reconnecting");
    this.pc?.close();
    this.ws?.close();
  }
  private watchPair(pc: RTCPeerConnection) {
    const transport = pc.getTransceivers()[0]?.receiver.transport?.iceTransport as
      (RTCIceTransport & { getSelectedCandidatePair?: () => { local: RTCIceCandidate; remote: RTCIceCandidate } | null }) | undefined;
    if (!transport || transport.onselectedcandidatepairchange) return;
    transport.onselectedcandidatepairchange = () => {
      if (pc !== this.pc) return;
      const pair = transport.getSelectedCandidatePair?.();
      this.event("pair", pair ? `${pair.local.type}/${pair.local.protocol}${pair.local.relayProtocol ? "/" + pair.local.relayProtocol : ""} -> ${pair.remote.type}/${pair.remote.protocol}` : "none");
    };
  }
  private watchFrames(gen: number) {
    const video = this.video;
    const frame: VideoFrameRequestCallback = (now, metadata) => {
      if (this.stopped || this.force || gen !== this.generation || video !== this.video) return;
      this.frames++;
      this.lastVideoFrame = performance.now();
      if (!this.firstFrame) this.firstFrame = performance.now() - this.started;
      this.stateTo("live");
      const { sample, marker } = decodeFrame(video, this.canvas);
      if (marker !== this.marker) { this.marker = marker; this.update(); }
      if (sample) this.quality.g2g = sample.delay;
      const previousFresh = this.freshness.count;
      this.freshness.observe(now, metadata);
      this.recovery.video(now, decoderLiveness(this.freshness.count > previousFresh, sample?.delay, videoFreshnessLimit(this.quality.repair)));
      if (this.role === "view" && previousFresh === 0 && this.freshness.count > 0)
        this.send({ type: "ping", at: Date.now(), freshFrames: this.freshness.count });
      if (previousFresh === 0 && this.freshness.count > 0 && this.channel?.readyState === "open")
        this.channel.send(JSON.stringify({ type: "heartbeat", at: Date.now(), freshFrames: this.freshness.count }));
      this.frameTimeline.push({
        at: Date.now(),
        delay: sample?.delay ?? null,
        frame: sample?.frame ?? null,
        rtpTimestamp: metadata.rtpTimestamp,
      });
      if (this.frameTimeline.length > 20000) this.frameTimeline.shift();
      video.requestVideoFrameCallback(frame);
    };
    video.requestVideoFrameCallback(frame);
  }
  async mode(motion: boolean) {
    const t = this.stream?.getVideoTracks()[0];
    if (!t) return;
    this.motion = motion;
    t.contentHint = motion ? "motion" : "text";
    // A processed outgoing track (safe mode) is what the encoder sees: give it the same hint.
    if (this.outgoing && this.outgoing !== t) this.outgoing.contentHint = t.contentHint;
    await t
      .applyConstraints({
        width: { max: 2560 },
        height: { max: 1440 },
        frameRate: motion ? 30 : 15,
      })
      .catch(() => {});
    const sender = this.videoSender ?? this.pc?.getSenders().find((s) => s.track === t);
    if (sender) {
      const p = sender.getParameters();
      p.degradationPreference = publisherEncoding(t).maxFramerate ? "maintain-resolution" : motion
        ? "maintain-framerate"
        : "maintain-resolution";
      if (p.encodings?.length) p.encodings[0].maxFramerate = publisherEncoding(t).maxFramerate ?? (motion ? 30 : 15);
      await sender.setParameters(p).catch(() => {});
    }
  }
  private async tick() {
    if (this.stopped) return;
    const now = performance.now();
    if (this.role === "view" && document.visibilityState === "hidden") this.recovery.deferVideo(now);
    if (
      this.role === "view" &&
      this.channel?.readyState === "open" &&
      now - this.lastVideoFrame > 1000 &&
      now - this.lastKeyframeRequest > 1000
    ) {
      this.channel.send(JSON.stringify({ type: "keyframe" }));
      this.event("keyframe-request");
      this.lastKeyframeRequest = now;
      this.stateTo("reconnecting");
    }
    if (this.channel?.readyState === "open")
      this.channel.send(JSON.stringify({ type: "heartbeat", at: Date.now(),freshFrames:this.freshness.count }));
    this.send({ type: "ping", at: Date.now(), freshFrames: this.role === "view" ? this.freshness.count : undefined });
    if (!this.heartbeatReady && heartbeatStartupExpired(now, this.peerStarted, this.role === "view" ? this.lastVideoFrame : undefined, this.peerConnected)) this.replace();
    if (!this.pc || this.statsBusy) return;
    this.statsBusy = true;
    try {
      const pc = this.pc;
      const stats = await (this.role === "publish" ? publisherStats(pc, this.videoSender) : pc.getStats());
      if (pc !== this.pc) return;
      this.recovery.observe(stats, performance.now());
      if (this.heartbeatReady) this.recover(performance.now());
      const pair = selectedPair(stats);
      let q = { ...this.quality, timestamp: Date.now(), frames: this.frames };
      for (const s of stats.values()) {
        if (
          s.type === "candidate-pair" &&
          s.id === pair?.id
        ) {
          q.rtt = (s.currentRoundTripTime ?? 0) * 1000;
          const local = stats.get(s.localCandidateId),
            remote = stats.get(s.remoteCandidateId);
          q.path =
            (local?.candidateType === "relay" ||
            remote?.candidateType === "relay"
              ? "TURN/"
              : "") +
            (local?.relayProtocol ?? local?.protocol ?? "").toUpperCase();
        }
        if (
          (s.type === "inbound-rtp" || s.type === "outbound-rtp") &&
          s.kind === "video"
        ) {
          const bytes = s.bytesReceived ?? s.bytesSent ?? 0;
          if (this.stamp && s.timestamp > this.stamp)
            q.bitrate =
              ((bytes - this.bytes) * 8000) / (s.timestamp - this.stamp);
          this.bytes = bytes;
          this.stamp = s.timestamp;
          q.fps = s.framesPerSecond ?? 0;
          q.freezes = s.freezeCount ?? 0;
          q.freezeSeconds = s.totalFreezesDuration ?? 0;
          const lost = s.packetsLost ?? 0,
            received = s.packetsReceived ?? 0;
          const total = lost - this.lost + received - this.received;
          q.loss =
            total > 0 ? Math.max(0, (lost - this.lost) / total) * 100 : 0;
          this.lost = lost;
          this.received = received;
          q.layer = s.scalabilityMode ?? q.layer;
          if (s.type === "inbound-rtp" && this.receiver) {
            const target = this.playout.observe(s.nackCount ?? 0, s.freezeCount ?? 0, q.rtt, performance.now(),lossEvidence(q.repair));
            if (this.receiver.jitterBufferTarget !== target) this.receiver.jitterBufferTarget = target;
            q.playoutTarget = target;
          }
          const keys = ["jitterBufferDelay", "jitterBufferEmittedCount", "jitterBufferTargetDelay", "framesDropped", "fecPacketsReceived", "fecPacketsDiscarded", "packetsLost", "packetsReceived", "nackCount", "pliCount", "retransmittedBytesSent", "framesEncoded", "framesDecoded", "totalPacketSendDelay", "targetBitrate", "totalEncodeTime", "qualityLimitationReason"];
          q.stats = Object.fromEntries(keys.flatMap(k => typeof s[k] === "number" || typeof s[k] === "string" ? [[k,s[k]]] : []));
        }
      }
      this.quality = q;
      this.samples.push({ ...q, width: this.video.videoWidth, height: this.video.videoHeight });
      if (this.samples.length > 7200) this.samples.shift();
      this.update();
    } catch {
      /* PC replacement can race a stats query. */
    } finally {
      this.statsBusy = false;
    }
  }
  private recover(now: number) {
    const action = this.recovery.action(now, this.quality.rtt, this.ws?.readyState === WebSocket.OPEN, transportBaselineRTT(this.quality.repair));
    this.suspect = this.recovery.suspect;
    if (action === "none") return;
    this.event(action, `alive ${Math.round(now - this.recovery.lastAlive)} ms ago`);
    this.replace();
  }
  stop() {
    this.stopped = true;
    clearInterval(this.timer);
    clearTimeout(this.reconnectTimer);
    this.send({ type: "leave" });
    this.ws?.close();
    this.pc?.close();
    if (this.stream) stopTestSource(this.stream);
    this.stream?.getTracks().forEach((t) => t.stop());
    this.stateTo("ended");
  }
}
declare global {
  interface RTCIceCandidate {
    readonly relayProtocol?: string;
  }
  interface RTCRtpEncodingParameters {
    scalabilityMode?: string;
  }
  interface Window {
    passstream?: Session;
  }
}
