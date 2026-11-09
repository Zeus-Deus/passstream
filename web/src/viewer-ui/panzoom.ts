// Pinch-zoom and pan math for the player. The view is a CSS transform
// `translate(x px, y px) scale(s)` with transform-origin 0 0 on a layer the
// size of the viewport, so a layer point p appears at x + s·p on screen.
// The bounds are the rendered picture inside that layer (letterbox excluded),
// not the layer: zoom never shows more bars than fit does, and pan never
// moves the picture out of view.
export interface View {
  scale: number;
  x: number;
  y: number;
}
export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}
/** The stage (layer) size, and where the picture is drawn in it (default: all of it). */
export interface Size {
  w: number;
  h: number;
  picture?: Rect;
}
export interface Point {
  x: number;
  y: number;
}

export const FIT: View = { scale: 1, x: 0, y: 0 };
export const MIN_SCALE = 1;
export const MAX_SCALE = 8;
export const DOUBLE_TAP_SCALE = 2.5;

/**
 * Where object-fit draws a `video`-sized picture in a `stage`-sized box:
 * contain (fit) letterboxes, cover (fill) fills the box and crops.
 */
export function pictureRect(stage: { w: number; h: number }, video: { w: number; h: number } | null, fill: boolean): Rect {
  if (fill || !video || video.w <= 0 || video.h <= 0) return { x: 0, y: 0, w: stage.w, h: stage.h };
  const k = Math.min(stage.w / video.w, stage.h / video.h);
  const w = video.w * k,
    h = video.h * k;
  return { x: (stage.w - w) / 2, y: (stage.h - h) / 2, w, h };
}

/** One axis: a picture wider than the stage covers it (no gap); a narrower one is centred. */
function axis(t: number, scale: number, stage: number, start: number, len: number) {
  const shown = scale * len;
  if (shown <= stage) return (stage - shown) / 2 - scale * start;
  return Math.min(-scale * start, Math.max(stage - scale * (start + len), t));
}

/** Keep the zoomed picture in view: no panning past its edges, no new gaps. */
export function clamp(v: View, size: Size): View {
  const scale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, v.scale));
  const p = size.picture ?? { x: 0, y: 0, w: size.w, h: size.h };
  return { scale, x: axis(v.x, scale, size.w, p.x, p.w) + 0, y: axis(v.y, scale, size.h, p.y, p.h) + 0 };
}

/** Scale to `scale`, keeping the screen point `at` over the same content. */
export function zoomTo(v: View, scale: number, at: Point, size: Size): View {
  const s = Math.min(MAX_SCALE, Math.max(MIN_SCALE, scale));
  const lx = (at.x - v.x) / v.scale,
    ly = (at.y - v.y) / v.scale;
  return clamp({ scale: s, x: at.x - s * lx, y: at.y - s * ly }, size);
}

export function panBy(v: View, dx: number, dy: number, size: Size): View {
  return clamp({ ...v, x: v.x + dx, y: v.y + dy }, size);
}

/** Double tap: fit if zoomed, otherwise zoom in on the tapped point. */
export function toggleZoom(v: View, at: Point, size: Size): View {
  return v.scale > 1.01 ? FIT : zoomTo(FIT, DOUBLE_TAP_SCALE, at, size);
}

export function isFit(v: View) {
  return v.scale <= 1.01;
}

export function distance(a: Point, b: Point) {
  return Math.hypot(a.x - b.x, a.y - b.y);
}
export function midpoint(a: Point, b: Point): Point {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
}
