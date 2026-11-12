/**
 * Sender-window parts of track D2 (FEATURES.md): the privacy curtain (Escape,
 * one-tap lift), the host's preview with the shared annotation overlay, and
 * the sender quality HUD. Kept apart from index.tsx so the hook-up is small.
 */
import { useEffect, useMemo, useRef, useState, type PointerEvent as RPointerEvent } from "react";
import { ANNOTATION_HOST } from "@passstream/e2ee";
import { InkInput, type Tool } from "../annotations/input";
import { InkOverlay } from "../annotations/Overlay";
import { frameRect, toFrame } from "../annotations/geometry";
import { HudSampler, kbps, senderHud, type SenderHud } from "../hud/stats";
import type { SenderCore } from "./core";
import { senderHealth } from "./health";

type Act = (op: string, args: Record<string, unknown>) => void;

/** Escape lowers the curtain (pause). It never lifts it, so a double press can't uncover the screen. */
export function useCurtainKey(core: SenderCore | null, act: Act) {
  const ref = useRef({ core, act });
  ref.current = { core, act };
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.repeat) return;
      const { core, act } = ref.current;
      if (!core || core.phase === "ended" || core.latches.paused) return;
      act("pause", {});
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, []);
}

/** While curtained: what viewers see, and the one-tap lift. */
export function CurtainBanner({ core, act }: { core: SenderCore; act: Act }) {
  if (!core.latches.paused) return null;
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-md bg-ink px-4 py-3 text-paper" role="status" data-testid="curtain-banner">
      <span className="flex gap-1" aria-hidden="true">
        <span className="h-5 w-1.5 rounded-sm bg-warn-tint" />
        <span className="h-5 w-1.5 rounded-sm bg-warn-tint" />
      </span>
      <span className="flex-1 text-[0.92rem]">
        <strong>Curtain down.</strong> Viewers see "Paused"; no picture or sound leaves this window.
      </span>
      <button type="button" className="min-h-11 rounded-md bg-stamp px-4 text-[0.92rem] font-semibold text-paper hover:bg-stamp-deep" onClick={() => act("resume", {})} data-testid="lift-curtain">
        Lift curtain
      </button>
    </div>
  );
}

/**
 * The host's preview of the capture with the shared overlay. The host can
 * point (laser) or draw (pen) for every viewer, and sees viewers' ink when
 * they're allowed to draw. Blurred and dimmed while curtained.
 */
