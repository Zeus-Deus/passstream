import { describe, expect, it } from "vitest";
import { statusSize } from "./source";

/**
 * Review D2 r2 #3: the curtain picture must differ in size from the capture
 * on every encoding (native, encoder-scaled, the 180p one), so the swap
 * forces a keyframe. Checked for exact 720p and the scalings encoders use.
 */
describe("status picture size", () => {
  const thumb = (w: number, h: number, captureW: number) => [Math.round(w / (captureW / 320)), Math.round(h / (captureW / 320))];
  const even = (v: number) => 2 * Math.round(v / 2);
  const captures: [number, number][] = [
    [1280, 720], [1920, 1080], [2560, 1440], [3840, 2160], [1366, 768], [1280, 800], [1440, 900], [1680, 1050],
    [1920, 1200], [2560, 1600], [1024, 768], [1600, 1200], [2560, 1080], [3440, 1440], [960, 540], [800, 600],
  ];
  it("an exact 1280×720 capture gets a different picture (was the same 1280×720)", () => {
    expect(statusSize({ width: 1280, height: 720 })).not.toEqual({ width: 1280, height: 720 });
  });
  it("never equals the capture, any encoder downscale of it, or the 180p encoding of either", () => {
    for (const [w, h] of captures) {
      const p = statusSize({ width: w, height: h });
      expect([p.width, p.height]).not.toEqual([w, h]);
      // Encoder downscales (maintain-framerate adaptation, scaleResolutionDownBy) keep the aspect, rounded to even sizes.
      for (const f of [1, 1.25, 1.5, 2, 2.5, 3, 4]) expect([p.width, p.height]).not.toEqual([even(w / f), even(h / f)]);
      // The independent 180p encoding scales both by the capture's width / 320.
      expect(thumb(p.width, p.height, w)).not.toEqual(thumb(w, h, w));
    }
  });
  it("keeps the marker row readable on the 180p encoding: ≥ 1.5 px cells for captures up to 2560 wide (review D2 r2, e2ee low_rendition)", () => {
    for (const [w, h] of captures.filter(([w]) => w <= 2560 && w >= 960)) {
      const p = statusSize({ width: w, height: h });
      const lowWidth = p.width / (w / 320);
      expect(lowWidth * (12 / 1280)).toBeGreaterThanOrEqual(1.5);
    }
  });
  it("defaults to 1280×720 without capture settings", () => {
    expect(statusSize(null)).toEqual({ width: 1280, height: 720 });
    expect(statusSize({})).toEqual({ width: 1280, height: 720 });
  });
});
