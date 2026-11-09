import { expect, it, vi } from "vitest";
import { addPublisherThumbnail, publisherOffer } from "./thumbnail";

it("keeps the publisher floor scoped to VP9 video when audio reuses a payload ID", () => {
  const offer = { type: "offer", sdp: [
    "v=0", "m=audio 9 UDP/TLS/RTP/SAVPF 98", "a=rtpmap:98 opus/48000/2", "a=fmtp:98 useinbandfec=1",
    "m=video 9 UDP/TLS/RTP/SAVPF 98 100", "a=rtpmap:98 VP9/90000", "a=fmtp:98 profile-id=0",
    "a=rtpmap:100 H264/90000", "a=fmtp:100 profile-level-id=42001f", "",
  ].join("\r\n") } satisfies RTCSessionDescriptionInit;
  const configured = publisherOffer(offer);
  expect(configured.sdp).toContain("a=fmtp:98 profile-id=0;x-google-min-bitrate=128\r\n");
  expect(configured.sdp).toContain("a=fmtp:98 useinbandfec=1\r\n");
  expect(configured.sdp).toContain("a=fmtp:100 profile-level-id=42001f\r\n");
  expect(publisherOffer(configured)).toEqual(configured);
  expect(offer.sdp).not.toContain("x-google-min-bitrate");
});

it("attaches the thumbnail transform before setting its track", async () => {
  const calls: unknown[] = [];
  const track = { getSettings: () => ({ width: 2560 }) } as MediaStreamTrack;
  const stream = { getVideoTracks: () => [track] } as MediaStream;
  vi.stubGlobal("MediaStream", class {});
  const sender = { replaceTrack: async (t: unknown) => { calls.push(t); } } as RTCRtpSender;
  const pc = { addTransceiver: (kind: unknown) => { calls.push(kind); return { sender }; } } as RTCPeerConnection;
  await addPublisherThumbnail(pc, stream, () => {}, () => { calls.push("transform"); });
  expect(calls).toEqual(["video", "transform", track]);
  calls.length = 0;
  await expect(addPublisherThumbnail(pc, stream, () => {}, () => { throw new Error("attach refused"); })).rejects.toThrow("attach refused");
  expect(calls).toEqual(["video"]);
  vi.unstubAllGlobals();
});
