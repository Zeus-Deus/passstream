import { describe, expect, it } from "vitest";
import type { Quality } from "../session";
import { senderHealth } from "./health";

const capture = () => ({ getVideoTracks: () => [{ getSettings: () => ({ width: 1920, height: 1080, deviceId: "secret" }) }] });
const quality = (timestamp: number, totalEncodeTime: number): Pick<Quality, "timestamp" | "bitrate" | "fps" | "stats"> => ({ timestamp, bitrate: 2048000, fps: 30, stats: { framesEncoded: 100, totalEncodeTime, qualityLimitationReason: "cpu", pin: "secret" } });

describe("sender health allowlist", () => {
  it("reports rate, encode work, resolution and bounded interval encode CPU", () => {
    const c = capture();
    expect(senderHealth(null)).toBeNull();
    expect(senderHealth(c, quality(1000, 1))?.encode_cpu_pct).toBeNull();
    const h = senderHealth(c, quality(2000, 1.25));
    expect(h).toEqual({ uplink_kbps: 2048, encode_ms: 12.5, encode_cpu_pct: 25, fps: 30, width: 1920, height: 1080, quality_limitation: "cpu" });
    expect(JSON.stringify(h)).not.toContain("secret");
    expect(senderHealth(c, quality(2000, 1.25))?.encode_cpu_pct).toBe(25);
    expect(senderHealth(c, quality(3000, 10))?.encode_cpu_pct).toBe(100);
    expect(senderHealth(c, quality(4000, 0))?.encode_cpu_pct).toBeNull();
  });
  it("never emits invalid numbers or arbitrary reason strings", () => {
    const q = quality(1000, Infinity);
    q.bitrate = Infinity; q.fps = NaN;
    q.stats!.qualityLimitationReason = "link-secret";
    const h = senderHealth(capture(), q);
    expect(h?.uplink_kbps).toBeNull(); expect(h?.encode_ms).toBeNull(); expect(h?.fps).toBeNull();
    expect(h?.quality_limitation).toBe("other");
  });
});

it("rounds fractional capture dimensions to wire integers", () => {
 const c = {getVideoTracks: () => [{getSettings: () => ({width: 1919.8, height:1079.6})}]};
 const h = senderHealth(c, quality(1000,1));
 expect(h?.width).toBe(1920); expect(h?.height).toBe(1080);
 expect(Number.isInteger(h?.uplink_kbps)).toBe(true);
});
