// Live updates from GET /api/v1/streams/{id}/events (CONTRACT §9.3).
// Events aren't replayed: each (re)connect starts with fresh `stream` and
// `viewers` snapshots, so a reconnect simply resumes from the new snapshot.
// The browser's own EventSource retry stops on HTTP errors (401, 429), so
// the feed closes on every error and reconnects itself with jittered
// exponential backoff.
import { useEffect, useRef, useState } from "react";
import type { Alert, Stream, ViewerList } from "./types";

export type FeedStatus = "connecting" | "open" | "retrying" | "ended";

export interface FeedHandlers {
  stream?: (s: Stream) => void;
  viewers?: (v: ViewerList) => void;
  alert?: (a: Alert) => void;
  ended?: (d: { reason: string | null }) => void;
  status?: (s: FeedStatus, retryInMs?: number) => void;
  /** Called before each reconnect; return false to stop (e.g. signed out). */
  beforeRetry?: () => Promise<boolean>;
}

export interface FeedDeps {
  EventSource: typeof EventSource;
  setTimeout: (fn: () => void, ms: number) => unknown;
  clearTimeout: (t: unknown) => void;
  random: () => number;
}

const defaults = (): FeedDeps => ({
  EventSource: globalThis.EventSource,
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (t) => globalThis.clearTimeout(t as number),
  random: Math.random,
});

export const BACKOFF_MIN_MS = 1000;
export const BACKOFF_MAX_MS = 30_000;

/** Full-jitter backoff: uniform in [base/2, base], base doubling per failure. */
export function backoff(failures: number, random: () => number): number {
  const base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** Math.max(0, failures - 1));
  return Math.round(base / 2 + (random() * base) / 2);
}

export class EventFeed {
  private es: EventSource | null = null;
  private timer: unknown = null;
  private failures = 0;
  private stopped = false;
  private deps: FeedDeps;

  constructor(
    private url: string,
    private h: FeedHandlers,
    deps: Partial<FeedDeps> = {},
  ) {
    this.deps = { ...defaults(), ...deps };
    this.open();
  }

  private parse<T>(e: Event, fn?: (v: T) => void) {
    if (!fn) return;
    try {
      fn(JSON.parse((e as MessageEvent).data) as T);
    } catch {
      /* a malformed event is dropped, never half-applied */
    }
  }

  private open() {
    if (this.stopped) return;
    this.h.status?.("connecting");
    const es = new this.deps.EventSource(this.url, { withCredentials: true });
    this.es = es;
    es.addEventListener("open", () => {
      this.failures = 0;
      this.h.status?.("open");
    });
    es.addEventListener("stream", (e) => {
      // The first snapshot also proves the connection is healthy.
      this.failures = 0;
      this.parse(e, this.h.stream);
    });
    es.addEventListener("viewers", (e) => this.parse(e, this.h.viewers));
    es.addEventListener("alert", (e) => this.parse(e, this.h.alert));
    es.addEventListener("ended", (e) => {
      this.parse(e, this.h.ended);
      this.h.status?.("ended");
      this.stop();
    });
    es.addEventListener("error", () => {
      if (this.es !== es || this.stopped) return;
      es.close();
      this.es = null;
      this.failures++;
      const wait = backoff(this.failures, this.deps.random);
      this.h.status?.("retrying", wait);
      this.timer = this.deps.setTimeout(async () => {
        this.timer = null;
        if (this.stopped) return;
        if (this.h.beforeRetry && !(await this.h.beforeRetry())) {
          this.stop();
          return;
        }
        this.open();
      }, wait);
    });
  }

  stop() {
    this.stopped = true;
    if (this.timer !== null) this.deps.clearTimeout(this.timer);
    this.timer = null;
    this.es?.close();
    this.es = null;
  }
}

/** React binding: one feed per stream id, handlers kept current by ref. */
export function useEventFeed(id: string | null, handlers: FeedHandlers) {
  const ref = useRef(handlers);
  ref.current = handlers;
  const [status, setStatus] = useState<{ s: FeedStatus; retryIn?: number }>({ s: "connecting" });
  useEffect(() => {
    if (!id) return;
    const feed = new EventFeed(`/api/v1/streams/${encodeURIComponent(id)}/events`, {
      stream: (v) => ref.current.stream?.(v),
      viewers: (v) => ref.current.viewers?.(v),
      alert: (v) => ref.current.alert?.(v),
      ended: (v) => ref.current.ended?.(v),
      beforeRetry: () => ref.current.beforeRetry?.() ?? Promise.resolve(true),
      status: (s, retryIn) => setStatus({ s, retryIn }),
    });
    return () => feed.stop();
  }, [id]);
  return status;
}
