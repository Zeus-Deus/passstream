import { describe, expect, it } from "vitest";
import { VideoFreshness, decoderLiveness, heartbeatStartupExpired } from "./freshness";
import { RecoveryPolicy } from "./recovery";

describe("content-independent decoder freshness", () => {
  it("keeps advancing video when SCTP setup outlasts its startup deadline", () => {
    expect(heartbeatStartupExpired(20_000, 0, 19_995)).toBe(false);
    expect(heartbeatStartupExpired(20_000, 0, 19_000)).toBe(false);
  });
  it("still retries missing or stalled media after the startup deadline", () => {
    expect(heartbeatStartupExpired(15_000, 0)).toBe(false);
    expect(heartbeatStartupExpired(20_000, 0)).toBe(true);
    expect(heartbeatStartupExpired(20_000, 0, 18_999)).toBe(true);
  });
  it("counts advancing received frames without reading pixels", () => {
    const f = new VideoFreshness();
    expect(f.observe(100, { presentedFrames: 1, rtpTimestamp: 9000, receiveTime: 90 })).toBe(1);
    expect(f.observe(200, { presentedFrames: 2, rtpTimestamp: 18000, receiveTime: 190 })).toBe(2);
  });
  it("rejects repeated, late and replayed frames and accepts timestamp wrap", () => {
    const f = new VideoFreshness();
    f.observe(100, { presentedFrames: 1, rtpTimestamp: 0xfffffff0, receiveTime: 90 });
    expect(f.observe(200, { presentedFrames: 2, rtpTimestamp: 16, receiveTime: 190 })).toBe(2);
    expect(f.observe(300, { presentedFrames: 3, rtpTimestamp: 16, receiveTime: 290 })).toBe(2);
    expect(f.observe(400, { presentedFrames: 4, rtpTimestamp: 32, receiveTime: -1000 })).toBe(2);
    expect(f.observe(500, { presentedFrames: 4, rtpTimestamp: 48, receiveTime: 490 })).toBe(2);
    expect(f.observe(600, { presentedFrames: 5, rtpTimestamp: 40, receiveTime: 590 })).toBe(2);
    expect(f.observe(700, { presentedFrames: 6 })).toBe(2);
  });
});

it("does not confuse a remapped capture estimate with stopped decoding", () => {
  const f = new VideoFreshness();
  expect(f.observe(2000, { presentedFrames: 1, rtpTimestamp: 9000, receiveTime: 1990, captureTime: 500 })).toBe(1);
  expect(f.observe(2100, { presentedFrames: 2, rtpTimestamp: 18000, receiveTime: 2090, captureTime: 2050 })).toBe(2);
});

it("allows finite paced catch-up without masking an endless queue", async () => {
 const { CatchupGrace }=await import("./freshness");const g=new CatchupGrace();
 expect(g.defer(0,{catchupMs:800})).toBe(true);
 expect(g.defer(5999,{catchupMs:800})).toBe(true);
 expect(g.defer(6000,{catchupMs:800})).toBe(false);
 expect(g.defer(10000,{catchupMs:800})).toBe(false);
 expect(g.defer(11000,{catchupMs:0})).toBe(false);
 expect(g.defer(12000,{catchupMs:100})).toBe(true);
 expect(g.defer(13000,{catchupMs:"100"})).toBe(false);
});

it("counts presented frames when callbacks coalesce", () => {
  const f = new VideoFreshness();
  expect(f.observe(100, {presentedFrames: 20, rtpTimestamp: 9000, receiveTime: 90})).toBe(1);
  expect(f.observe(300, {presentedFrames: 23, rtpTimestamp: 27000, receiveTime: 290})).toBe(4);
  expect(f.observe(400, {presentedFrames: 24, rtpTimestamp: 27000, receiveTime: 390})).toBe(4);
  expect(f.observe(500, {presentedFrames: 5000, rtpTimestamp: 36000, receiveTime: 490})).toBe(5);
});

it("keeps a receiving decoder alive through capture latency and still replaces a stall", () => {
  const freshness = new VideoFreshness(), recovery = new RecoveryPolicy();
  recovery.reset(0);
  for (let now = 500; now <= 5000; now += 500) {
    const previous = freshness.count;
    freshness.observe(now, { presentedFrames: now / 500, rtpTimestamp: now * 90, receiveTime: now - 10, captureTime: now - 1500 });
    recovery.video(now, decoderLiveness(freshness.count > previous, 1500));
    recovery.source(now / 500, now);
    recovery.pong(now);
    expect(recovery.action(now, 800, true, 1)).toBe("none");
  }
  recovery.pong(5500);
  recovery.pong(6000);
  const now = 6501, previous = freshness.count;
  freshness.observe(now, { presentedFrames: 11, rtpTimestamp: 5000 * 90, receiveTime: now - 10 });
  recovery.video(now, decoderLiveness(freshness.count > previous, 1500));
  recovery.source(20, now);
  recovery.pong(now);
  expect(recovery.action(now, 800, true, 1)).toBe("replace");
});

it("retains the extreme stale-picture watchdog without treating FIFO latency as a stopped decoder", () => {
  expect(decoderLiveness(true)).toBe(true);
  expect(decoderLiveness(true, 1100)).toBe(true);
  expect(decoderLiveness(true, 3000)).toBe(true);
  expect(decoderLiveness(true, 5000)).toBe(false);
  expect(decoderLiveness(false, 10)).toBe(false);
  const recovery = new RecoveryPolicy();
  recovery.reset(0);
  recovery.video(0);
  for (let now = 500; now <= 1500; now += 500) {
    recovery.source(now / 500, now);
    recovery.pong(now);
    recovery.alive(now);
    recovery.video(now, decoderLiveness(true, 5000));
  }
  expect(recovery.action(1500, 2000, true, 1)).toBe("replace");
});


it("gives connected media its own startup window after a slow ICE/DTLS handshake", () => {
 expect(heartbeatStartupExpired(20_000,0,undefined,14_950)).toBe(false);
 expect(heartbeatStartupExpired(29_950,0,undefined,14_950)).toBe(false);
 expect(heartbeatStartupExpired(29_951,0,undefined,14_950)).toBe(true);
 expect(heartbeatStartupExpired(29_951,0,29_950,14_950)).toBe(false);
});
it("bounds an unconnected attempt without repeatedly restarting its handshake", () => {
 expect(heartbeatStartupExpired(20_000,0,undefined,null)).toBe(false);
 expect(heartbeatStartupExpired(45_000,0,undefined,null)).toBe(false);
 expect(heartbeatStartupExpired(45_001,0,undefined,null)).toBe(true);
});
