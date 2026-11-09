import { useEffect, useRef } from "react";
import { ANNOTATION_HOST } from "@passstream/e2ee";
import { ROW_COLOUR_HEX, rowCodeColour } from "../viewer-ui/rowcode";
import { frameRect } from "./geometry";
import { AnnotationStore, LASER_FADE_MS } from "./store";

export function inkColour(origin: string): string {
  if (origin === ANNOTATION_HOST) return "#ffffff";
  const c = rowCodeColour(origin);
  return c ? ROW_COLOUR_HEX[c] : "#cccccc";
}

/**
 * The shared annotation overlay: one canvas over the video, inside the zoom
 * layer so it zooms with the picture. It redraws when the store changes and
 * runs an animation frame loop only while a laser trail is fading.
 */
export function InkOverlay({ store, video, fill = false }: { store: AnnotationStore; video: () => { w: number; h: number } | null; fill?: boolean }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const videoRef = useRef(video);
  videoRef.current = video;

  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    let raf = 0;
    const draw = () => {
      raf = 0;
      const parent = el.parentElement;
      const w = parent?.clientWidth || 1,
        h = parent?.clientHeight || 1;
      const dpr = Math.min(3, window.devicePixelRatio || 1);
      if (el.width !== Math.round(w * dpr) || el.height !== Math.round(h * dpr)) {
        el.width = Math.round(w * dpr);
        el.height = Math.round(h * dpr);
      }
      const ctx = el.getContext("2d");
      if (!ctx) return;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);
      const r = frameRect({ w, h }, videoRef.current(), fill);
      const X = (x: number) => r.x + x * r.w,
        Y = (y: number) => r.y + y * r.h;
      const now = performance.now();
      const animating = store.prune(now);
      ctx.lineCap = "round";
      ctx.lineJoin = "round";
      for (const [origin, ink] of store.origins) {
        const colour = inkColour(origin);
        let labelAt: { x: number; y: number } | null = null;
        for (const s of ink.strokes.values()) {
          if (s.points.length === 0) continue;
          for (const [width, style] of [[5, "rgba(0,0,0,0.45)"], [3, colour]] as const) {
            ctx.beginPath();
            ctx.lineWidth = width;
            ctx.strokeStyle = style;
            s.points.forEach((p, i) => (i ? ctx.lineTo(X(p.x), Y(p.y)) : ctx.moveTo(X(p.x), Y(p.y))));
            if (s.points.length === 1) ctx.lineTo(X(s.points[0]!.x) + 0.1, Y(s.points[0]!.y));
            ctx.stroke();
          }
          const last = s.points.at(-1)!;
          labelAt = { x: X(last.x), y: Y(last.y) };
        }
        const shown = ink.trail.filter((p) => p.at <= now);
        if (shown.length) {
          for (let i = 1; i < shown.length; i++) {
            const a = shown[i - 1]!,
              b = shown[i]!;
            ctx.globalAlpha = Math.max(0, 1 - (now - b.at) / LASER_FADE_MS) * 0.7;
            ctx.beginPath();
            ctx.lineWidth = 4;
            ctx.strokeStyle = colour;
            ctx.moveTo(X(a.x), Y(a.y));
            ctx.lineTo(X(b.x), Y(b.y));
            ctx.stroke();
          }
          const head = shown.at(-1)!;
          const alpha = Math.max(0, 1 - (now - head.at) / LASER_FADE_MS);
          ctx.globalAlpha = alpha;
          ctx.beginPath();
          ctx.arc(X(head.x), Y(head.y), 7, 0, Math.PI * 2);
          ctx.fillStyle = colour;
          ctx.fill();
          // The host's white dot needs a heavier ring to show on light screens.
          ctx.lineWidth = origin === ANNOTATION_HOST ? 3 : 2;
          ctx.strokeStyle = origin === ANNOTATION_HOST ? "rgba(0,0,0,0.85)" : "rgba(0,0,0,0.6)";
          ctx.stroke();
          labelAt = { x: X(head.x), y: Y(head.y) };
          ctx.globalAlpha = alpha;
        }
        if (labelAt) {
          // The drawer's row code (or "Host") in its colour, next to the newest mark.
          const text = origin === ANNOTATION_HOST ? "Host" : origin;
          ctx.font = "600 12px ui-monospace, monospace";
          const tw = ctx.measureText(text).width;
          const lx = Math.min(w - tw - 12, labelAt.x + 10),
            ly = Math.max(4, labelAt.y - 24);
          ctx.fillStyle = "rgba(10,10,10,0.75)";
          ctx.fillRect(lx, ly, tw + 10, 18);
          ctx.fillStyle = colour;
          ctx.fillText(text, lx + 5, ly + 13);
        }
        ctx.globalAlpha = 1;
      }
      if (animating) raf = requestAnimationFrame(draw);
    };
    const schedule = () => {
      if (!raf) raf = requestAnimationFrame(draw);
    };
    const off = store.subscribe(schedule);
    const ro = typeof ResizeObserver === "function" && el.parentElement ? new ResizeObserver(schedule) : null;
    if (el.parentElement) ro?.observe(el.parentElement);
    schedule();
    return () => {
      off();
      ro?.disconnect();
      if (raf) cancelAnimationFrame(raf);
    };
  }, [store, fill]);

  return <canvas ref={canvas} className="pointer-events-none absolute inset-0 h-full w-full" data-testid="ink-overlay" aria-hidden="true" />;
}
