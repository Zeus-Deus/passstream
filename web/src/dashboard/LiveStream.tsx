import { useCallback, useEffect, useState } from "react";
import { LOCAL_ONLY, runCommand, type RemoteOp } from "./actions";
import { api, ApiError } from "./api";
import { useEventFeed } from "./events";
import { ago, dateTime, endReason, expiry, kbps, phaseOf, pinSummary, rowTitle } from "./format";
import { href, Link } from "./router";
import type { Alert, Stream, Viewer, ViewerList } from "./types";
import { Badge, Empty, IconLock, IconWarn, Loading, Notice, PageHead, RowCodeChip } from "./ui";
import { ViewerTable } from "./ViewerTable";

export function useNow(ms = 1000) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

interface FailedBatch {
  count: number;
  sources: number;
  from_at: number;
  to_at: number;
}

/** Failed-PIN alerts arrive batched by the sender; merge them for display. */
export function mergeFailed(prev: FailedBatch | null, a: Extract<Alert, { kind: "failed_pin" }>): FailedBatch {
  if (!prev) return { count: a.count, sources: a.sources, from_at: a.from_at, to_at: a.to_at };
  return {
    count: prev.count + a.count,
    // Sources can't be summed across batches: the max is a lower bound.
    sources: Math.max(prev.sources, a.sources),
    from_at: Math.min(prev.from_at, a.from_at),
    to_at: Math.max(prev.to_at, a.to_at),
  };
}

type Flash = { tone: "ok" | "stamp" | "warn"; text: string } | null;

function Disabled({ label }: { label: string }) {
  return (
    <button type="button" className="btn" disabled title={LOCAL_ONLY}>
      {label}
    </button>
  );
}

