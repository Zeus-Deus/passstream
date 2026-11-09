import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type FocusEvent as ReactFocusEvent,
  type PointerEvent as RPointerEvent,
  type ReactNode,
  type WheelEvent,
} from "react";
import { clamp, distance, FIT, isFit, midpoint, panBy, pictureRect, toggleZoom, zoomTo, type Point, type Size, type View } from "./panzoom";
import {
  Brand,
  IconClose,
  IconExitFullscreen,
  IconFit,
  IconFullscreen,
  IconInfo,
  IconMuted,
  IconPip,
  IconSound,
  IconTheatre,
  IconZoom,
  IconExpand,
  IconShrink,
} from "./parts";
import { ROW_COLOUR_HEX, rowCodeColour } from "./rowcode";
import { frameRect, toFrame } from "../annotations/geometry";

export interface Quality {
  rtt_ms: number | null;
  loss_pct: number | null;
  bitrate_kbps: number | null;
  fps: number | null;
  path: string | null;
  layer: string | null;
  freezes: number;
  /** Gaps over 500 ms between rendered frames, outside pauses (freezes are the ones with evidence of input). */
  render_gaps?: number;
  /** HUD details (FEATURES.md "Track D2" §1); optional so older callers still fit. */
  rendition?: "native" | "low" | null;
  temporal?: string | null;
  g2g_ms?: number | null;
  e2ee?: { epoch: number | null; failures: number; top: string | null } | null;
}

/** Explicit draw mode: one finger draws (frame-normalised points); a second finger pinches as usual. */
export interface DrawControl {
  active: boolean;
  begin(p: Point): void;
  move(p: Point): void;
  end(): void;
}

/** 0–4 bars from RTT, loss and frame rate; -1 without data. */
export function qualityBars(q: Quality | null | undefined): number {
  if (!q || (q.rtt_ms === null && q.loss_pct === null)) return -1;
  const rtt = q.rtt_ms ?? 0,
    loss = q.loss_pct ?? 0;
  let bars = rtt <= 80 && loss <= 1 ? 4 : rtt <= 150 && loss <= 3 ? 3 : rtt <= 300 && loss <= 8 ? 2 : rtt <= 1000 && loss <= 25 ? 1 : 0;
  if (q.fps !== null && q.fps < 4) bars = Math.min(bars, 2);
  return bars;
}
export function isDegraded(q: Quality | null | undefined) {
  const b = qualityBars(q);
  return b >= 0 && b < 3;
}

export type PlayerState = "live" | "paused" | "reconnecting";

/** Why a reconnecting player is waiting, and whether it has been a while. */
export interface Reconnect {
  cause: "network" | "host";
  long: boolean;
}

export function reconnectCopy(r: Reconnect | null | undefined): [string, string] {
  if (r?.cause === "host")
    return r.long
      ? ["Still waiting for the host", "Their computer is reconnecting. The picture comes back here; you don't need to reload."]
      : ["Waiting for the host", "Their connection dropped for a moment. Holding the last frame."];
  return r?.long
    ? ["Still reconnecting", "Your connection or the host's is slow to come back. We keep trying; you don't need to reload."]
    : ["Reconnecting…", "Holding the last frame. This usually takes a second."];
}

export interface PlayerProps {
  stream?: MediaStream | null;
  state: PlayerState;
  /** Only for "reconnecting": the cause and whether it has been long. */
  reconnect?: Reconnect | null;
  hasAudio?: boolean;
  quality?: Quality | null;
  rowCode?: string | null;
  title?: string | null;
  /** Hide the controls this long after the last interaction; 0 keeps them. */
  autoHideMs?: number;
  initialHud?: boolean;
  onVideo?: (v: HTMLVideoElement | null) => void;
  /** Drawn inside the zoom layer, over the video (the annotation overlay); gets the fit/fill mode. */
  overlay?: ((fill: boolean) => ReactNode) | null;
  /** Draw mode, when the host allows drawing. */
  draw?: DrawControl | null;
  /** A control for the top-right corner (the draw toggle). */
  topAction?: ReactNode;
  /** Replaces the bottom controls while drawing (the draw toolbar). */
  drawTools?: ReactNode;
}

function useMedia(query: string, fallback: boolean) {
  const get = () => (typeof window.matchMedia === "function" ? window.matchMedia(query).matches : fallback);
  const [m, setM] = useState(get);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(query);
    const on = () => setM(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, [query]);
  return m;
}

