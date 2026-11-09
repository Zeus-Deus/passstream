/**
 * E2EE audio: one Opus transceiver per PC, SFrame track 1 (video is track 0),
 * fully encrypted (no clear prefix). The transform is attached before the
 * track is set and before negotiation, like video, so no audio frame can
 * leave or play unencrypted. The SFU forwards it opaquely (internal/sfu/audio.go).
 *
 * Opus only: audio RED is refused because Chrome splits RED after the
 * receive transform, which would hand the decryptor a RED container.
 */
export const AUDIO_TRACK = 1;

function preferOpus(t: RTCRtpTransceiver) {
  const codecs = RTCRtpReceiver.getCapabilities("audio")?.codecs.filter((c) => c.mimeType.toLowerCase() === "audio/opus");
  if (codecs?.length) t.setCodecPreferences(codecs);
}

/** The PC's audio transceiver, if it has one. */
export function audioTransceiver(pc: RTCPeerConnection): RTCRtpTransceiver | undefined {
  return pc.getTransceivers().find((t) => t.receiver.track?.kind === "audio");
}

/** The publisher's audio sender. `track` may be null (paused or test m-line only): nothing is sent. */
export async function addAudioSender(pc: RTCPeerConnection, stream: MediaStream, track: MediaStreamTrack | null, attach: (s: RTCRtpSender) => void): Promise<RTCRtpSender> {
  const t = pc.addTransceiver("audio", { direction: "sendonly", streams: [stream] });
  preferOpus(t);
  attach(t.sender); // throws (and the PC is replaced) if the transform can't attach: never plaintext
  await t.sender.replaceTrack(track);
  return t.sender;
}

/** The viewer's audio receiver, decrypt transform attached before negotiation. */
export function addAudioReceiver(pc: RTCPeerConnection, attach: (r: RTCRtpReceiver) => void): RTCRtpReceiver {
  const t = pc.addTransceiver("audio", { direction: "recvonly" });
  preferOpus(t);
  attach(t.receiver);
  return t.receiver;
}

/**
 * Ask for NACK on Opus: Chrome supports audio NACK but doesn't offer it. The
 * SFU answers it from a small per-viewer cache (cheap repair for Wi-Fi loss).
 */
export function withAudioNack(sdp: string): string {
  const pt = sdp.match(/^a=rtpmap:(\d+) opus\/48000/im)?.[1];
  if (!pt || new RegExp(`^a=rtcp-fb:${pt} nack\\r?$`, "m").test(sdp)) return sdp;
  return sdp.replace(new RegExp(`^(a=rtpmap:${pt} opus/48000[^\\r\\n]*)(\\r?\\n)`, "im"), `$1$2a=rtcp-fb:${pt} nack$2`);
}

/** Test source: a sine tone on its own audio track (sender `?tone=<Hz>`). */
export function addTestTone(stream: MediaStream, hz: number): void {
  const ctx = new AudioContext();
  const osc = ctx.createOscillator();
  osc.frequency.value = hz;
  const gain = ctx.createGain();
  gain.gain.value = 0.3;
  const dest = ctx.createMediaStreamDestination();
  osc.connect(gain).connect(dest);
  osc.start();
  void ctx.resume().catch(() => undefined);
  const track = dest.stream.getAudioTracks()[0]!;
  const stop = track.stop.bind(track);
  track.stop = () => {
    stop();
    void ctx.close().catch(() => undefined);
  };
  stream.addTrack(track);
}