export function LiveStream({ id, onSignedOut }: { id: string; onSignedOut: () => void }) {
  const now = useNow();
  const [stream, setStream] = useState<Stream | null>(null);
  const [list, setList] = useState<ViewerList | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [failed, setFailed] = useState<FailedBatch | null>(null);
  const [notes, setNotes] = useState<{ key: string; tone: "warn" | "stamp" | "mute"; text: string }[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [flash, setFlash] = useState<Flash>(null);
  const [confirmStop, setConfirmStop] = useState(false);

  useEffect(() => {
    let off = false;
    Promise.all([api.stream(id), api.viewers(id)]).then(
      ([s, v]) => {
        if (off) return;
        setStream(s);
        setList(v);
      },
      (e: ApiError) => !off && setError(e.message),
    );
    return () => {
      off = true;
    };
  }, [id]);

  const note = (key: string, tone: "warn" | "stamp" | "mute", text: string) =>
    setNotes((n) => [{ key, tone, text }, ...n.filter((x) => x.key !== key)].slice(0, 6));

  const feed = useEventFeed(id, {
    stream: setStream,
    viewers: setList,
    alert: (a) => {
      switch (a.kind) {
        case "failed_pin":
          setFailed((p) => mergeFailed(p, a));
          break;
        case "frozen":
          note("frozen", "stamp", `New admissions are frozen after ${a.failures} wrong PINs. Unfreeze on the host machine.`);
          break;
        case "unfrozen":
          setNotes((n) => n.filter((x) => x.key !== "frozen"));
          note("unfrozen", "mute", a.pin_rotated ? "Unfrozen with a new PIN." : "Unfrozen.");
          break;
        case "state_mismatch":
          note("mismatch", "stamp", "The sender's PIN state didn't reconcile; it locked and froze the stream. Fix it on the host machine.");
          break;
        case "revocation_durability":
          if (a.not_durable) note("durability", "warn", "The sender couldn't save a revocation durably. A reload could readmit a kicked viewer; consider stopping.");
          else setNotes((n) => n.filter((x) => x.key !== "durability"));
          break;
        default:
          break; // joined/left/knock are reflected by the viewer snapshots
      }
    },
    ended: () => api.stream(id).then(setStream, () => undefined),
    beforeRetry: async () => {
      try {
        await api.session();
        return true;
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) {
          onSignedOut();
          return false;
        }
        return true;
      }
    },
  });

  const send = useCallback(
    async (op: RemoteOp, args: Record<string, unknown>, rowId: string, done: string) => {
      setBusy(rowId);
      setFlash(null);
      const r = await runCommand(id, op, args);
      setBusy(null);
      if (r.ok) setFlash({ tone: "ok", text: done });
      else if (r.code === "local_owner_required") setFlash({ tone: "warn", text: LOCAL_ONLY });
      else if (r.details?.locked === true) setFlash({ tone: "warn", text: `Locked, but the kick failed: ${r.message}` });
      else setFlash({ tone: "stamp", text: r.message });
      if (r.ok && op === "stop") api.stream(id).then(setStream, () => undefined);
      return r.ok;
    },
    [id],
  );

  if (error)
    return (
      <>
        <PageHead kicker="Stream" title="Can't load this stream" />
        <Notice tone="stamp" role="alert">{error}</Notice>
        <p className="mt-5"><Link className="btn" to={href({ name: "streams" })}>All streams</Link></p>
      </>
    );
  if (!stream || !list) return <Loading />;

  const ph = phaseOf(stream);
  const eff = stream.effective;
  const access = eff?.access;
  const latches = access?.latches;
  const ended = stream.ended_at !== null || stream.phase === "ended";
  const pin = pinSummary(access?.pin ?? stream.declared.pin);
  const knocks = list.viewers.filter((v) => v.status === "knocking");
  const online = stream.sender.online;
  const title = stream.label || `Stream ${stream.id.slice(0, 6)}`;

  const kick = (v: Viewer, lock: boolean) =>
    v.admission_ids[0] &&
    send("kick", lock ? { admission_id: v.admission_ids[0], lock: true } : { admission_id: v.admission_ids[0] }, v.row_id, lock ? `Kicked ${rowTitle(v)} and locked the stream.` : `Kicked ${rowTitle(v)}.`);
  const ban = (v: Viewer) =>
    v.connections[0] && send("ban", { connection_id: v.connections[0].connection_id }, v.row_id, `Banned ${rowTitle(v)}'s IP for this stream.`);

  return (
    <>
      <PageHead
        kicker={ended ? "Ended stream" : "Live stream"}
        title={title}
        aside={
          <div className="flex flex-wrap items-center gap-2" aria-live="polite">
            <Badge tone={ph.tone}>{ph.live && ph.text === "Live" && <span className="size-1.5 animate-breathe rounded-full bg-stamp" />}{ph.text.toUpperCase()}</Badge>
            {!ended && (
              <span className="num text-[0.78rem] text-ink-mute" data-testid="feed-status">
                {feed.s === "open" ? "● live updates" : feed.s === "retrying" ? `reconnecting${feed.retryIn ? ` in ${Math.ceil(feed.retryIn / 1000)}s` : ""}…` : feed.s === "connecting" ? "connecting…" : ""}
              </span>
            )}
          </div>
        }
      >
        <span className="num text-[0.82rem]">
          {stream.watching} watching{stream.knocking ? ` · ${stream.knocking} knocking` : ""} · started {dateTime(stream.created_at)}
          {expiry(stream, now) && ` · ${expiry(stream, now)}`}
          {ended && stream.end_reason && ` · ${endReason(stream.end_reason)}`}
        </span>
      </PageHead>

      {!ended && <p className="mb-5 text-sm text-ink-mute">
        <a className="underline" href={`http://127.0.0.1:47823/owner?stream_id=${encodeURIComponent(id)}`} target="_blank" rel="noreferrer">Snapshot → PassPage / PassFile · record locally</a>
        {" · Open on the host machine. A custom daemon port needs the matching local URL."}
      </p>}

      {!ended && (
        <div className="mb-5">
          <Notice>
            This view is relayed by the server. The bar plugin shows the sender's own view. To check who someone is, ask them for their row code.
          </Notice>
        </div>
      )}

      <div className="space-y-3" aria-live="polite">
        {flash && <Notice tone={flash.tone} role="status">{flash.text}</Notice>}
        {!ended && list.sender_stale && <Notice tone="warn">The sender hasn't reported for a while. Sender facts may be out of date.</Notice>}
        {!ended && !online && stream.phase !== "pending" && <Notice tone="warn">The sender is offline. Commands other than Stop need it online.</Notice>}
        {failed && (
          <Notice tone="stamp" role="alert">
            <span className="inline-flex items-center gap-2 font-medium">
              <IconWarn className="size-4 shrink-0" />
              {failed.count} wrong {failed.count === 1 ? "PIN" : "PINs"} from {failed.sources}
              {failed.sources > 1 ? "+ sources" : " source"} · {ago(failed.to_at, now)}
            </span>
            <button type="button" className="ml-3 underline" onClick={() => setFailed(null)}>Dismiss</button>
          </Notice>
        )}
        {notes.map((n) => <Notice key={n.key} tone={n.tone}>{n.text}</Notice>)}
      </div>

      <section className="mt-6 grid gap-4 lg:grid-cols-[1.3fr_1fr]" aria-label="Access and controls">
        <div className="sheet p-4 md:p-5">
          <h2 className="label">Access</h2>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <span className="inline-flex items-center gap-1.5 font-medium">
              {pin.tone !== "warn" && <IconLock className="size-4" />}
              <span className={pin.tone === "warn" ? "text-warn" : ""}>{pin.text}</span>
            </span>
            {latches?.locked && <Badge tone="warn">LOCKED</Badge>}
            {latches?.paused && <Badge tone="warn">PAUSED</Badge>}
            {latches?.frozen && <Badge tone="stamp">FROZEN</Badge>}
            {access?.knock && <Badge>KNOCK TO JOIN</Badge>}
            {access?.rotation_pending && <Badge tone="warn">PIN CHANGE PENDING</Badge>}
            {!access?.rotation_pending && access?.reconciling && <Badge tone="warn">RECONCILING</Badge>}
          </div>
          <p className="mt-2 text-[0.82rem] text-ink-mute">
            {access && access.failures.limit > 0 ? `Wrong PINs ${access.failures.stream}/${access.failures.limit} · ` : ""}
            The PIN is checked by the sender and never reaches the server. Link is on the host machine.
          </p>
          {!eff && <p className="mt-1 text-[0.82rem] text-ink-mute">Declared at creation (the sender hasn't reported yet).</p>}

          {!ended && (
            <div className="mt-4 flex flex-wrap gap-2 border-t border-paper-edge pt-4" data-testid="stream-actions">
              {latches?.paused || stream.phase === "paused" ? (
                <Disabled label="Resume" />
              ) : (
                <button type="button" className="btn" disabled={busy !== null} onClick={() => send("pause", {}, "stream", "Paused. Resume on the host machine.")}>
                  Pause
                </button>
              )}
              {latches?.locked ? (
                <Disabled label="Unlock" />
              ) : (
                <button type="button" className="btn" disabled={busy !== null} onClick={() => send("lock", { locked: true }, "stream", "Locked: no new viewers.")}>
                  Lock
                </button>
              )}
              {latches?.frozen && <Disabled label="Unfreeze" />}
              <Disabled label="Rotate PIN" />
              {eff && eff.mode !== "unknown" && (
                <button
                  type="button"
                  className="btn"
                  disabled={busy !== null}
                  onClick={() => send("mode", { mode: eff.mode === "text" ? "motion" : "text" }, "stream", `Switched to ${eff.mode === "text" ? "motion" : "text"} mode.`)}
                >
                  {eff.mode === "text" ? "Switch to motion" : "Switch to text"}
                </button>
              )}
              {confirmStop ? (
                <span className="inline-flex gap-2">
                  <button type="button" className="btn btn-stamp" disabled={busy !== null} onClick={() => { setConfirmStop(false); void send("stop", {}, "stream", "Stopped the stream."); }}>
                    Stop for everyone
                  </button>
                  <button type="button" className="btn-quiet" onClick={() => setConfirmStop(false)}>Cancel</button>
                </span>
              ) : (
                <button type="button" className="btn btn-danger border-stamp" disabled={busy !== null} onClick={() => setConfirmStop(true)}>
                  Stop
                </button>
              )}
              <p className="basis-full text-[0.76rem] text-ink-mute">
                Disabled buttons loosen access. {LOCAL_ONLY}
              </p>
            </div>
          )}
        </div>
        <SenderHealth stream={stream} now={now} />
      </section>

      {!ended && (
        <section className="mt-8" aria-labelledby="knocks-h">
          <h2 id="knocks-h" className="label mb-3">Waiting to join · {knocks.length}</h2>
          {knocks.length === 0 ? (
            <p className="text-[0.86rem] text-ink-mute">{access?.knock ? "No one is knocking." : "Knock to join is off for this stream."}</p>
          ) : (
            <ul className="space-y-2" data-testid="knocks">
              {knocks.map((k) => (
                <li key={k.row_id} className="sheet flex flex-wrap items-center gap-3 px-3.5 py-3">
                  <RowCodeChip code={k.row_code} />
                  <span className="min-w-0 flex-1">
                    <span className="block font-medium">{k.connections[0]?.reported.label ? `${k.connections[0].reported.label} wants to watch` : "Someone wants to watch"}</span>
                    <span className="block text-[0.78rem] text-ink-mute">{[k.connections[0]?.device, k.connections[0]?.geo?.city].filter(Boolean).join(" · ") || "no connection details"}</span>
                  </span>
                  <span className="text-[0.78rem] text-ink-mute" title={LOCAL_ONLY}>Approve on your computer (plugin/CLI)</span>
                  <button
                    type="button"
                    className="btn btn-danger min-h-9 border-stamp/50 py-1"
                    disabled={busy !== null || !k.knock_id}
                    onClick={() => k.knock_id && send("knock.deny", { knock_id: k.knock_id }, k.row_id, `Denied ${k.row_code ?? "the knock"}.`)}
                  >
                    Deny
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>
      )}

      <section className="mt-8" aria-labelledby="viewers-h">
        <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
          <h2 id="viewers-h" className="label">Viewers · {list.watching} watching</h2>
          <Link className="btn-quiet" to={href({ name: "history", stream: id })}>History & audit</Link>
        </div>
        {list.viewers.filter((v) => v.status !== "knocking").length === 0 ? (
          <Empty>{ended ? "No one is left on this stream." : "Nobody yet. The link and PIN are on the host machine; send them separately."}</Empty>
        ) : (
          <ViewerTable viewers={list.viewers} now={now} onKick={kick} onBan={ban} busy={busy} ended={ended} />
        )}
        <p className="mt-3 text-[0.76rem] text-ink-mute">
          Count sessions, not people. Names and tab visibility are viewer-reported; IP, place and device are what the server observed and can be a VPN or NAT exit.
          {list.meta?.geoip_attribution && <> {list.meta.geoip_attribution}.</>}
        </p>
      </section>
    </>
  );
}

function SenderHealth({ stream, now }: { stream: Stream; now: number }) {
  const eff = stream.effective;
  const h = eff?.health;
  const rows: [string, string][] = [
    ["Sender", stream.sender.online ? `online · ${stream.sender.kind}` : `offline · ${stream.sender.kind}`],
    ["Last report", stream.sender.last_seen_at ? ago(stream.sender.last_seen_at, now) : "—"],
    ["Mode", eff?.mode ?? "—"],
    ["E2EE", eff ? `${eff.e2ee.status === "on" ? "on" : eff.e2ee.status === "beta" ? "beta" : "unknown"} · epoch ${eff.e2ee.epoch}${eff.e2ee.suspended ? " · suspended" : ""}` : stream.e2ee ? "expected (beta)" : "off"],
  ];
  if (h) {
    rows.push(["Uplink", kbps(h.uplink_kbps)]);
    rows.push(["Encode", h.encode_ms != null ? `${h.encode_ms.toFixed(1)} ms/frame` : "—"]);
    rows.push(["Encode CPU", h.encode_cpu_pct != null ? `${h.encode_cpu_pct.toFixed(1)}%` : "—"]);
    rows.push(["Resolution", h.width != null && h.height != null ? `${h.width} × ${h.height}` : "—"]);
    rows.push(["Frame rate", h.fps != null ? `${h.fps.toFixed(0)} fps` : "—"]);
    rows.push(["Limited by", h.quality_limitation]);
  }
  return (
    <div className="sheet p-4 md:p-5" data-testid="sender-health">
      <h2 className="label">Sender health</h2>
      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-[0.86rem]">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-ink-mute">{k}</dt>
            <dd className={`num text-[0.82rem] ${k === "Limited by" && v !== "none" ? "text-warn" : ""}`}>{v}</dd>
          </div>
        ))}
      </dl>
      {eff?.e2ee.revocation_not_durable && <p className="mt-2 text-[0.8rem] text-warn">A revocation isn't durable yet.</p>}
      {!h && <p className="mt-3 text-[0.76rem] text-ink-mute">Waiting for sender health. Metrics are available after capture starts.</p>}
      <p className="mt-1 text-[0.72rem] text-ink-mute">Reported by the sender, via the server.</p>
    </div>
  );
}