type WakeLockNavigator = Navigator & { wakeLock?: { request(type: "screen"): Promise<{ release(): Promise<void>; addEventListener(t: "release", f: () => void): void }> } };

/**
 * Keep the screen on while watching. Browsers drop the lock when the page is
 * hidden; it's re-requested on the next tap after the page comes back,
 * because iOS only grants it from a user gesture.
 */
export function useWakeLock(active: boolean, nav: WakeLockNavigator = navigator as WakeLockNavigator) {
  const lock = useRef<{ release(): Promise<void> } | null>(null);
  const pending = useRef(false);
  const wanted = useRef(false);
  const request = useCallback(async () => {
    if (!nav.wakeLock || lock.current || pending.current || document.visibilityState === "hidden") return;
    pending.current = true;
    try {
      const l = await nav.wakeLock.request("screen");
      // Resolved after the player stopped wanting it (paused, unmounted).
      if (!wanted.current) {
        void l.release().catch(() => undefined);
        return;
      }
      lock.current = l;
      l.addEventListener("release", () => {
        if (lock.current === l) lock.current = null;
      });
    } catch {
      /* not allowed now (no gesture, low battery); try again on the next tap */
    } finally {
      pending.current = false;
    }
  }, [nav]);
  useEffect(() => {
    wanted.current = active;
    if (active) void request();
    const onVis = () => {
      if (document.visibilityState === "hidden") lock.current = null;
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      document.removeEventListener("visibilitychange", onVis);
      // Runs on unmount and before every change of `active`.
      wanted.current = false;
      void lock.current?.release().catch(() => undefined);
      lock.current = null;
    };
  }, [active, request]);
  // Call from tap handlers.
  return useCallback(() => {
    if (wanted.current && !lock.current) void request();
  }, [request]);
}

type FsVideo = HTMLVideoElement & {
  webkitEnterFullscreen?: () => void;
  webkitSupportsPresentationMode?: (m: string) => boolean;
  webkitSetPresentationMode?: (m: string) => void;
};

const stop = (e: { stopPropagation(): void }) => e.stopPropagation();

function Overlay({ className = "", style, children }: { className?: string; style?: CSSProperties; children: ReactNode }) {
  // Controls swallow pointer events so they never pan, zoom or toggle the chrome.
  return (
    <div className={`pointer-events-auto ${className}`} style={style} onPointerDown={stop} onPointerUp={stop} onDoubleClick={stop}>
      {children}
    </div>
  );
}

