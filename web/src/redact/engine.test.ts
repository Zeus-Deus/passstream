import { describe, expect, it } from "vitest";
import { Engine } from "./engine";
import type { OcrJob } from "./ocr-worker";

const W = 640, H = 360;
const BG = 0xfff0f0f0, INK = 0xff202020;

/** A frame with fake text lines: rows of glyph-like strokes at (x, y), `n` glyphs each. */
function frame(lines: { x: number; y: number; n: number; seed?: number }[], bg = BG, ink = INK): Uint32Array {
  const px = new Uint32Array(W * H).fill(bg);
  for (const l of lines) {
    for (let g = 0; g < l.n; g++) {
      const gx = l.x + g * 9;
      const tall = ((g + (l.seed ?? 0)) * 7) % 3; // vary glyph shapes so lines differ
      for (let y = l.y + (tall === 0 ? 3 : 0); y < l.y + 14; y++) for (let x = gx; x < gx + 3 + tall; x++) px[y * W + x] = ink;
    }
  }
  return px;
}

function setup() {
  const jobs: OcrJob[] = [];
  const engine = new Engine((j) => jobs.push(...j));
  return { engine, jobs };
}

describe("safe mode engine", () => {
  it("covers new text until it is checked (fail-closed)", () => {
    const { engine, jobs } = setup();
    engine.frame(frame([{ x: 40, y: 40, n: 20 }, { x: 40, y: 100, n: 12, seed: 1 }]), W, H);
    expect(jobs.length).toBe(2);
    const covers = engine.covers();
    expect(covers.map((c) => c.kind)).toEqual(["pending", "pending"]);
    // Every ink pixel of the lines lies under a cover.
    expect(covers.some((c) => c.x <= 40 && c.x + c.w >= 40 + 19 * 9 + 3 && c.y <= 40 && c.y + c.h >= 54)).toBe(true);
  });

  it("uncovers checked clean lines and keeps sensitive spans covered", () => {
    const { engine, jobs } = setup();
    engine.frame(frame([{ x: 40, y: 40, n: 20 }, { x: 40, y: 100, n: 12, seed: 1 }]), W, H);
    engine.results([
      { key: jobs[0].key, result: { covers: [] } },
      { key: jobs[1].key, result: { covers: [{ x0: 10, x1: 50, category: "card" }] } },
    ]);
    const covers = engine.covers();
    expect(covers).toHaveLength(1);
    expect(covers[0].kind).toBe("card");
    expect(engine.pendingCount()).toBe(0);
  });

  it("does not re-read unchanged or scrolled lines", () => {
    const { engine, jobs } = setup();
    engine.frame(frame([{ x: 40, y: 40, n: 20 }, { x: 40, y: 100, n: 12, seed: 1 }]), W, H);
    engine.results(jobs.map((j) => ({ key: j.key, result: { covers: [] } })));
    jobs.length = 0;
    // Same picture: nothing changes, nothing is submitted.
    expect(engine.frame(frame([{ x: 40, y: 40, n: 20 }, { x: 40, y: 100, n: 12, seed: 1 }]), W, H).changed).toBe(false);
    // Scrolled up by 23 px: same content elsewhere hits the cache.
    engine.frame(frame([{ x: 40, y: 17, n: 20 }, { x: 40, y: 77, n: 12, seed: 1 }]), W, H);
    expect(jobs).toHaveLength(0);
    expect(engine.covers()).toHaveLength(0);
  });

  it("covers a line again when its pixels change", () => {
    const { engine, jobs } = setup();
    engine.frame(frame([{ x: 40, y: 40, n: 20 }]), W, H);
    engine.results(jobs.map((j) => ({ key: j.key, result: { covers: [] } })));
    jobs.length = 0;
    engine.frame(frame([{ x: 40, y: 40, n: 21 }]), W, H); // one more glyph typed
    expect(jobs).toHaveLength(1);
    expect(engine.covers().map((c) => c.kind)).toEqual(["pending"]);
  });

  it("finds text against a tall border instead of dropping it", () => {
    const { engine, jobs } = setup();
    const px = frame([{ x: 40, y: 40, n: 20 }]);
    // A window edge running past the line, touching its last glyph.
    for (let y = 0; y < H; y++) for (let x = 40 + 20 * 9; x < 40 + 20 * 9 + 2; x++) px[y * W + x] = INK;
    engine.frame(px, W, H);
    expect(jobs.length).toBeGreaterThanOrEqual(1);
    expect(engine.covers().some((c) => c.kind === "pending" && c.y <= 40 && c.y + c.h >= 54)).toBe(true);
  });

  it("finds low-contrast and saturated text (light grey on white, pure blue on black)", () => {
    for (const [bg, ink] of [
      [0xffffffff, 0xffdcdcdc], // #dcdcdc on white: a luma step of 35
      [0xff000000, 0xffff0000], // pure blue (RGBX little-endian) on black: a luma step of 28
    ]) {
      const { engine, jobs } = setup();
      engine.frame(frame([{ x: 40, y: 40, n: 20 }], bg, ink), W, H);
      expect(jobs.length).toBe(1);
      expect(engine.covers().some((c) => c.kind === "pending" && c.y <= 40 && c.y + c.h >= 54)).toBe(true);
    }
  });

  it("reads two rows inside a bordered box as two lines", () => {
    const { engine, jobs } = setup();
    const px = frame([{ x: 50, y: 50, n: 20 }, { x: 50, y: 72, n: 14, seed: 2 }]);
    // A 1 px outline around both rows, 66 px tall (under the 80 px line limit).
    for (let x = 40; x < 240; x++) px[40 * W + x] = px[106 * W + x] = INK;
    for (let y = 40; y <= 106; y++) px[y * W + 40] = px[y * W + 239] = INK;
    engine.frame(px, W, H);
    expect(jobs.length).toBeGreaterThanOrEqual(2);
    const rows = engine.covers().filter((c) => c.kind === "pending");
    expect(rows.some((c) => c.y <= 50 && c.y + c.h >= 64 && c.h < 48)).toBe(true);
    expect(rows.some((c) => c.y <= 72 && c.y + c.h >= 86 && c.h < 48)).toBe(true);
  });
});
