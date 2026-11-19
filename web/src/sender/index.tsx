import { StrictMode, useEffect, useState, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import "../assets/theme.css";
import { isE2EEError, type PinPolicy } from "@passstream/e2ee";
import { testSource } from "../source";
import { addTestTone } from "../audio";
import { Brand } from "../viewer-ui/parts";
import { ROW_COLOUR_HEX, rowCodeColour } from "../viewer-ui/rowcode";
import { SenderCore, type Result } from "./core";
import { LocalChannel, takeLaunchFragment, type Command } from "./local-channel";
import { SENDER_TEST_HOOKS } from "./test-hooks";
import { OwnerActions } from "./owner-actions";
import { CurtainBanner, Preview, SenderHudPanel, useCurtainKey } from "./features";

type Shell = { state: string; detail?: string };

function useCore(core: SenderCore | null) {
  const [, tick] = useState(0);
  useEffect(() => core?.subscribe(() => tick((n) => n + 1)), [core]);
}

function useShell(store: ShellStore) {
  return useSyncExternalStore(store.subscribe, store.get);
}

class ShellStore {
  private v: Shell = { state: "connecting" };
  private core: SenderCore | null = null;
  private l = new Set<() => void>();
  subscribe = (f: () => void) => {
    this.l.add(f);
    return () => this.l.delete(f);
  };
  get = () => this.v;
  getCore = () => this.core;
  set(v: Shell) {
    this.v = v;
    this.l.forEach((f) => f());
  }
  setCore(c: SenderCore | null) {
    this.core = c;
    this.v = { ...this.v, state: c ? "ready" : this.v.state };
    this.l.forEach((f) => f());
  }
}

/** What the header shows for each phase; `data-phase` keeps the raw value. */
export const PHASE_COPY: Record<string, { label: string; tone: "idle" | "live" | "warn" | "off" }> = {
  connecting: { label: "Connecting", tone: "idle" },
  ready: { label: "Connecting", tone: "idle" },
  starting: { label: "Starting", tone: "idle" },
  picking: { label: "Not sharing yet", tone: "idle" },
  live: { label: "Live", tone: "live" },
  paused: { label: "Paused", tone: "warn" },
  ended: { label: "Ended", tone: "off" },
  superseded: { label: "Moved", tone: "off" },
  closed: { label: "Closed", tone: "off" },
  error: { label: "Error", tone: "warn" },
};

const SHELL_COPY: Record<string, string> = {
  connecting: "Connecting to PassStream on this machine…",
  ready: "Starting the stream…",
  superseded: "Another window is the sender now. You can close this one.",
  closed: "This window isn't connected to a stream. Start one with passstream share.",
  error: "The sender couldn't start.",
};

/** Plain words for a refused command. Unknown codes fall back to the code itself. */
export function commandError(op: string, code: string, details?: Record<string, unknown>): string {
  if (op === "safe" && code === "storage" && details?.applied === true)
    return "Safe mode is on for this capture. PassStream couldn't confirm it will stay on after reload.";
  const what: Record<string, string> = {
    paused: "Start sharing first: there's no picture to use yet.",
    not_live: "This stream isn't live on this machine any more.",
    conflict: "That's already in progress.",
    not_found: "There's nothing to stop or publish yet.",
    not_supported: "This browser can't do that.",
    rate_limited: "Too many requests. Wait a moment and try again.",
    rotation_pending: "A PIN change is still finishing. Try again in a moment.",
    sender_offline: "This window lost its connection to PassStream on this machine.",
    sender_timeout: "That took too long and was given up.",
    upload_failed: "The upload failed. Check the Pass service, then try again.",
    recording_too_large: "The recording got too large and was dropped.",
    recording_failed: "The recording failed.",
    local_owner_required: "That needs to be done from this machine.",
    unknown_admission: "That viewer already left.",
    unknown_knock: "That request already ended.",
    storage: "Couldn't save the change on this machine, so it wasn't applied.",
  };
  return what[code] ?? `Couldn't ${op.replace(/[._]/g, " ")} (${code}).`;
}

/** A cancelled picker is a normal choice, not a failure. */
export function shareError(e: unknown): { text: string; error: boolean } {
  const name = e instanceof DOMException || e instanceof Error ? e.name : "";
  if (name === "NotAllowedError" || name === "AbortError") return { text: "Nothing was picked, so nothing is being shared.", error: false };
  if (name === "NotFoundError" || name === "NotReadableError") return { text: "The browser couldn't capture that. Pick another screen, window or tab.", error: true };
  return { text: `Screen sharing didn't start (${e instanceof Error ? e.message : String(e)}).`, error: true };
}

function Code({ code }: { code: string }) {
  const colour = rowCodeColour(code);
  return (
    <span className="inline-flex items-center gap-1.5 font-mono font-semibold" data-testid="row-code">
      <span aria-hidden="true" className="inline-block size-2.5 rounded-full" style={{ background: colour ? ROW_COLOUR_HEX[colour] : "#888" }} />
      {code} <span className="font-sans text-[0.8rem] font-normal text-ink-mute">· {colour}</span>
    </span>
  );
}

function Btn({ children, onClick, tone = "plain", testid, disabled }: { children: React.ReactNode; onClick: () => void; tone?: "plain" | "stamp" | "ghost"; testid?: string; disabled?: boolean }) {
  const cls =
    tone === "stamp"
      ? "bg-stamp text-paper hover:bg-stamp-deep"
      : tone === "ghost"
        ? "border border-paper-edge bg-transparent hover:bg-paper-deep"
        : "bg-ink text-paper hover:bg-ink-soft";
  return (
    <button type="button" disabled={disabled} data-testid={testid} onClick={onClick} className={`min-h-9 rounded-md px-3 py-1.5 text-[0.88rem] font-medium disabled:opacity-40 ${cls}`}>
      {children}
    </button>
  );
}

function PinPanel({ core }: { core: SenderCore }) {
  const [shown, setShown] = useState(false);
  const [warn, setWarn] = useState(false);
  useEffect(() => {
    if (!shown) return;
    const t = setTimeout(() => setShown(false), 10_000);
    return () => clearTimeout(t);
  }, [shown]);
  const policy: PinPolicy = core.policy;
  const pin = core.pin;
  if (policy.kind === "none")
    return (
      <p className="rounded-md bg-warn-tint px-3 py-2 text-[0.88rem] text-warn" data-testid="no-pin">
        No PIN: anyone with the link can watch. It's still end-to-end encrypted (beta).
      </p>
    );
  const masked = "•".repeat(policy.kind === "digits" ? policy.length : 8);
  return (
    <div className="flex flex-wrap items-center gap-3">
      <span className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">PIN</span>
      <span className="font-mono text-[1.4rem] tracking-[0.2em]" data-testid="pin">
        {shown ? pin : masked}
      </span>
      {core.pendingPin && <span className="text-[0.8rem] text-warn">PIN change pending</span>}
      {!shown && !warn && (
        <Btn tone="ghost" testid="reveal" onClick={() => setWarn(true)}>
          Reveal
        </Btn>
      )}
      {warn && !shown && (
        <span className="flex items-center gap-2 text-[0.82rem]">
          <span className="text-warn">Visible in your share if this window is captured.</span>
          <Btn tone="stamp" testid="reveal-confirm" onClick={() => (setShown(true), setWarn(false))}>
            Show for 10 s
          </Btn>
          <Btn tone="ghost" onClick={() => setWarn(false)}>
            Cancel
          </Btn>
        </span>
      )}
    </div>
  );
}

function Knocks({ core, act }: { core: SenderCore; act: (op: string, args: Record<string, unknown>) => void }) {
  const [confirm, setConfirm] = useState<string | null>(null);
  const knocks = core.knocks();
  if (knocks.length === 0) return null;
  return (
    <section className="space-y-2" aria-label="Knocks">
      <h2 className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">Waiting to join</h2>
      {knocks.map((k) => (
        <div key={k.knockId} className="flex flex-wrap items-center gap-3 rounded-md border border-paper-edge bg-paper-sheet px-3 py-2" data-testid="knock">
          <Code code={k.rowCode} />
          {confirm === k.knockId ? (
            <>
              <span className="text-[0.85rem]">Ask them which code they see. Approve only if it's {k.rowCode}.</span>
              <Btn tone="stamp" testid="approve-confirm" onClick={() => (setConfirm(null), act("knock.approve", { knock_id: k.knockId, row_code: k.rowCode }))}>
                Codes match · approve
              </Btn>
              <Btn tone="ghost" onClick={() => setConfirm(null)}>
                Cancel
              </Btn>
            </>
          ) : (
            <>
              <Btn testid="approve" onClick={() => setConfirm(k.knockId)}>
                Approve…
              </Btn>
              <Btn tone="ghost" testid="deny" onClick={() => act("knock.deny", { knock_id: k.knockId })}>
                Deny
              </Btn>
            </>
          )}
        </div>
      ))}
    </section>
  );
}

function Viewers({ core, act }: { core: SenderCore; act: (op: string, args: Record<string, unknown>) => void }) {
  const rows = new Map<string, ReturnType<SenderCore["viewers"]>>();
  for (const v of core.viewers()) rows.set(v.viewerId, [...(rows.get(v.viewerId) ?? []), v]);
  const watching = [...rows.values()].filter((r) => r.some((v) => v.connected)).length;
  return (
    <section className="space-y-2" aria-label="Viewers">
      <h2 className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase" data-testid="watching">
        Watching · {watching}
      </h2>
      {rows.size === 0 && <p className="text-[0.88rem] text-ink-mute">Nobody yet. Viewers appear here once they enter the PIN.</p>}
      {[...rows.entries()].map(([vid, adms]) => {
        const newest = [...adms].sort((a, b) => b.admittedAt - a.admittedAt)[0];
        return (
          <div key={vid} className="flex flex-wrap items-center gap-3 rounded-md border border-paper-edge px-3 py-2" data-testid="viewer-row">
            <span className={`size-2 rounded-full ${adms.some((a) => a.connected) ? "bg-ok" : "bg-ink-faint"}`} />
            <Code code={newest.rowCode} />
            {newest.viewerReportedLabel && <span className="text-[0.85rem]">{newest.viewerReportedLabel} <span className="text-ink-faint">(viewer-reported)</span></span>}
            {newest.kickIncomplete && <span className="text-[0.8rem] text-stamp">kick incomplete · retry</span>}
            <span className="ml-auto flex gap-2">
              <Btn tone="ghost" testid="kick" onClick={() => act("kick", { admission_id: newest.admissionId })}>
                Kick
              </Btn>
              <Btn tone="ghost" testid="kick-rotate" onClick={() => act("kick", { admission_id: newest.admissionId, rotate_pin: true })}>
                Kick + new PIN
              </Btn>
            </span>
          </div>
        );
      })}
    </section>
  );
}

function CopyLink({ link }: { link: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  useEffect(() => {
    if (state === "idle") return;
    const t = setTimeout(() => setState("idle"), 2500);
    return () => clearTimeout(t);
  }, [state]);
  const copy = () => {
    if (!navigator.clipboard) return setState("failed");
    navigator.clipboard.writeText(link).then(
      () => setState("copied"),
      () => setState("failed"),
    );
  };
  return (
    <>
      <button type="button" className="btn btn-primary" data-testid="copy-link" onClick={copy}>
        {state === "copied" ? "Link copied" : "Copy link"}
      </button>
      <span role="status" className="text-[0.82rem] text-stamp">
        {state === "failed" ? "Couldn't reach the clipboard. Use passstream copy link instead." : ""}
      </span>
    </>
  );
}

/** Stop ends the stream for everyone and retires the link, so it asks once. */
function StopButton({ onStop }: { onStop: () => void }) {
  const [confirm, setConfirm] = useState(false);
  useEffect(() => {
    if (!confirm) return;
    const t = setTimeout(() => setConfirm(false), 6000);
    return () => clearTimeout(t);
  }, [confirm]);
  if (!confirm)
    return (
      <button type="button" className="btn-quiet btn-danger ml-auto" data-testid="stop" onClick={() => setConfirm(true)}>
        Stop sharing
      </button>
    );
  return (
    <span className="ml-auto flex flex-wrap items-center gap-2" role="group" aria-label="Stop sharing?">
      <span className="text-[0.85rem] text-ink-soft">Ends it for everyone. The link stops working.</span>
      <button type="button" className="btn btn-stamp" data-testid="stop-confirm" onClick={onStop} autoFocus>
        Stop
      </button>
      <button type="button" className="btn" onClick={() => setConfirm(false)}>
        Keep sharing
      </button>
    </span>
  );
}

function StatusChip({ phase }: { phase: string }) {
  const copy = PHASE_COPY[phase] ?? { label: phase, tone: "idle" as const };
  const dot = { idle: "bg-ink-faint", live: "animate-breathe bg-stamp", warn: "bg-warn", off: "bg-ink-faint" }[copy.tone];
  return (
    <span className="inline-flex items-center gap-2 font-mono text-[0.75rem] tracking-[0.14em] uppercase" data-testid="phase" role="status">
      <span aria-hidden="true" className={`size-2 rounded-full ${dot}`} />
      {copy.label}
    </span>
  );
}

function SenderWindow({ store, trustedServer }: { store: ShellStore; trustedServer: boolean }) {
  const shell = useShell(store);
  const core = store.getCore();
  useCore(core);
  const [message, setMessage] = useState("");
  const [picker, setPicker] = useState<{ text: string; error: boolean } | null>(null);
  const act = (op: string, args: Record<string, unknown>) => {
    void core?.command("window", "", op, args).then((r: Result) => setMessage(r.ok ? "" : commandError(op, r.error.code, r.error.details)));
  };
  const share = async (test: boolean) => {
    if (!core) return;
    setPicker(null);
    try {
      // Tab or system audio where the browser offers it; it travels E2EE like the picture.
      const stream = test
        ? testSource()
        : await navigator.mediaDevices.getDisplayMedia({ video: { width: { max: 2560 }, height: { max: 1440 }, frameRate: 15 }, audio: true, systemAudio: "include" } as DisplayMediaStreamOptions);
      const tone = Number(new URLSearchParams(location.search).get("tone"));
      if (test && tone > 0) addTestTone(stream, tone);
      await core.share(stream);
    } catch (e) {
      setPicker(shareError(e));
    }
  };
  useCurtainKey(core, act);
  const l = core?.latches;
  const phase = core ? core.phase : shell.state;
  return (
    <main className="min-h-dvh bg-paper text-ink" data-testid="sender" data-phase={phase}>
      <header className="flex items-center justify-between border-b border-paper-edge px-5 py-3">
        <Brand />
        <StatusChip phase={phase} />
      </header>
      <div className="mx-auto max-w-2xl space-y-5 px-5 py-6">
        {trustedServer && (
          <p className="rounded-md bg-warn-tint px-3 py-2 text-[0.82rem] text-warn">
            Browser sender served by the PassStream server: it protects the stream against other people, not against a malicious server.
          </p>
        )}
        {!core && (
          <div data-testid="shell">
            <p className="text-[1.05rem]">{SHELL_COPY[shell.state] ?? shell.state}</p>
            {shell.detail && <p className="mt-1 text-[0.88rem] text-ink-mute">{shell.detail}</p>}
          </div>
        )}
        {core && core.phase === "ended" && (
          <section className="sheet p-6" data-testid="ended">
            <h1 className="display text-[1.7rem] leading-tight">{core.endedReason === "superseded" ? "This window is no longer the sender." : "Sharing ended."}</h1>
            <p className="mt-2 text-ink-soft">
              {core.endedReason === "superseded"
                ? "Another window took over this stream. You can close this one."
                : "Nothing is leaving this machine any more, and the link no longer works. You can close this window."}
            </p>
          </section>
        )}
        {core && core.phase !== "ended" && (
          <>
            {!core.capture && (
              <section className="sheet animate-rise p-6" aria-labelledby="pick-title">
                <p className="label text-stamp">Not sharing yet</p>
                <h1 id="pick-title" className="display mt-1 text-[1.9rem] leading-tight">
                  Pick what to share.
                </h1>
                <p className="mt-2 text-ink-soft">
                  Your browser asks which screen, window or tab. Nothing leaves this machine until you choose one, and you can pause or stop at any time.
                </p>
                <div className="mt-5 flex flex-wrap items-center gap-3">
                  <button type="button" className="btn btn-stamp min-h-12 px-5 text-[0.95rem]" data-testid="share" onClick={() => void share(false)} autoFocus>
                    Share screen, window or tab
                  </button>
                  {SENDER_TEST_HOOKS && (
                    <Btn testid="test-source" onClick={() => void share(true)}>
                      Start test source
                    </Btn>
                  )}
                </div>
                {picker && (
                  <p role={picker.error ? "alert" : "status"} className={`mt-3 text-[0.88rem] ${picker.error ? "text-stamp" : "text-ink-mute"}`} data-testid="picker-note">
                    {picker.text}
                  </p>
                )}
              </section>
            )}
            <section className="space-y-3" aria-labelledby="invite-title">
              <h2 id="invite-title" className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">
                Invite viewers
              </h2>
              <div className="flex flex-wrap items-center gap-3">
                <CopyLink link={core.link} />
              </div>
              <PinPanel core={core} />
              {core.policy.kind !== "none" && <p className="text-[0.82rem] text-ink-mute">Send the link and the PIN separately, so one leaked message isn't enough to watch.</p>}
            </section>
            <section className="space-y-2" aria-labelledby="controls-title">
              <h2 id="controls-title" className="text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">
                Controls
              </h2>
              <div className="flex flex-wrap items-center gap-2">
                {l?.paused ? (
                  <button type="button" className="btn btn-primary" data-testid="resume" onClick={() => act("resume", {})}>
                    Resume
                  </button>
                ) : (
                  <button type="button" className="btn" data-testid="pause" title="Hide your screen from viewers. Escape does the same." onClick={() => act("pause", {})}>
                    Pause · Esc
                  </button>
                )}
                <button type="button" className="btn" data-testid="lock" aria-pressed={!!l?.locked} title={l?.locked ? "Let new viewers join again" : "Keep current viewers, let nobody new in"} onClick={() => act("lock", { locked: !l?.locked })}>
                  {l?.locked ? "Unlock" : "Lock"}
                </button>
                {l?.frozen && (
                  <button type="button" className="btn" data-testid="unfreeze" onClick={() => act("unfreeze", {})}>
                    Unfreeze (new PIN)
                  </button>
                )}
                <span className="inline-flex items-center gap-2 text-[0.82rem] text-ink-mute" role="group" aria-label="Optimise for">
                  Optimise for
                  <span className="inline-flex overflow-hidden rounded-md border border-ink">
                    {(["text", "motion"] as const).map((m) => (
                      <button
                        key={m}
                        type="button"
                        aria-pressed={core.mode === m}
                        data-testid={`mode-${m}`}
                        title={m === "text" ? "Sharp text; motion may stutter" : "Smooth video; small text may soften"}
                        className={`min-h-10 px-3 text-sm font-semibold ${core.mode === m ? "bg-ink text-paper" : "text-ink hover:bg-paper-deep"}`}
                        onClick={() => core.mode !== m && act("mode", { mode: m })}
                      >
                        {m === "text" ? "Text" : "Motion"}
                      </button>
                    ))}
                  </span>
                </span>
                <button type="button" className="btn" data-testid="safe" aria-pressed={core.safe} onClick={() => act("safe", { on: !core.safe })}>
                  {core.safe ? "Safe mode: on" : "Safe mode: off"}
                </button>
                <StopButton onStop={() => act("stop", {})} />
              </div>
              <SafeStatus core={core} />
              {l?.locked && <p className="text-[0.82rem] text-warn">Locked: nobody new can join until you unlock.</p>}
            </section>
            {message && (
              <p role="alert" className="rounded-md border-l-2 border-stamp bg-stamp-tint px-3 py-2 text-[0.88rem] text-stamp-deep" data-testid="command-error">
                {message}
              </p>
            )}
            <CurtainBanner core={core} act={act} />
            <Preview core={core} act={act} />
            {core.host.revocationNotDurable && <p className="text-stamp">Storage failed: media is stopped and joining is refused until it recovers.</p>}
            {core.recovery.state !== "idle" && <p className="text-warn">PIN change pending ({core.recovery.state}).</p>}
            <Knocks core={core} act={act} />
            <Viewers core={core} act={act} />
            <SenderHudPanel core={core} />
            {core.features && core.cfg.channel && <OwnerActions channel={core.cfg.channel} capturing={!!core.capture} />}
          </>
        )}
      </div>
    </main>
  );
}

function render(el: HTMLElement, store: ShellStore, trustedServer: boolean) {
  document.title = "PassStream · Sender";
  createRoot(el).render(
    <StrictMode>
      <SenderWindow store={store} trustedServer={trustedServer} />
    </StrictMode>,
  );
}

/** /sender: the production sender behind `passstream host` (CONTRACT §11). */
export function mountLocalSender(el: HTMLElement) {
  takeLaunchFragment();
  const store = new ShellStore();
  render(el, store, false);
  let core: SenderCore | null = null;
  let booting: Promise<void> | null = null;
  const queue: Command[] = [];
  const run = (c: Command) => void core!.command("local", c.id, c.op, c.args).then((r) => channel.sendResult(c.id, r));
  const channel: LocalChannel = new LocalChannel({
    init: async (init, viaLaunch) => {
      if (core) {
        await core.reattach(init);
        return;
      }
      if (booting) return booting;
      booting = (async () => {
        try {
          core = await SenderCore.start({
            kind: "local",
            stream: init.stream,
            signalUrl: init.signal_url,
            serverUrl: init.server_url,
            policy: init.policy,
            preparedSafeOff: init.safe_off,
            latches: init.latches,
            mayCreate: viaLaunch,
            activeStreamIds: init.active_stream_ids,
            channel,
          });
          if (SENDER_TEST_HOOKS) window.passstreamSender = core;
          store.setCore(core);
          await core.reportNow();
          core.sendSecrets();
          queue.splice(0).forEach(run);
        } catch (e) {
          booting = null;
          const lost = isE2EEError(e, "state_mismatch");
          store.set({ state: "error", detail: lost ? "The stream's local state is gone." : String(e) });
          if (lost) channel.sendEnded("state_lost");
        }
      })();
      return booting;
    },
    command: (c) => (core ? run(c) : queue.push(c)),
    demand: (until) => core?.features?.demand(until),
    view: () => undefined,
    ended: (reason) => void core?.end(reason, false),
    terminal: (kind, detail) => {
      if (kind === "superseded") void core?.abandon();
      store.set({ state: kind, detail });
    },
    status: (state, detail) => {
      if (state === "connecting") core?.features?.reset();
      store.set({ state, detail });
    },
  });
  channel.start();
}

/** /host: the server-served browser sender ("trusted server", HOST-CONTROL §2.1). */
export function mountBrowserSender(el: HTMLElement) {
  const KEY = "ps:host";
  const q = new URLSearchParams(location.hash.slice(1));
  if (q.get("id") && q.get("t")) sessionStorage.setItem(KEY, location.hash.slice(1));
  if (location.hash) history.replaceState(null, "", location.pathname + location.search);
  const f = new URLSearchParams(sessionStorage.getItem(KEY) ?? "");
  const store = new ShellStore();
  render(el, store, true);
  const id = f.get("id"),
    token = f.get("t"),
    cap = f.get("c");
  if (!id || !token || !cap) {
    store.set({ state: "closed", detail: "Open the host link returned when the stream was created." });
    return;
  }
  const pin = f.get("pin") ?? "digits:4";
  const policy: PinPolicy = pin === "none" ? { kind: "none" } : pin === "passphrase" ? { kind: "passphrase" } : { kind: "digits", length: Number(pin.split(":")[1] ?? 4) };
  void SenderCore.start({
    kind: "browser",
    stream: { id, publish_token: token, view_link_base: `${location.origin}/v#c=${cap}` },
    signalUrl: `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/signal`,
    serverUrl: location.origin,
    policy: { pin_policy: policy, pin: null, knock: f.get("knock") === "1", mode: "text", safe: true },
    latches: { locked: false, paused: false, frozen: false },
    mayCreate: true,
  })
    .then(async (core) => {
      if (SENDER_TEST_HOOKS) window.passstreamSender = core;
      store.setCore(core);
      await core.reportNow();
      core.sendSecrets();
    })
    .catch((e) => store.set({ state: "error", detail: String(e) }));
}

/** Safe mode's state, as the host sees it (viewers just see covers). */
function SafeStatus({ core }: { core: SenderCore }) {
  if (!core.safe || !core.capture) return null;
  if (core.safeError) return <p className="text-stamp" data-testid="safe-status">{core.safeError}</p>;
  const s = core.redactor?.stats;
  const text = !s || s.status === "starting"
    ? "Safe mode is starting: all text is hidden until it has been checked."
    : s.status === "unavailable"
      ? "Safe mode can't read the screen right now, so viewers see only hidden text. Turn it off to share normally."
      : s.status === "catching_up"
        ? `Safe mode: checking ${s.pending} new ${s.pending === 1 ? "line" : "lines"}; hidden until checked.`
        : `Safe mode: ${s.covers} ${s.covers === 1 ? "item" : "items"} hidden from viewers.`;
  return (
    <p className={`text-[0.82rem] ${s?.status === "unavailable" ? "text-stamp" : "text-ink-mute"}`} data-testid="safe-status">
      {text}
    </p>
  );
}
