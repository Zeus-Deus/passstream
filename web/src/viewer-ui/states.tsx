import { useEffect, useRef, type ReactNode } from "react";
import { Brand, CodeBadge } from "./parts";
import { Player, type Quality, type Reconnect } from "./Player";

/** The viewer's fixed state set (PLAN "Viewer UX"). */
export type ViewerState =
  | { kind: "waiting" }
  | { kind: "knock"; code: string }
  | { kind: "connecting"; slow?: boolean }
  | { kind: "admitted"; slow?: boolean }
  | { kind: "live" }
  | { kind: "paused" }
  | { kind: "reconnecting"; cause?: Reconnect["cause"]; long?: boolean }
  | { kind: "ended"; reason?: "stopped" | "expired" | "window_closed" | "deleted" | string }
  | { kind: "removed"; reason: "kicked" | "locked" | "denied" | "frozen" | "full" }
  | { kind: "unsupported"; missing?: string[] };

export const STATE_KINDS = ["waiting", "knock", "connecting", "admitted", "live", "paused", "reconnecting", "ended", "removed", "unsupported"] as const;

/**
 * Fail closed: without RTCRtpScriptTransform (SFrame E2EE) the viewer can't
 * decrypt, and PassStream never falls back to plaintext.
 */
export function missingFeatures(g: Partial<typeof globalThis> = globalThis): string[] {
  const out: string[] = [];
  if (typeof g.RTCPeerConnection !== "function") out.push("WebRTC");
  if (typeof (g as { RTCRtpScriptTransform?: unknown }).RTCRtpScriptTransform !== "function") out.push("RTCRtpScriptTransform (encrypted media)");
  if (!g.crypto?.subtle) out.push("WebCrypto");
  return out;
}

function Card({ eyebrow, title, children, tone = "paper", testid }: { eyebrow: string; title: string; children?: ReactNode; tone?: "paper" | "stamp" | "warn"; testid: string }) {
  // Each state is a new screen: move focus to its heading so screen readers
  // announce it, without a visible ring (the heading isn't a control).
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (document.activeElement === document.body || !document.activeElement || document.activeElement?.closest("[data-state-card]")) heading.current?.focus({ preventScroll: true });
  }, [testid]);
  return (
    <main
      data-testid={`state-${testid}`}
      data-state-card=""
      className="flex min-h-dvh flex-col bg-night px-4 pt-[max(1.25rem,env(safe-area-inset-top))] pb-[max(1.5rem,env(safe-area-inset-bottom))] text-paper sm:px-6"
    >
      <div className="mx-auto flex w-full max-w-lg items-center justify-between">
        <Brand night />
      </div>
      <div className="mx-auto flex w-full max-w-lg flex-1 flex-col justify-center py-10" aria-live="polite">
        <p className={`font-mono text-[0.7rem] font-medium tracking-[0.14em] uppercase ${tone === "stamp" ? "text-[#e8735c]" : tone === "warn" ? "text-warn-tint" : "text-paper/55"}`}>{eyebrow}</p>
        <h1 ref={heading} tabIndex={-1} className="display mt-3 text-[2.05rem] leading-[1.06] outline-none sm:text-[2.6rem]">{title}</h1>
        <div className="mt-4 space-y-4 text-[0.98rem] leading-relaxed text-paper/75">{children}</div>
      </div>
      <p className="mx-auto w-full max-w-lg text-[0.76rem] text-paper/60">
        Private screen share, end-to-end encrypted (beta). <a className="underline underline-offset-2" href="/privacy">Privacy notice</a>
      </p>
    </main>
  );
}

function Progress() {
  return (
    <div aria-hidden="true" className="h-1 w-40 overflow-hidden rounded-full bg-white/10">
      <div className="h-full w-1/3 animate-[pulse_1.2s_ease-in-out_infinite] rounded-full bg-stamp motion-reduce:animate-none" />
    </div>
  );
}

function Pulse() {
  return (
    <span aria-hidden="true" className="relative mb-6 inline-flex size-12">
      <span className="absolute inset-0 animate-ping rounded-full bg-stamp/30 motion-reduce:hidden" />
      <span className="relative m-auto size-4 rounded-full bg-stamp" />
    </span>
  );
}

export interface ViewerScreenProps {
  state: ViewerState;
  stream?: MediaStream | null;
  hasAudio?: boolean;
  quality?: Quality | null;
  rowCode?: string | null;
  title?: string | null;
  autoHideMs?: number;
  initialHud?: boolean;
  onRetry?: () => void;
}

