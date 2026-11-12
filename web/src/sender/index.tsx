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

function SenderWindow({ store, trustedServer }: { store: ShellStore; trustedServer: boolean }) {
  const shell = useShell(store);
  const core = store.getCore();
  useCore(core);
  const [message, setMessage] = useState("");
  const act = (op: string, args: Record<string, unknown>) => {
    void core?.command("window", "", op, args).then((r: Result) => setMessage(r.ok ? "" : `${op}: ${r.error.code}`));
  };
  const share = async (test: boolean) => {
    if (!core) return;
    try {
      // Tab or system audio where the browser offers it; it travels E2EE like the picture.
      const stream = test
        ? testSource()
        : await navigator.mediaDevices.getDisplayMedia({ video: { width: { max: 2560 }, height: { max: 1440 }, frameRate: 15 }, audio: true, systemAudio: "include" } as DisplayMediaStreamOptions);
      const tone = Number(new URLSearchParams(location.search).get("tone"));
      if (test && tone > 0) addTestTone(stream, tone);
      await core.share(stream);
    } catch (e) {
      setMessage(String(e));
    }
  };
  useCurtainKey(core, act);
  const l = core?.latches;
  return (
    <main className="min-h-dvh bg-paper text-ink" data-testid="sender" data-phase={core?.phase ?? shell.state}>
      <header className="flex items-center justify-between border-b border-paper-edge px-5 py-3">
        <Brand />
        <span className="font-mono text-[0.75rem] tracking-[0.14em] uppercase" data-testid="phase">
          {core ? core.phase : shell.state}
        </span>
      </header>
      <div className="mx-auto max-w-2xl space-y-5 px-5 py-5">
        {core?.features && core.cfg.channel && <OwnerActions channel={core.cfg.channel} />}
        {trustedServer && (
          <p className="rounded-md bg-warn-tint px-3 py-2 text-[0.82rem] text-warn">
            Browser sender served by the PassStream server: it protects the stream against other people, not against a malicious server.
          </p>
        )}
        {!core && <p data-testid="shell">{shell.detail ? `${shell.state} · ${shell.detail}` : shell.state}</p>}
        {core && core.phase === "ended" && (
          <p className="text-[1.1rem]" data-testid="ended">
            {core.endedReason === "superseded" ? "This window is no longer the sender." : "Sharing ended."}
          </p>
        )}
        {core && core.phase !== "ended" && (
          <>
            <PinPanel core={core} />
            <div className="flex flex-wrap gap-2">
              {!core.capture && (
                <>
                  <Btn tone="stamp" testid="share" onClick={() => void share(false)}>
                    Share screen, window or tab
                  </Btn>
                  {SENDER_TEST_HOOKS && (
                    <Btn testid="test-source" onClick={() => void share(true)}>
                      Start test source
                    </Btn>
                  )}
                </>
              )}
              <Btn tone="ghost" testid="copy-link" onClick={() => void navigator.clipboard?.writeText(core.link)}>
                Copy link
              </Btn>
              {l?.paused ? (
                <Btn testid="resume" onClick={() => act("resume", {})}>
                  Resume
                </Btn>
              ) : (
                <Btn testid="pause" onClick={() => act("pause", {})}>
                  Pause · Esc
                </Btn>
              )}
              <Btn tone="ghost" testid="lock" onClick={() => act("lock", { locked: !l?.locked })}>
                {l?.locked ? "Unlock" : "Lock"}
              </Btn>
              {l?.frozen && (
                <Btn tone="ghost" testid="unfreeze" onClick={() => act("unfreeze", {})}>
                  Unfreeze (new PIN)
                </Btn>
              )}
              <Btn tone="ghost" testid="mode" onClick={() => act("mode", { mode: core.mode === "text" ? "motion" : "text" })}>
                {core.mode === "text" ? "Text mode" : "Motion mode"}
              </Btn>
              <Btn tone="ghost" testid="safe" onClick={() => act("safe", { on: !core.safe })}>
                {core.safe ? "Safe mode: on" : "Safe mode: off"}
              </Btn>
              <Btn tone="ghost" testid="stop" onClick={() => act("stop", {})}>
                Stop
              </Btn>
            </div>
            <CurtainBanner core={core} act={act} />
            <Preview core={core} act={act} />
            <SenderHudPanel core={core} />
            {core.host.revocationNotDurable && <p className="text-stamp">Storage failed: media is stopped and joining is refused until it recovers.</p>}
            {core.recovery.state !== "idle" && <p className="text-warn">PIN change pending ({core.recovery.state}).</p>}
            <Knocks core={core} act={act} />
            <Viewers core={core} act={act} />
          </>
        )}
        {message && (
          <p role="alert" className="text-[0.85rem] text-stamp">
            {message}
          </p>
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
