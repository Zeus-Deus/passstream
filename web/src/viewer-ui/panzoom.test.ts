import { describe, expect, it } from "vitest";
import { clamp, FIT, MAX_SCALE, panBy, pictureRect, toggleZoom, zoomTo, type Rect } from "./panzoom";
import { isRowCode, rowCodeColour, rowCodeIndex } from "./rowcode";

const size = { w: 400, h: 800 };

describe("panzoom", () => {
  it("zooms around the pinch point", () => {
    const v = zoomTo(FIT, 2, { x: 100, y: 200 }, size);
    expect(v).toEqual({ scale: 2, x: -100, y: -200 });
    // The content under the point stays under it.
    expect((100 - v.x) / v.scale).toBe(100);
  });

  it("never zooms out past fit or beyond the max", () => {
    expect(zoomTo(FIT, 0.3, { x: 10, y: 10 }, size)).toEqual(FIT);
    expect(zoomTo(FIT, 99, { x: 0, y: 0 }, size).scale).toBe(MAX_SCALE);
  });

  it("can't pan past the edges", () => {
    const v = zoomTo(FIT, 2, { x: 0, y: 0 }, size);
    expect(panBy(v, 50, 50, size)).toEqual({ scale: 2, x: 0, y: 0 });
    expect(panBy(v, -9999, -9999, size)).toEqual({ scale: 2, x: -400, y: -800 });
    expect(clamp({ scale: 1, x: -20, y: 5 }, size)).toEqual(FIT);
  });

  it("double tap toggles between fit and zoomed in", () => {
    const z = toggleZoom(FIT, { x: 200, y: 400 }, size);
    expect(z.scale).toBe(2.5);
    expect(toggleZoom(z, { x: 0, y: 0 }, size)).toEqual(FIT);
  });
});

describe("row codes (CONTRACT §4.3)", () => {
  it("index and colour", () => {
    expect(rowCodeIndex("AAA")).toBe(0);
    expect(rowCodeIndex("999")).toBe(10647);
    expect(rowCodeColour("AAA")).toBe("red");
    expect(rowCodeColour("AAC")).toBe("orange");
    expect(isRowCode("K7P")).toBe(true);
    expect(isRowCode("K0P")).toBe(false);
    expect(rowCodeColour("bad")).toBeNull();
  });
});

describe("zoom bounds are the rendered picture, not the letterboxed layer", () => {
  // A 16:9 picture letterboxed in a 390×844 portrait stage: 390×219.4 at y 312.3.
  const stage = { w: 390, h: 844 };
  const picture = pictureRect(stage, { w: 1280, h: 720 }, false);
  const portrait = { ...stage, picture };
  const onScreen = (v: { scale: number; x: number; y: number }, p: Rect) => ({ left: v.x + v.scale * p.x, top: v.y + v.scale * p.y, right: v.x + v.scale * (p.x + p.w), bottom: v.y + v.scale * (p.y + p.h) });

  it("fit letterboxes, fill covers", () => {
    expect(picture).toEqual({ x: 0, y: 312.3125, w: 390, h: 219.375 });
    expect(pictureRect(stage, { w: 1280, h: 720 }, true)).toEqual({ x: 0, y: 0, w: 390, h: 844 });
    expect(pictureRect(stage, null, false)).toEqual({ x: 0, y: 0, w: 390, h: 844 });
  });

  it("max pan never moves the picture out of view", () => {
    const z = zoomTo(FIT, 2.5, { x: 195, y: 422 }, portrait);
    for (const [dx, dy] of [[9999, 9999], [-9999, -9999], [9999, -9999], [-9999, 9999]]) {
      const r = onScreen(panBy(z, dx, dy, portrait), picture);
      // Wider than the stage: covers it. Shorter than the stage (548 < 844): centred.
      expect(r.left).toBeLessThanOrEqual(0);
      expect(r.right).toBeGreaterThanOrEqual(390);
      expect((r.top + r.bottom) / 2).toBeCloseTo(422, 6);
    }
  });

  it("rotating while zoomed re-clamps to the new stage with no gap", () => {
    const z = panBy(zoomTo(FIT, 2.5, { x: 195, y: 422 }, portrait), -9999, -9999, portrait);
    const land = { w: 844, h: 390 };
    const lp = pictureRect(land, { w: 1280, h: 720 }, false);
    const r = onScreen(clamp(z, { ...land, picture: lp }), lp);
    expect(r.left).toBeLessThanOrEqual(0);
    expect(r.right).toBeGreaterThanOrEqual(844);
    expect(r.top).toBeLessThanOrEqual(0);
    expect(r.bottom).toBeGreaterThanOrEqual(390);
  });

  it("fit stays the identity view", () => {
    expect(clamp(FIT, portrait)).toEqual(FIT);
    expect(toggleZoom(toggleZoom(FIT, { x: 10, y: 400 }, portrait), { x: 0, y: 0 }, portrait)).toEqual(FIT);
  });
});
