/**
 * Where the video frame sits in its stage (untransformed, before zoom):
 * contained (letterboxed) or covering (cropped). Annotation coordinates are
 * normalised to the frame, so every viewer maps them the same way.
 */
export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}
export function frameRect(stage: { w: number; h: number }, video: { w: number; h: number } | null, fill: boolean): Rect {
  if (!video || video.w <= 0 || video.h <= 0) return { x: 0, y: 0, w: stage.w, h: stage.h };
  const k = (fill ? Math.max : Math.min)(stage.w / video.w, stage.h / video.h);
  const w = video.w * k,
    h = video.h * k;
  return { x: (stage.w - w) / 2, y: (stage.h - h) / 2, w, h };
}
/** A stage point (zoom already undone) to frame coordinates; null outside the frame. */
export function toFrame(p: { x: number; y: number }, r: Rect): { x: number; y: number } | null {
  const x = (p.x - r.x) / r.w,
    y = (p.y - r.y) / r.h;
  if (!(x >= -0.02 && x <= 1.02 && y >= -0.02 && y <= 1.02)) return null;
  return { x: Math.min(1, Math.max(0, x)), y: Math.min(1, Math.max(0, y)) };
}
