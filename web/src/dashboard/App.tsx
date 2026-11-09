import { useEffect, useState, type ComponentType, type FormEvent, type SVGProps } from "react";
import { api, ApiError, onSignedOut } from "./api";
import { History } from "./History";
import { LiveStream } from "./LiveStream";
import { href, Link, useRoute, type Route } from "./router";
import { Settings } from "./Settings";
import { Streams } from "./Streams";
import type { SessionInfo } from "./types";
import { IconHistory, IconSettings, IconStreams, Mark } from "./ui";

const NAV: [Route["name"], string, string, ComponentType<SVGProps<SVGSVGElement>>][] = [
  ["streams", "Streams", href({ name: "streams" }), IconStreams],
  ["history", "History & audit", href({ name: "history", stream: null }), IconHistory],
  ["settings", "Settings", href({ name: "settings" }), IconSettings],
];

export function Login({ onLogin }: { onLogin: (s: SessionInfo) => void }) {
  const [key, setKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    // The key goes out once as a bearer header and is then dropped (CONTRACT §7).
    const typed = key.trim();
    setKey("");
    try {
      await api.login(typed);
      onLogin(await api.session());
    } catch (err) {
      setError(err instanceof ApiError && err.code === "unauthorized" ? "That key isn't valid." : (err as Error).message);
      setBusy(false);
    }
  };
  return (
    <main id="main" className="grid min-h-dvh place-items-center px-5 py-10">
      <div className="w-full max-w-md">
        <div className="mb-8 flex items-center gap-3">
          <Mark className="size-10" />
          <span className="display text-[1.7rem] font-semibold">PassStream</span>
        </div>
        <form onSubmit={submit} className="sheet animate-rise p-6 md:p-8">
          <h1 className="display text-[2rem] leading-tight">Sign in with a key.</h1>
          <p className="mt-2 text-ink-soft">
            Paste an API key. It's swapped for a browser session once and never stored here.
          </p>
          <label className="label mt-6 mb-2 block" htmlFor="key">API key</label>
          <input
            id="key"
            className="field num"
            type="password"
            autoComplete="off"
            spellCheck={false}
            value={key}
            onChange={(e) => setKey(e.target.value)}
            aria-invalid={!!error}
            aria-describedby={error ? "login-error" : "login-hint"}
            required
            autoFocus
          />
          {error && <p id="login-error" role="alert" className="mt-2 text-sm font-medium text-stamp">{error}</p>}
          <button className="btn btn-primary mt-5 min-h-12 w-full" disabled={busy || !key.trim()}>
            {busy ? "Signing in…" : "Sign in"}
          </button>
          <p id="login-hint" className="mt-5 border-t border-paper-edge pt-4 text-[0.82rem] text-ink-mute">
            First run? The server wrote an owner key to <span className="num">DATA/owner-key</span>. A browser session never gets owner scope: key management asks for the key again.
          </p>
        </form>
      </div>
    </main>
  );
}

export function App() {
  const [me, setMe] = useState<SessionInfo | null | undefined>(undefined);
  useEffect(() => {
    api.session().then(setMe, () => setMe(null));
    return onSignedOut(() => setMe(null));
  }, []);
  if (me === undefined) return <div className="min-h-dvh" aria-busy="true" />;
  if (me === null) return <Login onLogin={setMe} />;
  return <Shell me={me} onSignedOut={() => setMe(null)} />;
}

function Shell({ me, onSignedOut }: { me: SessionInfo; onSignedOut: () => void }) {
  const route = useRoute();
  const [err, setErr] = useState<string | null>(null);
  const active = route.name === "stream" ? "streams" : route.name;
  const logout = async () => {
    try {
      await api.logout();
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 401)) {
        setErr(`Sign-out failed (${(e as Error).message}). You're still signed in.`);
        return;
      }
    }
    onSignedOut();
  };
  return (
    <div className="min-h-dvh pb-[calc(4.75rem+env(safe-area-inset-bottom))] md:pb-0">
      <a href="#main" className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-50 focus:rounded-md focus:bg-ink focus:px-3 focus:py-2 focus:text-paper">
        Skip to content
      </a>
      <header className="sticky top-0 z-40 border-b border-paper-edge bg-paper/90 pt-[env(safe-area-inset-top)] backdrop-blur-sm">
        <div className="mx-auto flex h-16 max-w-6xl items-center gap-8 px-4 sm:px-5 md:px-8">
          <Link to={href({ name: "streams" })} className="flex items-center gap-2.5" aria-label="PassStream, streams">
            <Mark className="size-7" />
            <span className="display text-[1.3rem] font-semibold tracking-tight">PassStream</span>
          </Link>
          <nav aria-label="Main" className="hidden h-full items-stretch gap-1 md:flex">
            {NAV.map(([name, label, to]) => (
              <Link
                key={name}
                to={to}
                aria-current={active === name ? "page" : undefined}
                className={`relative flex items-center px-3 text-[0.92rem] font-medium transition-colors ${active === name ? "text-ink" : "text-ink-mute hover:text-ink"}`}
              >
                {label}
                {active === name && <span className="absolute inset-x-3 -bottom-px h-0.5 bg-stamp" />}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3">
            <span className="hidden text-[0.8rem] text-ink-mute lg:inline">
              <span className="mr-1.5 inline-block size-1.5 -translate-y-px rounded-full bg-ok" />
              {me.scope} session
            </span>
            <button type="button" className="btn-quiet" onClick={logout}>Sign out</button>
          </div>
        </div>
      </header>
      <main id="main" className="mx-auto max-w-6xl px-4 pt-7 pb-16 sm:px-5 md:px-8 md:pt-12" key={route.name + (route.name === "stream" ? route.id : "")}>
        {err && <p role="alert" className="mb-4 text-sm text-stamp">{err}</p>}
        <div className="animate-rise">
          {route.name === "streams" && <Streams />}
          {route.name === "stream" && <LiveStream id={route.id} onSignedOut={onSignedOut} />}
          {route.name === "history" && <History stream={route.stream} />}
          {route.name === "settings" && <Settings />}
        </div>
      </main>
      <footer className="mx-auto hidden max-w-6xl px-8 pb-10 text-[0.78rem] text-ink-mute md:block">
        The dashboard can watch and tighten. Approving, unlocking and PIN changes happen on the host machine. <a className="underline" href="/privacy">Viewer privacy notice</a>.
      </footer>
      <nav aria-label="Main" className="fixed inset-x-0 bottom-0 z-40 border-t border-paper-edge bg-paper/95 pb-[env(safe-area-inset-bottom)] backdrop-blur-sm md:hidden">
        <div className="grid grid-cols-3">
          {NAV.map(([name, label, to, Icon]) => (
            <Link
              key={name}
              to={to}
              aria-current={active === name ? "page" : undefined}
              className={`relative flex h-[4.25rem] flex-col items-center justify-center gap-1 text-[0.7rem] font-medium ${active === name ? "text-ink" : "text-ink-mute"}`}
            >
              {active === name && <span className="absolute inset-x-6 top-0 h-0.5 bg-stamp" />}
              <Icon className="size-[1.35rem]" />
              {label}
            </Link>
          ))}
        </div>
      </nav>
    </div>
  );
}
