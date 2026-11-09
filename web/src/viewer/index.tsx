import { StrictMode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "../assets/theme.css";
import { PinScreen } from "../viewer-ui/PinScreen";
import { Player, type DrawControl, type Quality } from "../viewer-ui/Player";
import { ViewerScreen } from "../viewer-ui/states";
import { Brand, IconEraser, IconLaser, IconPen } from "../viewer-ui/parts";
import { InkOverlay } from "../annotations/Overlay";
import { e2eeHud, FreezeCounter, HudSampler, latestG2G, SessionLayerSource, viewerHud, type MediaLayerSource } from "../hud/stats";
import { ViewerController } from "./controller";
import { OutageClock, TerminalLatch, screenFor, trouble } from "./machine";

/**
 * The HUD model (FEATURES.md "Track D2" §1), recomputed at most twice a
 * second from state the session and controller already keep. The layer
 * source is the seam for the media controller rewrite.
 */
function hudSampler(c: ViewerController, layers: MediaLayerSource = new SessionLayerSource(() => c.session?.quality.layer)) {
  const freezes = new FreezeCounter();
  return new HudSampler<Quality | null>(() => {
    const s = c.session;
    const q = s?.quality;
    if (!s || !q || !q.timestamp) return null;
    freezes.sample({
      at: Date.now(),
      ingress: q.ingressFrames,
      packets: Number(q.stats?.packetsReceived ?? 0),
      paused: s.marker === "paused" || c.screen?.kind === "paused",
      hidden: document.visibilityState === "hidden",
    });
    freezes.observe(s.frameTimeline);
    const h = viewerHud({ sample: q, layer: layers.layer(), freezes: freezes.freezes, renderGaps: freezes.renderGaps, g2g: latestG2G(s.frameTimeline, Date.now()), e2ee: e2eeHud(c.epoch, c.drops) });
    return { ...h, layer: q.layer };
  });
}

/** Draw toggle and toolbar, shown only while the host lets viewers draw. */
function useDraw(c: ViewerController) {
  const a = c.annotations;
  const [active, setActive] = useState(false);
  const [, tick] = useState(0);
  useEffect(() => a.subscribe(() => tick((n) => n + 1)), [a]);
  const on = active && a.policy;
  const control: DrawControl = useMemo(() => ({ active: true, begin: (p) => a.input.begin(p), move: (p) => a.input.move(p), end: () => a.input.end() }), [a]);
  if (!a.policy) return { draw: null, topAction: null, drawTools: null };
  const btn = "glass-btn min-w-11 gap-1.5 px-2.5 text-[0.8rem]";
  return {
    draw: { ...control, active: on },
    topAction: (
      <button type="button" className={`glass-btn ${on ? "border-paper/60 bg-paper/15" : ""}`} onClick={() => setActive((x) => !x)} aria-pressed={on} aria-label={on ? "Stop drawing" : "Draw on the shared screen"} data-testid="draw-toggle">
        <IconPen className="size-5" />
      </button>
    ),
    drawTools: (
      <div className="glass flex items-center gap-1 rounded-md p-1" role="toolbar" aria-label="Drawing tools" data-testid="draw-tools">
        <button type="button" className={`${btn} ${a.tool === "laser" ? "bg-paper/20" : ""}`} onClick={() => a.setTool("laser")} aria-pressed={a.tool === "laser"} data-testid="tool-laser">
          <IconLaser className="size-5" /> Laser
        </button>
        <button type="button" className={`${btn} ${a.tool === "pen" ? "bg-paper/20" : ""}`} onClick={() => a.setTool("pen")} aria-pressed={a.tool === "pen"} data-testid="tool-pen">
          <IconPen className="size-5" /> Pen
        </button>
        <button type="button" className={btn} onClick={() => a.clearOwn()} aria-label="Clear my drawing" data-testid="tool-clear">
          <IconEraser className="size-5" /> Clear
        </button>
        <button type="button" className={`${btn} font-semibold`} onClick={() => setActive(false)} data-testid="tool-done">
          Done
        </button>
      </div>
    ),
  };
}

/** Night stage for everything but the PIN sheet, so overscroll never flashes paper. */
function useNight(night: boolean) {
  useEffect(() => {
    document.documentElement.classList.toggle("night", night);
  }, [night]);
}

function Viewer({ c }: { c: ViewerController }) {
  const [, tick] = useState(0);
  useEffect(() => c.subscribe(() => tick((n) => n + 1)), [c]);
  const videoEl = useRef<HTMLVideoElement | null>(null);
  const onVideo = useCallback(
    (v: HTMLVideoElement | null) => {
      videoEl.current = v;
      c.session?.attachVideo(v);
    },
    [c],
  );
  const frameSize = useCallback(() => {
    const v = videoEl.current;
    return v && v.videoWidth ? { w: v.videoWidth, h: v.videoHeight } : null;
  }, []);
  const outage = useRef(new OutageClock());
  const hud = useMemo(() => hudSampler(c), [c]);
  const drawing = useDraw(c);
  const latch = useRef(new TerminalLatch());
  const snap = c.snapshot();
  const screen = latch.current.hold(screenFor(snap, outage.current.update(trouble(snap), snap.now)), snap);
  useNight(screen.kind !== "pin");
  // Diagnostics for tests and the torture suite: the drawn screen.
  c.screen = screen;
  const s = c.session;
  switch (screen.kind) {
    case "invalid":
      return (
        <main className="flex min-h-dvh flex-col items-center justify-center gap-4 bg-night px-6 text-center text-paper" data-testid="state-invalid">
          <Brand night />
          <h1 className="display text-[1.6rem]">This link is incomplete.</h1>
          <p className="text-paper/75">Ask the host to send it again. Copy the whole link, including the part after the #.</p>
        </main>
      );
    case "unsupported":
      return <ViewerScreen state={{ kind: "unsupported", missing: screen.missing }} onRetry={() => location.reload()} />;
    case "removed":
    case "ended":
      return <ViewerScreen state={screen} />;
    case "pin":
      return (
        <div data-testid="pin-screen" data-lockout={screen.lockout ? "1" : "0"}>
          <PinScreen
            policy={screen.policy}
            busy={screen.checking}
            error={c.pinError}
            triesLeft={c.triesLeft}
            lockedUntil={c.lockedUntil}
            onSubmit={(secret, name) => c.submitPin(secret, name)}
          />
        </div>
      );
    case "knock":
    case "waiting":
    case "connecting":
      return <ViewerScreen state={screen} />;
    case "admitted":
    case "live":
    case "paused":
    case "reconnecting": {
      // Admitted, media flowing: the immersive player. It mounts before the
      // first frame so the session can watch decoded frames on its <video>.
      const state = screen.kind === "admitted" ? "live" : screen.kind;
      return (
        <>
          <Player
            stream={s?.remoteStream ?? null}
            state={state}
            reconnect={screen.kind === "reconnecting" ? { cause: screen.cause, long: screen.long } : null}
            hasAudio={c.audioLive && state !== "paused"}
            quality={hud.get()}
            rowCode={c.rowCode}
            onVideo={onVideo}
            overlay={(fill) => <InkOverlay store={c.annotations.store} video={frameSize} fill={fill} />}
            draw={drawing.draw}
            topAction={drawing.topAction}
            drawTools={drawing.drawTools}
          />
          {screen.kind === "admitted" && (
            <div className="fixed inset-0">
              <ViewerScreen state={screen} />
            </div>
          )}
        </>
      );
    }
  }
}

/** /v#c=…&k=…&s=…: the one viewer. */
export function mountViewer(el: HTMLElement) {
  document.title = "PassStream";
  const c = new ViewerController();
  window.passstreamViewer = c;
  function App() {
    const video = useMemo(() => document.createElement("video"), []);
    useEffect(() => {
      video.muted = true;
      video.playsInline = true;
      c.start(video);
    }, [video]);
    return <Viewer c={c} />;
  }
  createRoot(el).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
