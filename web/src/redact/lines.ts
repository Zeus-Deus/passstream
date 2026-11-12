/**
 * Classical text-line finding for screen content. Screen text is horizontal
 * and high contrast, so a gradient mask, a horizontal smear and connected
 * components find lines in a few milliseconds without a detection network.
 * Boxes are then tightened to their exact ink bounds at full resolution, which
 * makes a line's content hash independent of where it sits: a scrolled line
 * hits the verdict cache instead of being recognised again.
 */

export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** Detection works on 2× downsampled luma. */
const S = 2;
/** Minimum luma step that counts as a glyph edge (0–255). */
const EDGE = 28;
/** Horizontal gap (downsampled px) bridged between glyphs and words. */
const GAP = 6;
/** Text line height bounds in full-resolution pixels. */
export const MIN_LINE_H = 5;
export const MAX_LINE_H = 80;

/**
 * Lines whose components lie in rows [y0, y1) of a W×H luma image. A line
 * cut by the band edge is returned clipped; callers make bands generous.
 *
 * Nothing that may be text is dropped for being too tall: long vertical
 * edges (window borders, dividers) are removed before grouping, and a tall
 * group (lines touching through descenders, text against a border) is split
 * into lines by its row profile. Only bands still taller than a line after
 * that (pictures, video) are treated as non-text.
 */
export function detectLines(luma: Uint8Array, W: number, H: number, y0 = 0, y1 = H): Box[] {
  const w = W >> 1;
  const top = y0 >> 1;
  const h = Math.max(0, Math.min(H >> 1, y1 >> 1) - top);
  if (w < 2 || h < 1) return [];
  // Edge mask.
  const m = new Uint8Array(w * h);
  for (let y = 0; y < h; y++) {
    const fy = (top + y) * S;
    const r = fy * W;
    const below = fy + 1 < H ? W : 0;
    const o = y * w;
    for (let x = 0; x < w - 1; x++) {
      const i = r + x * S;
      const a = luma[i];
      const dx = a - luma[i + 1];
      const dx2 = a - luma[i + 2];
      const dy = a - luma[i + below];
      if ((dx < 0 ? -dx : dx) > EDGE || (dx2 < 0 ? -dx2 : dx2) > EDGE || (dy < 0 ? -dy : dy) > EDGE) m[o + x] = 1;
    }
  }
  // Remove vertical runs longer than any line: borders would weld lines together.
  const vmax = MAX_LINE_H / S;
  for (let x = 0; x < w; x++) {
    let run = 0;
    for (let y = 0; y <= h; y++) {
      if (y < h && m[y * w + x]) run++;
      else {
        if (run > vmax) for (let k = y - run; k < y; k++) m[k * w + x] = 0;
        run = 0;
      }
    }
  }
  // Horizontal smear bridges glyphs and letter gaps.
  const sm = new Uint8Array(w * h);
  for (let y = 0; y < h; y++) {
    const o = y * w;
    let last = -1e9;
    for (let x = 0; x < w; x++) {
      if (!m[o + x]) continue;
      if (x - last <= GAP) sm.fill(1, o + last + 1, o + x + 1);
      else sm[o + x] = 1;
      last = x;
    }
  }
  // Connected components over horizontal runs (union-find).
  const parent: number[] = [];
  const bx0: number[] = [], by0: number[] = [], bx1: number[] = [], by1: number[] = [];
  const find = (a: number) => {
    while (parent[a] !== a) a = parent[a] = parent[parent[a]];
    return a;
  };
  let prev: number[] = []; // [start, end, id] triples
  for (let y = 0; y < h; y++) {
    const o = y * w;
    const cur: number[] = [];
    let x = 0;
    while (x < w) {
      if (!sm[o + x]) {
        x++;
        continue;
      }
      const s = x;
      while (x < w && sm[o + x]) x++;
      const id = parent.length;
      parent.push(id);
      bx0.push(s), by0.push(y), bx1.push(x - 1), by1.push(y);
      cur.push(s, x - 1, id);
      for (let k = 0; k < prev.length; k += 3) {
        if (prev[k] <= x - 1 && prev[k + 1] >= s) {
          const a = find(id), b = find(prev[k + 2]);
          if (a !== b) parent[a] = b;
        }
      }
    }
    prev = cur;
  }
  const roots = new Map<number, number[]>();
  for (let i = 0; i < parent.length; i++) {
    const r = find(i);
    const b = roots.get(r);
    if (!b) roots.set(r, [bx0[i], by0[i], bx1[i], by1[i]]);
    else {
      if (bx0[i] < b[0]) b[0] = bx0[i];
      if (by0[i] < b[1]) b[1] = by0[i];
      if (bx1[i] > b[2]) b[2] = bx1[i];
      if (by1[i] > b[3]) b[3] = by1[i];
    }
  }
  const found: number[][] = []; // half-res [x0, y0, x1, y1]
  const lineMax = MAX_LINE_H / S;
  for (const b of roots.values()) {
    if (b[3] - b[1] + 1 <= lineMax) {
      found.push(b);
      continue;
    }
    // Too tall for one line: split at near-empty rows of the group's box.
    const [x0, ya, x1, yb] = b;
    const thresh = Math.max(1, Math.floor((x1 - x0 + 1) * 0.01));
    let bandStart = -1;
    for (let y = ya; y <= yb + 1; y++) {
      let n = 0;
      if (y <= yb) for (let x = x0, o = y * w; x <= x1; x++) n += sm[o + x];
      if (n > thresh && bandStart < 0) bandStart = y;
      else if (n <= thresh && bandStart >= 0) {
        if (y - bandStart <= lineMax) {
          let lx = x1, rx = x0;
          for (let yy = bandStart; yy < y; yy++)
            for (let x = x0, o = yy * w; x <= x1; x++) if (sm[o + x]) (lx = Math.min(lx, x)), (rx = Math.max(rx, x));
          if (rx >= lx) found.push([lx, bandStart, rx, y - 1]);
        }
        bandStart = -1;
      }
    }
  }
  // Words on one row become one line (a spaced card number is one token).
  found.sort((a, b) => a[1] - b[1] || a[0] - b[0]);
  const merged: number[][] = [];
  for (const b of found) {
    const bh = b[3] - b[1] + 1;
    const into = merged.find((o) => {
      const oh = o[3] - o[1] + 1;
      const overlap = Math.min(o[3], b[3]) - Math.max(o[1], b[1]) + 1;
      const gap = b[0] > o[2] ? b[0] - o[2] : o[0] > b[2] ? o[0] - b[2] : 0;
      return overlap >= 0.6 * Math.min(oh, bh) && Math.abs(oh - bh) <= 0.5 * Math.max(oh, bh) && gap <= Math.max(oh, bh);
    });
    if (into) {
      into[0] = Math.min(into[0], b[0]), into[1] = Math.min(into[1], b[1]);
      into[2] = Math.max(into[2], b[2]), into[3] = Math.max(into[3], b[3]);
    } else merged.push([...b]);
  }
  const out: Box[] = [];
  for (const [x0, ya, x1, yb] of merged) {
    const bh = (yb - ya + 1) * S;
    const bw = (x1 - x0 + 1) * S;
    if (bh < MIN_LINE_H || bh > MAX_LINE_H || bw < 6) continue;
    const x = Math.max(0, x0 * S - S);
    const y = Math.max(0, (top + ya) * S - S);
    out.push({ x, y, w: Math.min(W, x0 * S + bw + S) - x, h: Math.min(H, (top + ya) * S + bh + S) - y });
  }
  return out;
}

