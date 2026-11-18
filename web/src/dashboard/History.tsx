import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import { dateTime, durationShort, place } from "./format";
import { href, Link, navigate } from "./router";
import type { AuditEntry, HistoryEntry, Stream } from "./types";
import { Badge, Empty, Loading, Notice, PageHead, RowCodeChip } from "./ui";
import type { Page } from "./types";

const END: Record<string, string> = {
  left: "Left",
  kicked: "Kicked",
  revoked: "Revoked",
  denied: "Denied",
  knock_timeout: "Knock timed out",
  stream_ended: "Stream ended",
};

const OPS: Record<string, string> = {
  "stream.create": "Stream created",
  "stream.delete": "Stream deleted",
  "session.login": "Dashboard sign-in",
  "settings.update": "Settings changed",
  "key.create": "Key created",
  "key.delete": "Key deleted",
  stop: "Stop",
  pause: "Pause",
  mode: "Mode",
  lock: "Lock",
  kick: "Kick",
  ban: "Ban IP",
  "knock.deny": "Deny knock",
};

function usePages<T, S>(load: (stream: S, cursor: string | null) => Promise<Page<T>>, stream: S) {
  const [page, setPage] = useState<Page<T> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const pending = useRef<Promise<Page<T>> | null>(null);
  useEffect(() => {
    let off = false;
    load(stream, null).then(
      (r) => { if (!off) setPage(r); },
      (e: ApiError) => { if (!off) setError(e.message); },
    );
    return () => {
      off = true;
      pending.current = null;
    };
  }, [load, stream]);
  const more = async () => {
    if (!page?.next_cursor || pending.current) return;
    const request = load(stream, page.next_cursor);
    pending.current = request;
    setLoadingMore(true);
    setError(null);
    try {
      const r = await request;
      if (pending.current === request) setPage({ entries: [...page.entries, ...r.entries], next_cursor: r.next_cursor });
    } catch (e) {
      if (pending.current === request) setError(e instanceof Error ? e.message : "Couldn't load more entries.");
    } finally {
      if (pending.current === request) {
        pending.current = null;
        setLoadingMore(false);
      }
    }
  };
  return { rows: page?.entries, next: page?.next_cursor, error, loadingMore, more };
}

function HistoryList({ stream }: { stream: string }) {
  const { rows, next, error, loadingMore, more } = usePages<HistoryEntry, string>(api.history, stream);
  if (!rows) return error ? <Notice tone="stamp" role="alert">{error}</Notice> : <Loading />;
  if (!rows.length) return <Empty>No one has left this stream yet.</Empty>;
  return (
    <>
      <ul className="space-y-2" data-testid="history">
        {rows.map((h) => {
          const c = h.connections[0];
          return (
            <li key={`${h.row_id}-${h.last_at}`} className="sheet flex flex-wrap items-center gap-x-4 gap-y-1.5 px-4 py-3 text-[0.86rem]">
              <RowCodeChip code={h.row_code} />
              <Badge tone={h.end === "kicked" || h.end === "denied" ? "stamp" : "mute"}>{(END[h.end] ?? h.end).toUpperCase()}</Badge>
              <span className="num text-[0.78rem] text-ink-soft">
                {dateTime(h.first_at)} · {durationShort(h.last_at - h.first_at)}
              </span>
              <span className="min-w-0 flex-1 truncate text-ink-mute">
                {[c?.device, place(c?.geo), c?.ip].filter(Boolean).join(" · ") || "no connection"}
                {h.connections.length > 1 && ` · ${h.connections.length} connections`}
              </span>
            </li>
          );
        })}
      </ul>
      {next && <button type="button" className="btn mt-3" onClick={more} disabled={loadingMore}>More</button>}
      {error && <Notice tone="stamp" role="alert">{error}</Notice>}
      <p className="mt-2 text-[0.76rem] text-ink-mute">Newest first. IPs show only when the owner opted in to keep them.</p>
    </>
  );
}