export function Player({ stream, state, reconnect = null, hasAudio = false, quality = null, rowCode = null, title = null, autoHideMs = 4000, initialHud = false, onVideo, overlay = null, draw = null, topAction = null, drawTools = null }: PlayerProps) {
  const desktop = useMedia("(min-width: 768px) and (pointer: fine)", false);
  const short = useMedia("(max-height: 480px)", false);
  const [theatre, setTheatre] = useState(false);
  const immersive = !desktop || theatre;
  const root = useRef<HTMLDivElement>(null);
  const stage = useRef<HTMLDivElement>(null);
  const video = useRef<FsVideo | null>(null);
  const [view, setView] = useState<View>(FIT);
  // Fit shows the whole screen (letterboxed); fill crops to cover the viewport.
  const [fill, setFill] = useState(false);
  const [gesturing, setGesturing] = useState(false);
  const [muted, setMuted] = useState(true);
  const [chrome, setChrome] = useState(true);
  const [hud, setHud] = useState(initialHud);
  const [hudMore, setHudMore] = useState(false);
  const drawing = !!draw?.active && state !== "paused";
  const [fullscreen, setFullscreen] = useState(false);
  const [pip, setPip] = useState(false);
  const [fsSupported, setFsSupported] = useState(false);
  const wake = useWakeLock(state === "live");
  const degraded = isDegraded(quality) || state === "reconnecting";

  const setVideo = useCallback(
    (v: HTMLVideoElement | null) => {
      video.current = v as FsVideo | null;
      onVideo?.(v);
      if (v) {
        const fv = v as FsVideo;
        setPip(!!(document.pictureInPictureEnabled && !v.disablePictureInPicture) || !!fv.webkitSupportsPresentationMode?.("picture-in-picture"));
        setFsSupported(!!document.documentElement.requestFullscreen || typeof fv.webkitEnterFullscreen === "function");
      }
    },
    [onVideo],
  );

  useEffect(() => {
    if (video.current && stream !== undefined && video.current.srcObject !== stream) {
      video.current.srcObject = stream;
      void video.current.play?.()?.catch(() => undefined);
    }
  }, [stream]);

  useEffect(() => {
    const on = () => setFullscreen(!!document.fullscreenElement);
    document.addEventListener("fullscreenchange", on);
    return () => document.removeEventListener("fullscreenchange", on);
  }, []);

  // Controls hide after a while on a live picture; any tap brings them back.
  // Keyboard focus inside the controls shows them and holds the timer, so a
  // focused button is never invisible.
  const hideTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const focusInside = useRef(false);
  const showChrome = useCallback(() => {
    setChrome(true);
    if (hideTimer.current) clearTimeout(hideTimer.current);
    if (autoHideMs > 0 && !focusInside.current) hideTimer.current = setTimeout(() => setChrome(false), autoHideMs);
  }, [autoHideMs]);
  const onControlsFocus = () => {
    focusInside.current = true;
    showChrome();
  };
  const onControlsBlur = (e: ReactFocusEvent<HTMLDivElement>) => {
    if (e.relatedTarget instanceof Node && e.currentTarget.contains(e.relatedTarget)) return;
    focusInside.current = false;
    if (state === "live" && !hud) showChrome();
  };
  useEffect(() => {
    if (state === "live" && !hud && !drawing) showChrome();
    else {
      if (hideTimer.current) clearTimeout(hideTimer.current);
      setChrome(true);
    }
    return () => {
      if (hideTimer.current) clearTimeout(hideTimer.current);
    };
  }, [state, hud, drawing, showChrome]);

  // The stage, and the picture's rect in it: the zoom bounds.
  const fillRef = useRef(fill);
  fillRef.current = fill;
  const size = (): Size => {
    const r = stage.current?.getBoundingClientRect();
    const st = { w: r?.width || 1, h: r?.height || 1 };
    const v = video.current;
    return { ...st, picture: pictureRect(st, v ? { w: v.videoWidth, h: v.videoHeight } : null, fillRef.current) };
  };
  // Re-clamp when the bounds change: resize and rotation (the stage), a new
  // picture size (the video), and fit/fill. The zoom stays; gaps don't.
  useEffect(() => {
    const reclamp = () => setView((v) => (isFit(v) ? FIT : clamp(v, size())));
    reclamp();
    const el = stage.current;
    const v = video.current;
    const ro = typeof ResizeObserver === "function" && el ? new ResizeObserver(reclamp) : null;
    if (el) ro?.observe(el);
    window.addEventListener("orientationchange", reclamp);
    v?.addEventListener("resize", reclamp);
    v?.addEventListener("loadedmetadata", reclamp);
    return () => {
      ro?.disconnect();
      window.removeEventListener("orientationchange", reclamp);
      v?.removeEventListener("resize", reclamp);
      v?.removeEventListener("loadedmetadata", reclamp);
    };
    // size() reads the stage and video through refs; fill is the one bound held in state.
  }, [fill]);
  const local = (e: { clientX: number; clientY: number }): Point => {
    const r = stage.current?.getBoundingClientRect();
    return { x: e.clientX - (r?.left ?? 0), y: e.clientY - (r?.top ?? 0) };
  };

  const unmute = () => {
    const v = video.current;
    if (!v) return;
    v.muted = false;
    setMuted(false);
    void v.play?.()?.catch(() => undefined);
  };
  const toggleMute = () => {
    const v = video.current;
    if (!v || !hasAudio) return;
    v.muted = !v.muted;
    setMuted(v.muted);
    if (!v.muted) void v.play?.()?.catch(() => undefined);
  };
  const toggleFullscreen = () => {
    if (document.fullscreenElement) {
      void document.exitFullscreen?.();
      return;
    }
    const el = root.current;
    if (el?.requestFullscreen) void el.requestFullscreen({ navigationUI: "hide" }).catch(() => video.current?.webkitEnterFullscreen?.());
    // iPhone: only the video element can go fullscreen; overlays hide there.
    else video.current?.webkitEnterFullscreen?.();
  };
  const togglePip = async () => {
    const v = video.current;
    if (!v) return;
    try {
      if (document.pictureInPictureElement) await document.exitPictureInPicture();
      else if (v.requestPictureInPicture) await v.requestPictureInPicture();
      else v.webkitSetPresentationMode?.("picture-in-picture");
    } catch {
      /* PiP refused (no frame yet, policy); nothing to undo */
    }
  };
  const zoomKey = () => {
    const s = size();
    setView((v) => toggleZoom(v, { x: s.w / 2, y: s.h / 2 }, s));
  };

  // Desktop keys: f fullscreen, m mute, z zoom.
  const keys = useRef({ toggleFullscreen, toggleMute, zoomKey });
  keys.current = { toggleFullscreen, toggleMute, zoomKey };
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
      const k = e.key.toLowerCase();
      if (k === "f") keys.current.toggleFullscreen();
      else if (k === "m") keys.current.toggleMute();
      else if (k === "z") keys.current.zoomKey();
      else if (k === "escape") {
        setHud(false);
        setTheatre(false);
      } else return;
      showChrome();
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [showChrome]);

  // Pointer gestures: one finger pans (when zoomed), two pinch, tap toggles
  // the controls, double tap fits or zooms in.
  const pointers = useRef(new Map<number, Point>());
  const pinch = useRef<{ dist: number; mid: Point; view: View } | null>(null);
  const tap = useRef<{ start: Point; t: number; moved: boolean } | null>(null);
  const lastTap = useRef<{ at: Point; t: number } | null>(null);
  const tapTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Draw mode: a stage point → frame coordinates (zoom undone, letterbox/crop applied).
  const stroke = useRef(false);
  const framePoint = (p: Point) => {
    const s = size();
    const v = video.current;
    const r = frameRect({ w: s.w, h: s.h }, v && v.videoWidth ? { w: v.videoWidth, h: v.videoHeight } : null, fillRef.current);
    return toFrame({ x: (p.x - view.x) / view.scale, y: (p.y - view.y) / view.scale }, r);
  };
  const onDown = (e: RPointerEvent<HTMLDivElement>) => {
    wake();
    const p = local(e);
    stage.current?.setPointerCapture?.(e.pointerId);
    pointers.current.set(e.pointerId, p);
    if (drawing && draw && pointers.current.size === 1) {
      const f = framePoint(p);
      tap.current = null;
      if (f) {
        stroke.current = true;
        draw.begin(f);
      }
      return;
    }
    if (stroke.current && pointers.current.size === 2) {
      // A second finger: the stroke ends and this becomes a pinch.
      stroke.current = false;
      draw?.end();
    }
    if (pointers.current.size === 1) tap.current = { start: p, t: performance.now(), moved: false };
    if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()];
      pinch.current = { dist: distance(a, b) || 1, mid: midpoint(a, b), view };
      tap.current = null;
    }
    setGesturing(true);
  };
  const onMove = (e: RPointerEvent<HTMLDivElement>) => {
    const prev = pointers.current.get(e.pointerId);
    if (!prev) return;
    const p = local(e);
    pointers.current.set(e.pointerId, p);
    if (stroke.current) {
      const f = framePoint(p);
      if (f) draw?.move(f);
      return;
    }
    if (tap.current && distance(p, tap.current.start) > 10) tap.current.moved = true;
    const s = size();
    if (pointers.current.size >= 2 && pinch.current) {
      const [a, b] = [...pointers.current.values()];
      const start = pinch.current;
      const mid = midpoint(a, b);
      const zoomed = zoomTo(start.view, start.view.scale * (distance(a, b) / start.dist), start.mid, s);
      setView(panBy(zoomed, mid.x - start.mid.x, mid.y - start.mid.y, s));
    } else if (pointers.current.size === 1 && tap.current?.moved) {
      setView((v) => (isFit(v) ? v : panBy(v, p.x - prev.x, p.y - prev.y, s)));
    }
  };
  const onUp = (e: RPointerEvent<HTMLDivElement>) => {
    const p = pointers.current.get(e.pointerId) ?? local(e);
    pointers.current.delete(e.pointerId);
    if (stroke.current) {
      stroke.current = false;
      draw?.end();
      if (pointers.current.size === 0) setGesturing(false);
      tap.current = null;
      return;
    }
    if (pointers.current.size < 2) pinch.current = null;
    if (pointers.current.size === 0) setGesturing(false);
    if (pointers.current.size === 1) {
      // Lifting one finger of a pinch: the other continues as a pan from where
      // it is now, and never counts as a tap.
      const [rest] = pointers.current.values();
      tap.current = { start: rest, t: 0, moved: true };
      return;
    }
    const t = tap.current;
    tap.current = null;
    if (!t || t.moved || performance.now() - t.t > 350) return;
    const now = performance.now();
    if (lastTap.current && now - lastTap.current.t < 320 && distance(lastTap.current.at, p) < 40) {
      lastTap.current = null;
      if (tapTimer.current) clearTimeout(tapTimer.current);
      setView((v) => toggleZoom(v, p, size()));
      return;
    }
    lastTap.current = { at: p, t: now };
    if (tapTimer.current) clearTimeout(tapTimer.current);
    tapTimer.current = setTimeout(() => {
      setChrome((c) => {
        if (!c || focusInside.current) {
          showChrome();
          return true;
        }
        return false;
      });
    }, 260);
  };
  const onWheel = (e: WheelEvent<HTMLDivElement>) => {
    const p = local(e);
    const s = size();
    setView((v) => zoomTo(v, v.scale * Math.exp(-e.deltaY * 0.0015), p, s));
  };
  useEffect(() => () => {
    if (tapTimer.current) clearTimeout(tapTimer.current);
  }, []);

  const safe = {
    top: "max(0.75rem, env(safe-area-inset-top))",
    bottom: "max(0.75rem, env(safe-area-inset-bottom))",
    left: "max(0.75rem, env(safe-area-inset-left))",
    right: "max(0.75rem, env(safe-area-inset-right))",
  };
  const bars = qualityBars(quality);
  const fade = `transition-opacity duration-200 ${chrome ? "opacity-100" : "pointer-events-none opacity-0"}`;

  return (
    <div
      ref={root}
      data-testid="player"
      data-layout={immersive ? "immersive" : "framed"}
      className={immersive ? "fixed inset-0 h-[100dvh] w-full overflow-hidden bg-night text-paper" : "flex h-dvh flex-col bg-night text-paper"}
    >
      {!immersive && (
        <header className="flex h-14 shrink-0 items-center gap-4 border-b border-night-edge px-5">
          <Brand night />
          {title && <span className="truncate text-[0.92rem] text-paper/70">{title}</span>}
          <span className="ml-auto flex items-center gap-2">
            <span className="hidden font-mono text-[0.68rem] tracking-wide text-paper/60 lg:inline">f fullscreen · m sound · z zoom</span>
            <button type="button" className="glass-btn" onClick={() => setTheatre(true)} aria-label="Theatre mode">
              <IconTheatre className="size-4" /> Theatre
            </button>
          </span>
        </header>
      )}
      <div
        ref={stage}
        className={`relative touch-none overflow-hidden select-none ${immersive ? "h-full w-full" : "m-4 flex-1 rounded-md border border-night-edge"}`}
        onPointerDown={onDown}
        onPointerMove={onMove}
        onPointerUp={onUp}
        onPointerCancel={onUp}
        onWheel={onWheel}
        data-testid="stage"
        data-drawing={drawing ? "1" : "0"}
        aria-label={drawing ? "Shared screen. Draw with one finger; pinch with two to zoom." : "Shared screen. Pinch or double-tap to zoom."}
        role="region"
      >
        <div
          className="absolute inset-0 origin-top-left will-change-transform"
          style={{
            transform: `translate3d(${view.x}px, ${view.y}px, 0) scale(${view.scale})`,
            transition: gesturing ? "none" : "transform 180ms ease-out",
          }}
          data-testid="zoom-layer"
          data-scale={view.scale.toFixed(2)}
          data-fit={fill ? "fill" : "fit"}
        >
          <video
            ref={setVideo}
            className={`h-full w-full transition-[filter] ${fill ? "object-cover" : "object-contain"} ${state === "reconnecting" ? "brightness-75 grayscale-[0.4]" : ""}`}
            playsInline
            muted
            autoPlay
            disablePictureInPicture={false}
            data-testid="video"
          />
          {state !== "paused" && overlay?.(fill)}
        </div>

        {/* Paused: an opaque cover once the first "Paused" picture decodes, so no earlier frame (or the
            sender's marker picture) shows. The video keeps decoding beneath
            it, which is how resuming is noticed. */}
        {state === "paused" && (
          <div className="absolute inset-0 grid place-items-center bg-night px-6 text-center" data-testid="paused-cover" role="status">
            <div className="max-w-xs">
              <span aria-hidden="true" className="mx-auto mb-5 flex w-fit gap-1.5"><span className="h-7 w-2.5 rounded-sm bg-warn-tint" /><span className="h-7 w-2.5 rounded-sm bg-warn-tint" /></span>
              <p className="display text-[1.7rem] leading-tight">Paused by the host</p>
              <p className="mt-2 text-[0.92rem] text-paper/70">They hid their screen. It comes back here by itself when they resume.</p>
            </div>
          </div>
        )}

        <div className="pointer-events-none absolute inset-0" onFocus={onControlsFocus} onBlur={onControlsBlur} data-testid="controls">
          {/* Top: one column that ends above the bottom controls, so nothing
              overlaps whatever wraps. A row of LIVE, the quality pill (only
              when degraded) and the zoom pill, which wrap on narrow phones;
              the code chip and theatre exit stay right. Below it the
              reconnecting badge, then the HUD. */}
          <div
            className="absolute flex flex-col gap-2"
            style={{ top: safe.top, left: safe.left, right: safe.right, bottom: `calc(${safe.bottom} + ${hasAudio && muted ? "7.5rem" : "3.25rem"})` }}
            data-testid="top-controls"
          >
            <div className="flex shrink-0 items-start gap-2">
              <div className="flex min-w-0 flex-1 flex-wrap items-start gap-2">
                {state === "live" && (
                  <span className="inline-flex items-center gap-1.5 rounded-sm bg-stamp px-2 py-1 font-mono text-[0.7rem] font-medium tracking-[0.14em] text-paper shadow" data-testid="live-badge">
                    <span className="size-1.5 animate-breathe rounded-full bg-paper" /> LIVE
                  </span>
                )}
                {degraded && (
                  <Overlay>
                    <button type="button" className="glass inline-flex min-h-11 items-center gap-2 px-2.5 text-[0.76rem]" onClick={() => setHud((h) => !h)} aria-expanded={hud} data-testid="quality-pill">
                      <Bars bars={bars} /> {state === "reconnecting" ? "Reconnecting" : "Weak connection"}
                    </button>
                  </Overlay>
                )}
                {!isFit(view) && state !== "paused" && (
                  <Overlay>
                    <button type="button" className="glass inline-flex min-h-11 items-center gap-1.5 px-2.5 text-[0.76rem]" onClick={() => setView(FIT)} data-testid="zoom-level" aria-label={`Zoomed ${view.scale.toFixed(1)} times. Fit to screen`}>
                      <span className="num">{view.scale.toFixed(1)}×</span>
                      <span className="text-paper/70">Fit</span>
                    </button>
                  </Overlay>
                )}
              </div>
              {topAction && state !== "paused" && <Overlay className={`shrink-0 ${fade}`}>{topAction}</Overlay>}
              {rowCode && (
                <Overlay className={`shrink-0 min-[560px]:hidden ${fade}`}>
                  <CodeChip code={rowCode} compact hud={hud} onClick={() => setHud((h) => !h)} />
                </Overlay>
              )}
              {immersive && desktop && (
                <Overlay className={`shrink-0 ${fade}`}>
                  <button type="button" className="glass-btn" onClick={() => setTheatre(false)} aria-label="Leave theatre mode">
                    <IconClose className="size-4" />
                  </button>
                </Overlay>
              )}
            </div>

            {/* State badge over the frozen or paused frame. */}
            {state === "reconnecting" && (
              <div className="flex shrink-0 justify-center px-1">
                <div className="glass flex max-w-sm items-center gap-3 px-4 py-3" role="status" data-testid="state-badge" data-cause={reconnect?.cause ?? "network"} data-long={reconnect?.long ? "1" : "0"}>
                  <span aria-hidden="true" className="size-4 shrink-0 animate-spin rounded-full border-2 border-paper/30 border-t-paper motion-reduce:animate-none" />
                  <span>
                    <span className="block text-[0.92rem] font-semibold">{reconnectCopy(reconnect)[0]}</span>
                    {/* A short landscape phone with the HUD open keeps the headline only, so the HUD header fits. */}
                    {!(hud && short) && <span className="block text-[0.78rem] text-paper/70">{reconnectCopy(reconnect)[1]}</span>}
                  </span>
                </div>
              </div>
            )}

            {hud && (
              // Above the bottom controls (and "Tap to unmute"). The header with
              // the close button never scrolls and never shrinks below its 44 px
              // target; only the body scrolls on a short landscape phone.
              <Overlay className="flex min-h-11 w-[min(19rem,100%)] self-end flex-col overflow-hidden rounded-md">
                <section className="glass flex min-h-11 flex-col overflow-hidden" aria-label="Connection quality" data-testid="hud">
                  <div className="flex h-11 shrink-0 items-center pl-3.5">
                    <h2 className="mr-auto font-mono text-[0.66rem] tracking-[0.14em] text-paper/60 uppercase">Connection</h2>
                    <button type="button" className="min-h-11 shrink-0 rounded-md px-2.5 text-[0.78rem] text-paper/75" onClick={() => setHudMore((m) => !m)} aria-expanded={hudMore} data-testid="hud-more">
                      {hudMore ? "Less" : "More"}
                    </button>
                    <button type="button" className="grid size-11 shrink-0 place-items-center rounded-md text-paper/70" onClick={() => setHud(false)} aria-label="Close connection details" data-testid="hud-close">
                      <IconClose className="size-4" />
                    </button>
                  </div>
                  <div className="min-h-0 overflow-y-auto px-3.5 pb-3.5" data-testid="hud-body">
                  {rowCode && (
                    <p className="mb-2.5 flex items-center gap-2 border-b border-white/10 pb-2.5 text-[0.82rem]" data-testid="hud-code">
                      <span className="text-paper/55">Your code</span>
                      <CodeDot code={rowCode} />
                      <span className="num font-medium tracking-wider">{rowCode}</span>
                      {rowCodeColour(rowCode) && <span className="text-paper/70">· {rowCodeColour(rowCode)}</span>}
                    </p>
                  )}
                  <dl className="grid grid-cols-2 gap-x-3 gap-y-1.5 text-[0.82rem]" data-testid="hud-rows">
                    {hudRows(quality, hudMore).map(([k, v]) => (
                      <div key={k} className="contents">
                        <dt className="text-paper/55">{k}</dt>
                        <dd className="num text-right text-[0.78rem]" data-hud={k}>{v}</dd>
                      </div>
                    ))}
                  </dl>
                  </div>
                </section>
              </Overlay>
            )}
          </div>

          {hasAudio && muted && (
            <div className="absolute inset-x-0 flex justify-center" style={{ bottom: "calc(max(0.75rem, env(safe-area-inset-bottom)) + 4.25rem)" }}>
              <Overlay>
                <button type="button" className="glass-btn border-white/25 px-4 text-[0.92rem]" onClick={unmute} data-testid="unmute">
                  <IconMuted className="size-5" /> Tap to unmute
                </button>
              </Overlay>
            </div>
          )}

          {/* Draw mode replaces the bottom controls with the draw toolbar. */}
          {drawing && drawTools && (
            <Overlay className="absolute flex items-end justify-center" style={{ bottom: safe.bottom, left: safe.left, right: safe.right }}>
              {drawTools}
            </Overlay>
          )}
          {/* Bottom controls: always tap-reachable, never hover-only. */}
          {!(drawing && drawTools) && <Overlay className={`absolute flex items-end gap-1 min-[360px]:gap-2 ${fade}`} style={{ bottom: safe.bottom, left: safe.left, right: safe.right }}>
            {rowCode && <CodeChip code={rowCode} hud={hud} onClick={() => setHud((h) => !h)} />}
            <span className="flex-1" />
            <button type="button" className="glass-btn" onClick={() => setHud((h) => !h)} aria-label="Connection details" aria-expanded={hud}>
              <IconInfo className="size-5" />
            </button>
            <button type="button" className="glass-btn" onClick={zoomKey} aria-label={isFit(view) ? "Zoom in" : "Fit to screen"}>
              {isFit(view) ? <IconZoom className="size-5" /> : <IconFit className="size-5" />}
            </button>
            <button type="button" className="glass-btn" onClick={() => setFill((f) => !f)} aria-pressed={fill} aria-label="Fill the screen (crops the edges)" data-testid="fill-toggle">
              {fill ? <IconShrink className="size-5" /> : <IconExpand className="size-5" />}
            </button>
            {hasAudio && (
              <button type="button" className="glass-btn" onClick={toggleMute} aria-label={muted ? "Unmute" : "Mute"}>
                {muted ? <IconMuted className="size-5" /> : <IconSound className="size-5" />}
              </button>
            )}
            {pip && (
              <button type="button" className="glass-btn" onClick={() => void togglePip()} aria-label="Picture in picture">
                <IconPip className="size-5" />
              </button>
            )}
            {fsSupported && (
              <button type="button" className="glass-btn" onClick={toggleFullscreen} aria-label={fullscreen ? "Exit fullscreen" : "Fullscreen"}>
                {fullscreen ? <IconExitFullscreen className="size-5" /> : <IconFullscreen className="size-5" />}
              </button>
            )}
          </Overlay>}
        </div>
      </div>
    </div>
  );
}

