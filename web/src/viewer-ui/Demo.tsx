// Dev-only gallery (/dev/viewer-ui?state=…) that renders every viewer state
// with a fake screen, for screenshots. main.tsx only routes here when
// import.meta.env.DEV, so production builds drop this module.
import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "../assets/theme.css";
import { PinScreen } from "./PinScreen";
import type { Quality } from "./Player";
import { ViewerScreen, type ViewerState } from "./states";

/** A fake shared screen: an editor window with a ticking clock. */
export function fakeScreen(fps = 15): MediaStream | null {
  const c = document.createElement("canvas");
  c.width = 1600;
  c.height = 1000;
  const g = c.getContext("2d");
  if (!g || typeof c.captureStream !== "function") return null;
  const draw = () => {
    g.fillStyle = "#e9e4da";
    g.fillRect(0, 0, c.width, c.height);
    g.fillStyle = "#2b2622";
    g.fillRect(0, 0, c.width, 44);
    g.fillStyle = "#f4eee3";
    g.font = "500 20px monospace";
    g.fillText(`workspace 2 · ${new Date().toLocaleTimeString()}`, 24, 29);
    g.fillStyle = "#fbf8f2";
    g.fillRect(70, 90, 1460, 850);
    g.strokeStyle = "#cbbf9f";
    g.strokeRect(70, 90, 1460, 850);
    g.fillStyle = "#ece5d3";
    g.fillRect(70, 90, 1460, 40);
    ["#e5484d", "#ffc53d", "#30a46c"].forEach((col, i) => {
      g.fillStyle = col;
      g.beginPath();
      g.arc(98 + i * 26, 110, 7, 0, Math.PI * 2);
      g.fill();
    });
    const lines = [
      ["#8e4ec6", "func"],
      ["#1a1614", " (s *Server) Handler() http.Handler {"],
      ["#1a1614", "    mux := http.NewServeMux()"],
      ["#74695f", "    // Every viewer gets its own bounded queue."],
      ["#1a1614", "    mux.HandleFunc(\"GET /signal\", s.signal)"],
      ["#0090ff", "    return withHeaders(mux)"],
      ["#1a1614", "}"],
    ];
    g.font = "28px monospace";
    lines.forEach(([col, text], i) => {
      g.fillStyle = col;
      g.fillText(text, 120, 200 + i * 56);
    });
    g.fillStyle = "#c1422b";
    g.fillRect(120, 620, 520, 6);
    g.fillStyle = "#1a1614";
    g.font = "600 44px serif";
    g.fillText("Readable text, not a blur.", 120, 720);
  };
  draw();
  const t = setInterval(draw, 1000 / fps);
  const s = c.captureStream(fps);
  s.getVideoTracks()[0]?.addEventListener("ended", () => clearInterval(t));
  return s;
}

const GOOD: Quality = { rtt_ms: 38, loss_pct: 0.2, bitrate_kbps: 2400, fps: 15, path: "direct UDP", layer: "L1T3", freezes: 0 };
const WEAK: Quality = { rtt_ms: 240, loss_pct: 6.5, bitrate_kbps: 420, fps: 7, path: "relay (TURN/TCP)", layer: "L1T1", freezes: 3 };

export const DEMO_STATES = [
  "pin",
  "pin-6",
  "pin-passphrase",
  "pin-12",
  "pin-checking",
  "pin-error",
  "pin-locked",
  "waiting",
  "knock",
  "connecting",
  "connecting-slow",
  "admitted",
  "live",
  "live-degraded",
  "paused",
  "reconnecting",
  "reconnecting-long",
  "reconnecting-host",
  "ended",
  "removed",
  "locked",
  "unsupported",
] as const;

export function Demo() {
  const params = new URLSearchParams(location.search);
  const state = params.get("state") ?? "";
  // ?hide=ms turns on the controls' auto-hide (off by default for stable screenshots).
  const hideMs = Number(params.get("hide")) || 0;
  const [stream, setStream] = useState<MediaStream | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const needsVideo = ["live", "live-degraded", "paused", "reconnecting", "reconnecting-long", "reconnecting-host"].includes(state);
  useEffect(() => {
    if (!needsVideo) return;
    const s = fakeScreen();
    setStream(s);
    return () => s?.getTracks().forEach((t) => t.stop());
  }, [needsVideo]);
  useEffect(() => {
    document.documentElement.classList.toggle("night", !state.startsWith("pin") && state !== "");
  }, [state]);

  const onPin = () => {
    setBusy(true);
    setTimeout(() => {
      setBusy(false);
      setError("That PIN didn't match.");
    }, 600);
  };
  if (state.startsWith("pin")) {
    const locked = state === "pin-locked";
    return (
      <PinScreen
        policy={state === "pin-passphrase" ? { kind: "passphrase" } : { kind: "digits", length: state === "pin-6" ? 6 : state === "pin-12" ? 12 : 4 }}
        onSubmit={onPin}
        busy={busy || state === "pin-checking"}
        error={state === "pin-error" || locked ? "That PIN didn't match." : error}
        triesLeft={state === "pin-error" ? 2 : null}
        lockedUntil={locked ? Date.now() + 42_000 : null}
        streamLabel="Design review"
      />
    );
  }
  const map: Record<string, ViewerState> = {
    waiting: { kind: "waiting" },
    knock: { kind: "knock", code: "K7P" },
    connecting: { kind: "connecting" },
    "connecting-slow": { kind: "connecting", slow: true },
    admitted: { kind: "admitted" },
    live: { kind: "live" },
    "live-degraded": { kind: "live" },
    paused: { kind: "paused" },
    reconnecting: { kind: "reconnecting" },
    "reconnecting-long": { kind: "reconnecting", cause: "network", long: true },
    "reconnecting-host": { kind: "reconnecting", cause: "host", long: false },
    ended: { kind: "ended", reason: "stopped" },
    removed: { kind: "removed", reason: "kicked" },
    locked: { kind: "removed", reason: "locked" },
    unsupported: { kind: "unsupported", missing: ["RTCRtpScriptTransform (encrypted media)"] },
  };
  const s = map[state];
  if (!s)
    return (
      <main className="mx-auto max-w-2xl px-5 py-10">
        <h1 className="display text-[2.2rem]">Viewer UI states</h1>
        <p className="mt-2 text-ink-soft">Dev build only. Each state renders with a fake screen.</p>
        <ul className="mt-6 grid grid-cols-2 gap-2 sm:grid-cols-3">
          {DEMO_STATES.map((k) => (
            <li key={k}>
              <a className="btn w-full" href={`/dev/viewer-ui?state=${k}`}>{k}</a>
            </li>
          ))}
          <li><a className="btn w-full" href="/privacy">privacy</a></li>
        </ul>
      </main>
    );
  return (
    <ViewerScreen
      state={s}
      stream={stream}
      // ?audio=1 adds an audio track to any player state ("Tap to unmute" takes room at the bottom).
      hasAudio={state === "live" || params.has("audio")}
      quality={state === "live-degraded" || state.startsWith("reconnecting") ? WEAK : GOOD}
      rowCode="K7P"
      title="Design review"
      autoHideMs={hideMs}
      initialHud={state === "live-degraded"}
    />
  );
}

export function mount(el: HTMLElement) {
  document.title = "PassStream · Viewer UI states";
  createRoot(el).render(
    <StrictMode>
      <Demo />
    </StrictMode>,
  );
}