function AuditList({ stream }: { stream: string | null }) {
  const { rows, next, error, loadingMore, more } = usePages<AuditEntry, string | undefined>(api.audit, stream ?? undefined);
  if (!rows) return error ? <Notice tone="stamp" role="alert">{error}</Notice> : <Loading />;
  if (!rows.length) return <Empty>No audit entries.</Empty>;
  return (
    <>
      <div className="sheet overflow-x-auto">
        <table className="w-full min-w-[34rem] text-left text-[0.84rem]" data-testid="audit">
          <thead className="border-b border-paper-edge bg-paper-deep/50">
            <tr>
              <th className="label py-2 pl-4">When</th>
              <th className="label py-2">What</th>
              <th className="label py-2">Target</th>
              <th className="label py-2">Who</th>
              <th className="label py-2 pr-4">Result</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((a) => (
              <tr key={a.id} className="border-b border-paper-edge last:border-0">
                <td className="num py-2 pl-4 text-[0.76rem] whitespace-nowrap text-ink-soft">{dateTime(a.at)}</td>
                <td className="py-2">
                  {OPS[a.op] ?? a.op}
                  {!stream && a.stream_id && (
                    <Link to={href({ name: "stream", id: a.stream_id })} className="num ml-2 text-[0.72rem] text-ink-mute underline">
                      {a.stream_id.slice(0, 6)}
                    </Link>
                  )}
                </td>
                <td className="num py-2 text-[0.72rem] text-ink-mute">
                  {Object.entries(a.target).map(([k, v]) => `${k.replace("_id", "")} ${v.slice(0, 8)}`).join(", ") || "—"}
                </td>
                <td className="num py-2 text-[0.72rem] text-ink-mute">{a.actor.kind}{a.actor.key_id ? ` ${a.actor.key_id.slice(0, 6)}` : ""}</td>
                <td className="py-2 pr-4">
                  {a.result === "ok" ? <Badge tone="ok">OK</Badge> : <Badge tone="stamp">{a.error_code ?? "ERROR"}</Badge>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {next && <button type="button" className="btn mt-3" onClick={more} disabled={loadingMore}>More</button>}
      {error && <Notice tone="stamp" role="alert">{error}</Notice>}
      <p className="mt-2 text-[0.76rem] text-ink-mute">
        The server's audit covers what reaches the server. Commands from the bar plugin, CLI or sender window never pass through it.
      </p>
    </>
  );
}

export function History({ stream }: { stream: string | null }) {
  const [streams, setStreams] = useState<Stream[]>([]);
  useEffect(() => {
    api.streams("all").then((r) => setStreams(r.streams), () => undefined);
  }, []);
  return (
    <>
      <PageHead kicker="History & audit" title="What happened, and who did it.">
        Viewers who left, ended knocks, and every command, sign-in and settings change the server saw.
      </PageHead>
      <label className="label mb-2 block" htmlFor="hist-stream">Stream</label>
      <select
        id="hist-stream"
        className="field mb-8 max-w-md"
        value={stream ?? ""}
        onChange={(e) => navigate(href({ name: "history", stream: e.target.value || null }))}
      >
        <option value="">All streams (audit only)</option>
        {streams.map((s) => (
          <option key={s.id} value={s.id}>
            {(s.label || `Stream ${s.id.slice(0, 6)}`) + ` · ${dateTime(s.created_at)}`}
          </option>
        ))}
      </select>
      {stream && (
        <section className="mb-10" aria-labelledby="h-viewers">
          <h2 id="h-viewers" className="label mb-3">Viewer history</h2>
          <HistoryList key={stream} stream={stream} />
        </section>
      )}
      <section aria-labelledby="h-audit">
        <h2 id="h-audit" className="label mb-3">Audit log</h2>
        <AuditList key={stream} stream={stream} />
      </section>
    </>
  );
}