/** HUD rows: the compact set, plus details on "More". */
export function hudRows(q: Quality | null | undefined, more: boolean): [string, string][] {
  const rows: [string, string][] = [
    ["RTT", q?.rtt_ms != null ? (q.rtt_ms < 1 ? "<1 ms" : `${Math.round(q.rtt_ms)} ms`) : "—"],
    ["Loss", q?.loss_pct != null ? `${q.loss_pct.toFixed(1)} %` : "—"],
    ["Bitrate", q?.bitrate_kbps != null ? `${q.bitrate_kbps} kb/s` : "—"],
    ["Frame rate", q?.fps != null ? `${Math.round(q.fps)} fps` : "—"],
    ["Path", q?.path ?? "—"],
  ];
  if (!more) return rows;
  const e = q?.e2ee;
  return [
    ...rows,
    ["Rendition", q?.rendition ? (q.rendition === "native" ? "Native" : "Low (180p)") : "—"],
    ["Layer", q?.temporal ?? q?.layer ?? "—"],
    ["Freezes", String(q?.freezes ?? 0)],
    ["Render gaps", q?.render_gaps != null ? String(q.render_gaps) : "—"],
    ["Delay", q?.g2g_ms != null ? `${q.g2g_ms} ms` : "—"],
    ["E2EE epoch", e?.epoch != null ? String(e.epoch) : "—"],
    ["Decrypt drops", e ? (e.failures ? `${e.failures}${e.top ? ` · ${e.top}` : ""}` : "0") : "—"],
  ];
}