export function Preview({ core, act }: { core: SenderCore; act: Act }) {
  const video = useRef<HTMLVideoElement>(null);
  const [tool, setTool] = useState<Tool>("laser");
  const [, tick] = useState(0);
  const a = core.annotations;
  useEffect(() => a.subscribe(() => tick((n) => n + 1)), [a]);
  const input = useMemo(
    () =>
      new InkInput({
        echo: (m) => a.store.apply(ANNOTATION_HOST, m, performance.now()),
        emit: (m) => a.draw(m, true),
      }),
    [a],
  );
  useEffect(() => () => input.dispose(), [input]);
  useEffect(() => input.setTool(tool), [input, tool]);
  useEffect(() => {
    const v = video.current;
    if (v && v.srcObject !== core.preview) {
      v.srcObject = core.preview;
      void v.play().catch(() => undefined);
    }
  });
  const curtained = core.latches.paused;
  const point = (e: RPointerEvent<HTMLDivElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const v = video.current;
    return toFrame({ x: e.clientX - r.left, y: e.clientY - r.top }, frameRect({ w: r.width, h: r.height }, v && v.videoWidth ? { w: v.videoWidth, h: v.videoHeight } : null, false));
  };
  if (!core.capture) return null;
  return (
    <section className="space-y-2" aria-label="Preview and annotations">
      <div
        className="relative aspect-video touch-none overflow-hidden rounded-md bg-night select-none"
        onPointerDown={(e) => {
          if (curtained) return;
          const p = point(e);
          if (!p) return;
          e.currentTarget.setPointerCapture(e.pointerId);
          input.begin(p);
        }}
        onPointerMove={(e) => {
          const p = input.active ? point(e) : null;
          if (p) input.move(p);
        }}
        onPointerUp={() => input.end()}
        onPointerCancel={() => input.end()}
        data-testid="sender-preview"
      >
        <video ref={video} muted playsInline autoPlay className={`h-full w-full object-contain transition-[filter] ${curtained ? "blur-2xl brightness-50" : ""}`} />
        {!curtained && <InkOverlay store={a.store} video={() => (video.current?.videoWidth ? { w: video.current.videoWidth, h: video.current.videoHeight } : null)} />}
        {curtained && <p className="absolute inset-0 grid place-items-center text-[1.1rem] font-semibold text-paper">Curtain down</p>}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-[0.85rem]" role="toolbar" aria-label="Annotations">
        <span className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">Point</span>
        {(["laser", "pen"] as const).map((t) => (
          <button key={t} type="button" className={`min-h-9 rounded-md border border-paper-edge px-3 ${tool === t ? "bg-ink text-paper" : ""}`} onClick={() => setTool(t)} aria-pressed={tool === t} data-testid={`host-tool-${t}`}>
            {t === "laser" ? "Laser" : "Pen"}
          </button>
        ))}
        <button type="button" className="min-h-9 rounded-md border border-paper-edge px-3" onClick={() => act("annotations.clear", {})} data-testid="annotations-clear">
          Clear all
        </button>
        <label className="ml-auto flex min-h-9 items-center gap-2">
          <input type="checkbox" checked={a.viewersEnabled} onChange={(e) => act("annotations", { viewers: e.target.checked })} data-testid="viewers-draw" />
          Viewers can draw
        </label>
      </div>
    </section>
  );
}

/** The sender quality HUD: collapsed by default; while open it samples at 2 Hz and audio stats at 1 Hz. */
export function SenderHudPanel({ core }: { core: SenderCore }) {
  const [open, setOpen] = useState(false);
  const [, tick] = useState(0);
  const audio = useRef<{ prev: { bytes: number; at: number } | null; kbps: number | null }>({ prev: null, kbps: null });
  const sampler = useMemo(
    () =>
      new HudSampler<SenderHud>(() => {
        const a = core.audio;
        return senderHud(senderHealth(core.capture, core.session?.quality), { present: !!a && a.readyState === "live", muted: core.latches.paused, kbps: audio.current.kbps });
      }),
    [core],
  );
  useEffect(() => {
    if (!open) return;
    const t = setInterval(() => tick((n) => n + 1), 500);
    const s = setInterval(() => {
      void core.audioBytes().then((cur) => {
        if (!cur) return void (audio.current.kbps = null);
        audio.current.kbps = kbps(audio.current.prev, cur);
        audio.current.prev = cur;
      });
    }, 1000);
    return () => (clearInterval(t), clearInterval(s));
  }, [open, core]);
  const h = open ? sampler.get() : null;
  const rows: [string, string][] = h
    ? [
        ["Uplink", h.uplink_kbps !== null ? `${h.uplink_kbps} kb/s` : "—"],
        ["Encode", h.encode_ms !== null ? `${h.encode_ms} ms/frame` : "—"],
        ["Encoder busy", h.encode_cpu_pct !== null ? `${h.encode_cpu_pct} %` : "—"],
        ["Limited by", h.quality_limitation],
        ["Frame rate", h.fps !== null ? `${h.fps} fps` : "—"],
        ["Resolution", h.resolution ?? "—"],
        ["Audio", h.audio],
        ["E2EE epoch", String(core.host.keys.activeEpoch)],
      ]
    : [];
  return (
    <details className="rounded-md border border-paper-edge px-3 py-2" onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)} data-testid="sender-hud">
      <summary className="min-h-9 cursor-pointer content-center text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">Connection quality</summary>
      {h && (
        <dl className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1 text-[0.85rem]">
          {rows.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-ink-mute">{k}</dt>
              <dd className="num text-right" data-hud={k}>{v}</dd>
            </div>
          ))}
        </dl>
      )}
    </details>
  );
}