export function ViewerScreen({ state, stream, hasAudio, quality, rowCode, title, autoHideMs, initialHud, onRetry }: ViewerScreenProps) {
  switch (state.kind) {
    case "live":
    case "paused":
    case "reconnecting":
      return (
        <Player
          state={state.kind}
          stream={stream}
          reconnect={state.kind === "reconnecting" ? { cause: state.cause ?? "network", long: !!state.long } : null}
          hasAudio={hasAudio}
          quality={quality}
          rowCode={rowCode}
          title={title}
          autoHideMs={autoHideMs}
          initialHud={initialHud}
        />
      );
    case "waiting":
      return (
        <Card testid="waiting" eyebrow="Waiting for host" title="The host is away for a moment.">
          <Pulse />
          <p>You're in. The picture appears as soon as their sender is back, usually within a few seconds. Keep this page open.</p>
        </Card>
      );
    case "knock":
      return (
        <Card testid="knock" eyebrow="Waiting for approval" title="The host needs to let you in.">
          <p>Tell the host your code: <span className="num font-semibold text-paper">{state.code}</span></p>
          <CodeBadge code={state.code} />
          <p className="text-[0.86rem] text-paper/60">They'll see the same code next to your request. It proves it's you, not someone else with the link.</p>
        </Card>
      );
    case "connecting":
      return (
        <Card testid="connecting" eyebrow="Connecting" title="Opening the stream…">
          <Progress />
          <p>Setting up an encrypted connection to the host's screen.</p>
          {state.slow && <p className="text-[0.86rem] text-paper/60" data-testid="slow">This is taking longer than usual. Keep this page open; it keeps trying by itself.</p>}
        </Card>
      );
    case "admitted":
      return (
        <Card testid="admitted" eyebrow="You're in" title="Starting the picture…">
          <Progress />
          <p>The host's computer let you in and sent the keys. The screen appears in a moment.</p>
          {state.slow && <p className="text-[0.86rem] text-paper/60" data-testid="slow">This is taking longer than usual. Keep this page open; it keeps trying by itself.</p>}
        </Card>
      );
    case "ended":
      if (state.reason === "admission_timeout") return (
        <Card testid="admission-timeout" eyebrow="Connection interrupted" title="The host didn't finish the check.">
          <p>Reload this page to try again. The stream stays encrypted.</p>
        </Card>
      );
      return (
        <Card testid="ended" eyebrow="Stream ended" title={state.reason === "expired" ? "This link has expired." : "The host stopped sharing."}>
          <p>{state.reason === "expired" ? "Ask the host for a new link if you still need to watch." : "Thanks for watching. You can close this page."}</p>
        </Card>
      );
    case "removed": {
      const text: Record<string, [string, string]> = {
        kicked: ["You were removed", "The host ended your session on this stream. If you think that was a mistake, ask the host; they may have changed the PIN."],
        locked: ["The stream is locked", "The host isn't letting anyone new in right now. Ask them to unlock it."],
        denied: ["Request declined", "The host declined your request to watch."],
        frozen: ["Joining is paused", "Too many wrong PINs were tried on this stream, so the host's computer stopped new admissions. Ask the host."],
        full: ["The stream is full", "It has reached its viewer limit. Try again later."],
      };
      const [title, body] = text[state.reason] ?? text.kicked;
      return (
        <Card testid={`removed-${state.reason}`} eyebrow={state.reason === "kicked" || state.reason === "denied" ? "Removed" : "Locked"} title={title} tone="stamp">
          <p>{body}</p>
        </Card>
      );
    }
    case "unsupported":
      return (
        <Card testid="unsupported" eyebrow="Browser not supported" title="This browser can't decrypt the stream." tone="warn">
          <p>
            PassStream streams are end-to-end encrypted, and this browser is missing a piece it needs. It never falls back to an unencrypted stream.
          </p>
          {state.missing && state.missing.length > 0 && (
            <ul className="list-inside list-disc text-[0.86rem] text-paper/60">
              {state.missing.map((m) => <li key={m}>{m}</li>)}
            </ul>
          )}
          <p>Open the link in a current Safari, Chrome, Edge or Firefox.</p>
          {onRetry && <button type="button" className="glass-btn" onClick={onRetry}>Check again</button>}
        </Card>
      );
  }
}