function CodeDot({ code }: { code: string }) {
  const colour = rowCodeColour(code);
  return <span aria-hidden="true" className="size-2.5 shrink-0 rounded-full" style={{ background: colour ? ROW_COLOUR_HEX[colour] : "transparent" }} />;
}

/**
 * The viewer's own row code and colour, so they can read it to the host. A
 * real button at every width: it opens the HUD, which repeats the code with
 * its colour name. Narrow screens get the compact form in the top corner,
 * where the bottom bar has no room.
 */
function CodeChip({ code, compact = false, hud, onClick }: { code: string; compact?: boolean; hud: boolean; onClick: () => void }) {
  const colour = rowCodeColour(code);
  return (
    <button
      type="button"
      className={`glass min-h-11 items-center gap-2 px-3 text-[0.78rem] ${compact ? "inline-flex" : "hidden min-[560px]:inline-flex"}`}
      onClick={onClick}
      aria-expanded={hud}
      aria-label={`Your code ${code}${colour ? `, ${colour}` : ""}. Connection details`}
      data-testid={compact ? "chrome-code-compact" : "chrome-code"}
    >
      {!compact && <span className="text-paper/60">Your code</span>}
      <CodeDot code={code} />
      <span className="num font-medium tracking-wider">{code}</span>
      {!compact && colour && <span className="text-paper/60">{colour}</span>}
    </button>
  );
}

function Bars({ bars }: { bars: number }) {
  return (
    <span aria-label={bars < 0 ? "No signal data" : `Signal ${bars} of 4`} role="img" className="inline-flex h-3 items-end gap-[2px]">
      {[1, 2, 3, 4].map((i) => (
        <span key={i} className={`w-[3px] rounded-[1px] ${i <= bars ? (bars <= 1 ? "bg-stamp" : "bg-warn") : "bg-paper/25"}`} style={{ height: `${i * 25}%` }} />
      ))}
    </span>
  );
}
