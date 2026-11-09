import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError } from "./api";
import { dateTime } from "./format";
import type { Key, PinPolicy, Settings as S } from "./types";
import { Badge, Loading, Notice, PageHead } from "./ui";

const EXPIRY: [string, number | null][] = [
  ["Never", null],
  ["1 hour", 3600],
  ["4 hours", 4 * 3600],
  ["12 hours", 12 * 3600],
  ["24 hours", 24 * 3600],
  ["7 days", 7 * 86400],
];

function pinValue(p: PinPolicy): string {
  return p.kind === "digits" ? `digits:${p.length}` : p.kind;
}
function pinFrom(v: string): PinPolicy {
  if (v.startsWith("digits:")) return { kind: "digits", length: Number(v.slice(7)) };
  return v === "passphrase" ? { kind: "passphrase" } : { kind: "none" };
}

function Defaults({ owner }: { owner: string | null }) {
  const [s, setS] = useState<S | null>(null);
  const [saved, setSaved] = useState<S | null>(null);
  const [msg, setMsg] = useState<{ tone: "ok" | "stamp"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    api.settings().then(
      (v) => {
        setS(v);
        setSaved(v);
      },
      (e: ApiError) => setMsg({ tone: "stamp", text: e.message }),
    );
  }, []);
  if (!s) return msg ? <Notice tone="stamp">{msg.text}</Notice> : <Loading />;
  const d = s.defaults;
  const set = (p: Partial<S["defaults"]>) => setS({ ...s, defaults: { ...d, ...p } });
  // Turning IP retention on is owner-only and needs the owner key itself.
  const needsOwner = s.retention.keep_ip && !saved?.retention.keep_ip;
  const expiryOptions = EXPIRY.some(([, v]) => v === d.expires_in_s) ? EXPIRY : [...EXPIRY, [`${Math.round((d.expires_in_s ?? 0) / 3600)} hours`, d.expires_in_s] as [string, number | null]];
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      const v = await api.putSettings(s, needsOwner ? owner ?? undefined : undefined);
      setS(v);
      setSaved(v);
      setMsg({ tone: "ok", text: "Saved. New defaults apply to streams created from now on." });
    } catch (err) {
      setMsg({ tone: "stamp", text: (err as Error).message });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} className="sheet space-y-5 p-5 md:p-6" aria-labelledby="defaults-h">
      <div>
        <h2 id="defaults-h" className="display text-[1.5rem]">Defaults for new streams</h2>
        <p className="mt-1 text-[0.86rem] text-ink-mute">They apply only when a stream is created and never change a live one. A local choice on the host machine (plugin or CLI) can still pick its own PIN.</p>
      </div>
      <div className="grid gap-5 sm:grid-cols-2">
        <label className="block">
          <span className="label mb-2 block">PIN</span>
          <select className="field" value={pinValue(d.pin)} onChange={(e) => set({ pin: pinFrom(e.target.value) })} data-testid="pin-default">
            {[4, 5, 6, 8, 10, 12].map((n) => (
              <option key={n} value={`digits:${n}`}>{n} digits{n === 4 ? " (default)" : ""}</option>
            ))}
            <option value="passphrase">Passphrase</option>
            <option value="none">No PIN</option>
          </select>
          {d.pin.kind === "none" && <span className="mt-1.5 block text-[0.8rem] text-warn">Anyone with the link can watch. Not recommended.</span>}
        </label>
        <label className="block">
          <span className="label mb-2 block">Max viewers</span>
          <input className="field num" type="number" inputMode="numeric" min={1} max={500} value={d.max_viewers} onChange={(e) => set({ max_viewers: Math.max(1, Math.min(500, Number(e.target.value) || 1)) })} data-testid="max-viewers" />
        </label>
        <label className="block">
          <span className="label mb-2 block">Expiry</span>
          <select className="field" value={String(d.expires_in_s)} onChange={(e) => set({ expires_in_s: e.target.value === "null" ? null : Number(e.target.value) })}>
            {expiryOptions.map(([t, v]) => <option key={String(v)} value={String(v)}>{t}</option>)}
          </select>
        </label>
        <label className="flex min-h-10 items-center gap-3 self-end">
          <input type="checkbox" className="size-5 accent-stamp" checked={d.knock} onChange={(e) => set({ knock: e.target.checked })} />
          <span>
            <span className="block font-medium">Knock to join</span>
            <span className="block text-[0.8rem] text-ink-mute">Viewers wait for approval on the host machine.</span>
          </span>
        </label>
      </div>
      <fieldset className="border-t border-paper-edge pt-5">
        <legend className="label mb-2">Retention</legend>
        <label className="flex items-start gap-3">
          <input type="checkbox" className="mt-0.5 size-5 accent-stamp" checked={s.retention.keep_ip} onChange={(e) => setS({ ...s, retention: { keep_ip: e.target.checked } })} data-testid="keep-ip" />
          <span>
            <span className="block font-medium">Keep viewer IP addresses in history</span>
            <span className="block text-[0.8rem] text-ink-mute">Off by default: raw IPs last for the session only; history keeps country, city and device. Turning this on needs the owner key (unlock key management below).</span>
          </span>
        </label>
        {needsOwner && !owner && <p className="mt-2 text-[0.8rem] text-warn">Unlock key management with the owner key to turn this on.</p>}
      </fieldset>
      {msg && <Notice tone={msg.tone} role="status">{msg.text}</Notice>}
      <button className="btn btn-primary" disabled={busy || (needsOwner && !owner)}>
        {busy ? "Saving…" : "Save defaults"}
      </button>
    </form>
  );
}

