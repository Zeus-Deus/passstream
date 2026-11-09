import type { SVGProps } from "react";
import { ROW_COLOUR_HEX, rowCodeColour } from "./rowcode";

type P = SVGProps<SVGSVGElement>;
const line = {
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.7,
  strokeLinecap: "round",
  strokeLinejoin: "round",
} as const;

export function Mark(p: P) {
  return (
    <svg viewBox="0 0 64 64" aria-hidden="true" {...p}>
      <path fill="#c1422b" d="M10 4h44a6 6 0 0 0 6 6v44a6 6 0 0 0-6 6H10a6 6 0 0 0-6-6V10a6 6 0 0 0 6-6z" />
      <rect x="15" y="18" width="34" height="23" rx="2" fill="#f4eee3" />
      <path fill="#dcd2bb" d="M26 46h12v3H26z" />
      <circle cx="23" cy="25" r="3" fill="#c1422b" />
      <path fill="#c1422b" d="M29 31h14v2.5H29zm-8 5h22v2.5H21z" />
    </svg>
  );
}

export function Brand({ night = false }: { night?: boolean }) {
  return (
    <span className="inline-flex items-center gap-2">
      <Mark className="size-6" />
      <span className={`display text-[1.15rem] font-semibold ${night ? "text-paper" : ""}`}>PassStream</span>
    </span>
  );
}

/** The viewer's own row code, big, with its colour name. */
export function CodeBadge({ code, night = true }: { code: string; night?: boolean }) {
  const colour = rowCodeColour(code);
  return (
    <span
      data-testid="own-code"
      className={`inline-flex items-center gap-3 rounded-full border px-5 py-2.5 font-mono text-[1.6rem] font-medium tracking-[0.12em] ${night ? "border-white/15 bg-white/5 text-paper" : "border-paper-dark bg-paper text-ink"}`}
    >
      <span aria-hidden="true" className="size-4 rounded-full" style={{ background: colour ? ROW_COLOUR_HEX[colour] : "transparent" }} />
      {code}
      {colour && <span className="text-[0.95rem] tracking-normal opacity-70">· {colour}</span>}
    </span>
  );
}

export const IconFullscreen = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5" />
  </svg>
);
export const IconExitFullscreen = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5" />
  </svg>
);
export const IconTheatre = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <rect x="2.5" y="6" width="19" height="12" rx="1.5" />
  </svg>
);
export const IconPip = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <rect x="3" y="5" width="18" height="14" rx="1.5" />
    <rect x="12" y="12" width="7" height="5" rx="0.8" fill="currentColor" stroke="none" />
  </svg>
);
export const IconSound = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M4 9.5h3.5L12 6v12l-4.5-3.5H4z" />
    <path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11" />
  </svg>
);
export const IconMuted = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M4 9.5h3.5L12 6v12l-4.5-3.5H4z" />
    <path d="m16 10 4 4m0-4-4 4" />
  </svg>
);
export const IconZoom = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <circle cx="10.5" cy="10.5" r="6" />
    <path d="m15 15 5 5M8 10.5h5M10.5 8v5" />
  </svg>
);
export const IconFit = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <circle cx="10.5" cy="10.5" r="6" />
    <path d="m15 15 5 5M8 10.5h5" />
  </svg>
);
export const IconClose = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="m6 6 12 12M18 6 6 18" />
  </svg>
);
export const IconInfo = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 11v5M12 8h.01" />
  </svg>
);
/** Fill: the picture grows to cover the screen. */
export const IconExpand = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <rect x="3" y="6" width="18" height="12" rx="1.5" />
    <path d="M8 12H5.5m0 0 1.5-1.5M5.5 12 7 13.5M16 12h2.5m0 0L17 10.5m1.5 1.5L17 13.5" />
  </svg>
);
/** Fit: the whole picture, letterboxed. */
export const IconShrink = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <rect x="3" y="6" width="18" height="12" rx="1.5" />
    <path d="M4.5 12H8m0 0L6.5 10.5M8 12l-1.5 1.5M19.5 12H16m0 0 1.5-1.5M16 12l1.5 1.5" />
  </svg>
);
/** Draw on the shared overlay (annotations). */
export const IconPen = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M4 20l4-1 11-11-3-3L5 16l-1 4zM14 6l3 3" />
  </svg>
);
/** The laser pointer tool. */
export const IconLaser = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <circle cx="12" cy="12" r="3" />
    <path d="M12 3v3M12 18v3M3 12h3M18 12h3" />
  </svg>
);
/** Clear (an eraser). */
export const IconEraser = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...line} {...p}>
    <path d="M8 20h12M5 15l8-8 5 5-6 6H8l-3-3z" />
  </svg>
);