/**
 * The exact ink bounds of a box at full resolution: pixels that differ from
 * the box's background (its border's most common luma). Null if blank.
 */
export function tighten(luma: Uint8Array, W: number, b: Box): Box | null {
  const hist = new Uint32Array(64);
  const sample = (x: number, y: number) => hist[luma[y * W + x] >> 2]++;
  for (let x = b.x; x < b.x + b.w; x++) sample(x, b.y), sample(x, b.y + b.h - 1);
  for (let y = b.y; y < b.y + b.h; y++) sample(b.x, y), sample(b.x + b.w - 1, y);
  let bg = 0;
  for (let i = 1; i < 64; i++) if (hist[i] > hist[bg]) bg = i;
  const bgl = bg * 4 + 2;
  let x0 = Infinity, y0 = Infinity, x1 = -1, y1 = -1;
  for (let y = b.y; y < b.y + b.h; y++) {
    const r = y * W;
    for (let x = b.x; x < b.x + b.w; x++) {
      const d = luma[r + x] - bgl;
      if (d > 40 || d < -40) {
        if (x < x0) x0 = x;
        if (x > x1) x1 = x;
        if (y < y0) y0 = y;
        if (y > y1) y1 = y;
      }
    }
  }
  if (x1 < 0) return null;
  return { x: x0, y: y0, w: x1 - x0 + 1, h: y1 - y0 + 1 };
}

/**
 * A 64-bit content key for a box's pixels (two independent 32-bit hashes
 * plus its size). Identical text at another position gives the same key.
 */
export function contentKey(luma: Uint8Array, W: number, b: Box): string {
  let h1 = 0x811c9dc5 ^ b.w;
  let h2 = 0x9e3779b9 ^ (b.h << 16);
  for (let y = b.y; y < b.y + b.h; y++) {
    const r = y * W;
    for (let x = b.x; x < b.x + b.w; x++) {
      const v = luma[r + x];
      h1 = Math.imul(h1 ^ v, 0x01000193);
      h2 = Math.imul(h2 ^ v, 0x85ebca6b);
      h2 ^= h2 >>> 13;
    }
  }
  return `${b.w}x${b.h}:${(h1 >>> 0).toString(36)}${(h2 >>> 0).toString(36)}`;
}

/** Copy a box (with `pad` pixels of context, clamped) out of a luma image. */
export function crop(luma: Uint8Array, W: number, H: number, b: Box, pad: number): { data: Uint8Array; box: Box } {
  const x = Math.max(0, b.x - pad), y = Math.max(0, b.y - pad);
  const w = Math.min(W, b.x + b.w + pad) - x, h = Math.min(H, b.y + b.h + pad) - y;
  const data = new Uint8Array(w * h);
  for (let r = 0; r < h; r++) data.set(luma.subarray((y + r) * W + x, (y + r) * W + x + w), r * w);
  return { data, box: { x, y, w, h } };
}
