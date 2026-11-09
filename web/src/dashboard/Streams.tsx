import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";
import { dateTime, durationShort, endReason, expiry, phaseOf, pinSummary } from "./format";
import { useNow } from "./LiveStream";
import { href, Link } from "./router";
import type { Stream } from "./types";
import { Badge, Empty, IconChevron, Loading, Notice, PageHead } from "./ui";

function StreamRow({ s, now }: { s: Stream; now: number }) {
  const ph = phaseOf(s);
  const ended = s.ended_at !== null || s.phase === "ended";
  const access = s.effective?.access;
  const pin = pinSummary(access?.pin ?? s.declared.pin);
  return (
    <li>
      <Link
        to={href({ name: "stream", id: s.id })}
        data-testid="stream-row"
        className={`sheet flex items-center gap-3 px-4 py-3.5 transition-colors hover:bg-paper md:gap-5 md:px-5 ${ended ? "opacity-75" : ""}`}
      >
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-center gap-2">
            <span className="truncate font-semibold">{s.label || `Stream ${s.id.slice(0, 6)}`}</span>
            <Badge tone={ph.tone}>{ph.text.toUpperCase()}</Badge>
            {access?.latches.locked && <Badge tone="warn">LOCKED</Badge>}
            {access?.latches.frozen && <Badge tone="stamp">FROZEN</Badge>}
          </span>
          <span className="num mt-1 block text-[0.76rem] text-ink-mute">
            <span className={pin.tone === "warn" ? "text-warn" : ""}>{pin.text}</span>
            {(access?.knock ?? s.declared.knock) && " · knock"}
            {s.effective?.e2ee && ` · E2EE ${s.effective.e2ee.status === "on" ? "on" : "beta"}`}
            {" · "}
            {ended ? `${endReason(s.end_reason) || "ended"} ${s.ended_at ? dateTime(s.ended_at) : ""}` : `${s.sender.online ? "sender online" : "sender offline"}${expiry(s, now) ? ` · ${expiry(s, now)}` : ""}`}
          </span>
        </span>
        {!ended && (
          <>
            <span className="hidden text-right sm:block">
              <span className="num block text-[1.35rem] leading-none">{s.watching}</span>
              <span className="label">watching</span>
            </span>
            <span className="num text-[1.1rem] sm:hidden" aria-label={`${s.watching} watching`}>{s.watching}</span>
            <span className="num hidden w-24 text-right text-[0.76rem] text-ink-mute md:block">
              up {durationShort(now - s.created_at)}
            </span>
          </>
        )}
        <IconChevron className="size-4 -rotate-90 text-ink-mute" />
      </Link>
    </li>
  );
}

/** Server order: newest first by creation, then id (store.StreamPage). */
function byCreated(a: Stream, b: Stream) {
  return b.created_at - a.created_at || (a.id < b.id ? 1 : a.id > b.id ? -1 : 0);
}

/**
 * Reconcile rows by id: fresh rows replace older copies, rows already loaded
 * from older pages stay, and nothing is listed twice. Offset cursors shift
 * when streams end, so pages can overlap.
 */
export function mergeStreams(prev: Stream[] | null, fresh: Stream[]): Stream[] {
  const out = new Map((prev ?? []).map((s) => [s.id, s]));
  for (const s of fresh) out.set(s.id, s);
  return [...out.values()].sort(byCreated);
}

/** Every live stream: live shares are few, so all pages are fetched each refresh. */
async function allLive(): Promise<Stream[]> {
  const out: Stream[] = [];
  let cursor: string | null = null;
  for (let page = 0; page < 20; page++) {
    const r = await api.streams("live", cursor);
    out.push(...r.streams);
    cursor = r.next_cursor;
    if (!cursor) break;
  }
  return mergeStreams(null, out);
}

export function Streams() {
  const now = useNow(15_000);
  const [live, setLive] = useState<Stream[] | null>(null);
  const [ended, setEnded] = useState<Stream[] | null>(null);
  // Pagination is separate from refresh: polling re-reads the first page of
  // ended streams, "Older streams" walks the cursor, and neither overwrites
  // the other's rows or cursor.
  const [next, setNext] = useState<string | null>(null);
  const paged = useRef(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let off = false;
    const load = () =>
      Promise.all([allLive(), api.streams("ended")]).then(
        ([l, e]) => {
          if (off) return;
          const liveIds = new Set(l.map((s) => s.id));
          setLive(l);
          setEnded((prev) => mergeStreams(prev?.filter((s) => !liveIds.has(s.id)) ?? null, e.streams));
          if (!paged.current) setNext(e.next_cursor);
          setError(null);
        },
        (e: ApiError) => !off && setError(e.message),
      );
    void load();
    const t = setInterval(load, 10_000);
    return () => {
      off = true;
      clearInterval(t);
    };
  }, []);
  const more = async () => {
    if (!next || loadingMore) return;
    setLoadingMore(true);
    try {
      const r = await api.streams("ended", next);
      paged.current = true;
      setEnded((p) => mergeStreams(p, r.streams));
      setNext(r.next_cursor);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoadingMore(false);
    }
  };
  return (
    <>
      <PageHead kicker="Streams" title="Who's watching your screen.">
        Live shares first, then the ones that ended. Start a share from the bar plugin or <span className="num text-[0.9em]">passstream</span> CLI; the PIN and link stay on that machine.
      </PageHead>
      {error && <div className="mb-4"><Notice tone="stamp" role="alert">{error}</Notice></div>}
      {!live || !ended ? (
        <Loading />
      ) : (
        <>
          <h2 className="label mb-3">Live · {live.length}</h2>
          {live.length === 0 ? (
            <Empty>Nothing is live right now.</Empty>
          ) : (
            <ul className="space-y-2.5" data-testid="live-streams">
              {live.map((s) => <StreamRow key={s.id} s={s} now={now} />)}
            </ul>
          )}
          <h2 className="label mt-10 mb-3">Ended</h2>
          {ended.length === 0 ? (
            <Empty>No ended streams.</Empty>
          ) : (
            <ul className="space-y-2.5" data-testid="ended-streams">
              {ended.map((s) => <StreamRow key={s.id} s={s} now={now} />)}
            </ul>
          )}
          {next && (
            <button type="button" className="btn mt-4" onClick={more} disabled={loadingMore}>
              Older streams
            </button>
          )}
        </>
      )}
    </>
  );
}
