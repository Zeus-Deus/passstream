import { describe, expect, it } from "vitest";
import { withAudioNack } from "./audio";

const offer = ["v=0", "m=audio 9 UDP/TLS/RTP/SAVPF 111", "a=rtpmap:111 opus/48000/2", "a=rtcp-fb:111 transport-cc", "a=fmtp:111 minptime=10;useinbandfec=1", ""].join("\r\n");

describe("audio NACK offer", () => {
  it("adds nack feedback to Opus once", () => {
    const m = withAudioNack(offer);
    expect(m).toContain("a=rtpmap:111 opus/48000/2\r\na=rtcp-fb:111 nack\r\n");
    expect(withAudioNack(m)).toBe(m);
  });
  it("leaves an offer without Opus alone", () => {
    const sdp = "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 98\r\na=rtpmap:98 VP9/90000\r\n";
    expect(withAudioNack(sdp)).toBe(sdp);
  });
});