function Keys({ owner, setOwner }: { owner: string | null; setOwner: (k: string | null) => void }) {
  const [typed, setTyped] = useState("");
  const [keys, setKeys] = useState<Key[] | null>(null);
  const [msg, setMsg] = useState<{ tone: "ok" | "stamp" | "warn"; text: string } | null>(null);
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState<Key["scope"]>("read");
  const [fresh, setFresh] = useState<{ label: string; key: string } | null>(null);
  const load = async (k: string) => {
    try {
      const r = await api.keys(k);
      setKeys(r.keys);
      setOwner(k);
      setMsg(null);
    } catch (e) {
      setOwner(null);
      setKeys(null);
      setMsg({ tone: "stamp", text: e instanceof ApiError && e.code === "forbidden_scope" ? "That key doesn't have owner scope." : (e as Error).message });
    }
  };
  if (!owner || !keys)
    return (
      <form
        className="sheet space-y-3 p-5 md:p-6"
        onSubmit={(e) => {
          e.preventDefault();
          const k = typed.trim();
          setTyped("");
          void load(k);
        }}
      >
        <h2 className="display text-[1.5rem]">API keys</h2>
        <p className="text-[0.86rem] text-ink-mute">
          Key management needs the owner key itself, not this browser session. It's used for these requests only, kept in this tab's memory, and dropped when you leave Settings.
        </p>
        <input className="field num" type="password" autoComplete="off" spellCheck={false} placeholder="Owner key (DATA/owner-key)" value={typed} onChange={(e) => setTyped(e.target.value)} aria-label="Owner key" />
        {msg && <Notice tone={msg.tone} role="alert">{msg.text}</Notice>}
        <button className="btn" disabled={!typed.trim()}>Unlock key management</button>
      </form>
    );
  const create = async (e: FormEvent) => {
    e.preventDefault();
    try {
      const k = await api.createKey(owner, { label: label.trim(), scope, expires_in_s: null });
      setFresh({ label: k.label, key: k.key });
      setLabel("");
      await load(owner);
    } catch (err) {
      setMsg({ tone: "stamp", text: (err as Error).message });
    }
  };
  const remove = async (k: Key) => {
    try {
      await api.deleteKey(owner, k.id);
      await load(owner);
      setMsg({ tone: "ok", text: `Deleted “${k.label}”.` });
    } catch (err) {
      setMsg({ tone: "stamp", text: err instanceof ApiError && err.code === "conflict" ? "You can't delete the last owner key." : (err as Error).message });
    }
  };
  return (
    <section className="sheet space-y-4 p-5 md:p-6" aria-labelledby="keys-h">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 id="keys-h" className="display text-[1.5rem]">API keys</h2>
        <button type="button" className="btn-quiet" onClick={() => { setOwner(null); setKeys(null); setFresh(null); }}>Lock again</button>
      </div>
      {fresh && (
        <Notice tone="ok" role="status">
          <span className="block font-medium">New key “{fresh.label}”. It's shown once; store it now.</span>
          <code className="num mt-1.5 block rounded-sm bg-paper px-2 py-1.5 text-[0.78rem] break-all text-ink" data-testid="new-key">{fresh.key}</code>
          <button type="button" className="mt-2 underline" onClick={() => void navigator.clipboard?.writeText(fresh.key)}>Copy</button>{" · "}
          <button type="button" className="underline" onClick={() => setFresh(null)}>Done</button>
        </Notice>
      )}
      {msg && <Notice tone={msg.tone} role="status">{msg.text}</Notice>}
      <ul className="divide-y divide-paper-edge border-y border-paper-edge" data-testid="keys">
        {keys.map((k) => (
          <li key={k.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2.5 text-[0.88rem]">
            <span className="min-w-0 flex-1 font-medium">{k.label}</span>
            <Badge tone={k.scope === "owner" ? "stamp" : k.scope === "streams" ? "warn" : "mute"}>{k.scope.toUpperCase()}</Badge>
            <span className="num text-[0.74rem] text-ink-mute">
              {k.last_used_at ? `used ${dateTime(k.last_used_at)}` : "never used"}
              {k.expires_at ? ` · expires ${dateTime(k.expires_at)}` : ""}
            </span>
            <button type="button" className="btn-quiet btn-danger" onClick={() => void remove(k)}>Delete</button>
          </li>
        ))}
      </ul>
      <form onSubmit={create} className="flex flex-wrap items-end gap-3">
        <label className="min-w-[12rem] flex-1">
          <span className="label mb-2 block">Label</span>
          <input className="field" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. laptop agent" maxLength={64} />
        </label>
        <label>
          <span className="label mb-2 block">Scope</span>
          <select className="field" value={scope} onChange={(e) => setScope(e.target.value as Key["scope"])}>
            <option value="read">read: view only</option>
            <option value="streams">streams: create, stop, tighten</option>
            <option value="owner">owner: plus keys</option>
          </select>
        </label>
        <button className="btn btn-primary" disabled={!label.trim()}>Create key</button>
      </form>
    </section>
  );
}

function Snippets() {
  const base = location.origin;
  const snip = [
    ["Read the agent contract", `curl -s ${base}/llms.txt`],
    ["List live streams", `curl -s -H "Authorization: Bearer $PASSSTREAM_KEY" \\\n  "${base}/api/v1/streams?state=live"`],
    ["Watch a stream's viewers (SSE)", `curl -sN -H "Authorization: Bearer $PASSSTREAM_KEY" \\\n  ${base}/api/v1/streams/$STREAM/events`],
    ["Pause a stream (restrictive)", `curl -s -X POST -H "Authorization: Bearer $PASSSTREAM_KEY" \\\n  -H 'Content-Type: application/json' \\\n  -d '{"id":"pause-1","op":"pause","args":{}}' \\\n  ${base}/api/v1/streams/$STREAM/commands`],
  ];
  return (
    <section className="sheet space-y-4 p-5 md:p-6" aria-labelledby="agents-h">
      <div>
        <h2 id="agents-h" className="display text-[1.5rem]">Agent snippets</h2>
        <p className="mt-1 text-[0.86rem] text-ink-mute">
          Give agents a scoped key in <span className="num">$PASSSTREAM_KEY</span>. Remote agents can watch and tighten, never approve, unlock or change a PIN, and must not pick a person from IP, place or device.
        </p>
      </div>
      {snip.map(([t, code]) => (
        <div key={t}>
          <p className="label mb-1.5">{t}</p>
          <pre className="num overflow-x-auto rounded-sm bg-ink px-3 py-2.5 text-[0.74rem] leading-relaxed text-paper"><code>{code}</code></pre>
        </div>
      ))}
    </section>
  );
}

export function Settings() {
  // The owner key lives only in this component's state.
  const [owner, setOwner] = useState<string | null>(null);
  return (
    <>
      <PageHead kicker="Settings" title="Defaults, keys and agents." />
      <div className="space-y-6">
        <Defaults owner={owner} />
        <Keys owner={owner} setOwner={setOwner} />
        <Snippets />
      </div>
    </>
  );
}
