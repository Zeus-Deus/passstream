export const THUMBNAIL_TRACK = 2;

// A large screen's lowest VP9 spatial layer can still exceed a weak viewer's
// whole budget. Keep an independent 180p encoding so its references and small
// keyframes can be admitted without reducing the full-resolution source.
export async function addPublisherThumbnail(pc: RTCPeerConnection, stream: MediaStream, preferCodec: (t: RTCRtpTransceiver) => void, attach: (sender: RTCRtpSender) => void, outgoing: MediaStreamTrack | null = stream.getVideoTracks()[0]): Promise<RTCRtpSender | undefined> {
  const source = stream.getVideoTracks()[0];
  const width = source?.getSettings().width ?? 0;
  if (width < 960) return;
  // Sharing the track also shares capture enable/stop, while each sender scales independently.
  const track = source;
  const t = pc.addTransceiver("video", {
    direction: "sendonly",
    streams: [new MediaStream([track])],
    sendEncodings: [{ maxBitrate: width >= 1920 ? 80_000 : 100_000, maxFramerate: 15, scaleResolutionDownBy: width / 320, scalabilityMode: "L1T3" }],
  });
  preferCodec(t);
  attach(t.sender);
  await t.sender.replaceTrack(outgoing);
  return t.sender;
}

// Keep the publisher's HUD/source measurements on its full-resolution encoding.
export function publisherStats(pc: RTCPeerConnection, sender?: RTCRtpSender) {
  return sender?.getStats() ?? pc.getStats();
}

// Chromium's VP9 loss controller otherwise falls to 30 kbit/s even when NACK
// repairs the path. Bound that floor so independent keyframes remain viable.
export function publisherOffer(offer: RTCSessionDescriptionInit): RTCSessionDescriptionInit {
  return { ...offer, sdp: offer.sdp?.split(/(?=^m=)/m).map(section => {
    if (!section.startsWith("m=video ")) return section;
    const payloads = new Set([...section.matchAll(/^a=rtpmap:(\d+) VP9\/90000\r?$/gim)].map(m => m[1]));
    return section.replace(/^a=fmtp:(\d+) ([^\r\n]*)/gm, (line: string, payload: string, params: string) =>
      payloads.has(payload) ? `a=fmtp:${payload} ${params.replace(/;?x-google-min-bitrate=[^;]*/g, "")};x-google-min-bitrate=128` : line);
  }).join("") };
}
