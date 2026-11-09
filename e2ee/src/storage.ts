/**
 * Persistent, atomic storage for sender-side state (guess budget, grants,
 * epoch counter). Values are JSON. `update` must apply `fn` atomically with
 * respect to every other `update` on the same key, including from other tabs
 * of the same sender, and persist the result before resolving.
 */
import { E2EEError } from "./errors.ts";

export type Json = null | boolean | number | string | Json[] | { [k: string]: Json };

export interface AtomicStorage {
  get(key: string): Promise<Json | undefined>;
  /** `fn` must be synchronous and side-effect free; it may be re-run. */
  update(key: string, fn: (current: Json | undefined) => Json): Promise<Json>;
}

/** In-memory storage for tests and for senders that accept losing state on reload. */
export class MemoryStorage implements AtomicStorage {
  readonly #data = new Map<string, string>();

  async get(key: string): Promise<Json | undefined> {
    const v = this.#data.get(key);
    return v === undefined ? undefined : (JSON.parse(v) as Json);
  }

  async update(key: string, fn: (current: Json | undefined) => Json): Promise<Json> {
    // Synchronous read-modify-write: atomic within one JS realm.
    const cur = this.#data.get(key);
    const next = fn(cur === undefined ? undefined : (JSON.parse(cur) as Json));
    this.#data.set(key, JSON.stringify(next));
    return JSON.parse(JSON.stringify(next)) as Json;
  }

  /** Test helper: snapshot of the raw stored JSON. */
  dump(): Record<string, string> {
    return Object.fromEntries(this.#data);
  }
}

interface LocksLike {
  request<T>(name: string, cb: () => Promise<T> | T): Promise<T>;
}
interface KVLike {
  getItem(k: string): string | null;
  setItem(k: string, v: string): void;
}

/**
 * Browser storage: localStorage for persistence, the Web Locks API for
 * cross-tab atomicity. Refuses to run without Web Locks rather than silently
 * losing atomicity.
 */
export class LockedLocalStorage implements AtomicStorage {
  readonly #kv: KVLike;
  readonly #locks: LocksLike;
  readonly #prefix: string;

  constructor(opts: { kv: KVLike; locks: LocksLike | undefined; prefix?: string }) {
    if (!opts.locks) throw new Error("Web Locks API unavailable: guess budget cannot be atomic");
    this.#kv = opts.kv;
    this.#locks = opts.locks;
    this.#prefix = opts.prefix ?? "passstream:e2ee:";
  }

  async get(key: string): Promise<Json | undefined> {
    const v = this.#kv.getItem(this.#prefix + key);
    return v === null ? undefined : (JSON.parse(v) as Json);
  }

  update(key: string, fn: (current: Json | undefined) => Json): Promise<Json> {
    const k = this.#prefix + key;
    return this.#locks.request(k, () => {
      const cur = this.#kv.getItem(k);
      const next = fn(cur === null ? undefined : (JSON.parse(cur) as Json));
      this.#kv.setItem(k, JSON.stringify(next));
      return next;
    });
  }
}

/** No storage operation may block its caller longer than this (see TimedStorage). */
export const DEFAULT_STORAGE_TIMEOUT_MS = 1_000;

/** Retry delays after failures: 100 ms, doubling, at most 2 s apart. */
export function retryDelayMs(attempt: number): number {
  return Math.min(100 * 2 ** Math.min(attempt, 10), 2_000);
}

/**
 * Bounds every operation of an AtomicStorage: one still unresolved after
 * `timeoutMs` rejects with E2EEError("storage"), so a hung write can't hold a
 * queue (and with it recovery) forever. Measured on the injected clock by
 * check(), which the owner's tick() calls, and by a real timer as a backstop.
 *
 * The underlying operation may still complete later. Every write made
 * through it is safe to land late or twice: epoch numbers are only skipped,
 * revocations only revoke more, and an orphaned grant is never handed out.
 */
export class TimedStorage implements AtomicStorage {
  readonly #inner: AtomicStorage;
  readonly #now: () => number;
  readonly timeoutMs: number;
  readonly #ops = new Set<{ at: number; fail(): void }>();

  constructor(inner: AtomicStorage, now: () => number, timeoutMs = DEFAULT_STORAGE_TIMEOUT_MS) {
    this.#inner = inner;
    this.#now = now;
    this.timeoutMs = timeoutMs;
  }

  get(key: string): Promise<Json | undefined> {
    return this.#run(() => this.#inner.get(key), key);
  }

  update(key: string, fn: (current: Json | undefined) => Json): Promise<Json> {
    return this.#run(() => this.#inner.update(key, fn), key);
  }

  /** Reject every operation that has been running for `timeoutMs` or longer. */
  check(): void {
    const now = this.#now();
    for (const op of [...this.#ops]) if (now - op.at >= this.timeoutMs) op.fail();
  }

  #run<T>(op: () => Promise<T>, key: string): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const entry = {
        at: this.#now(),
        fail: () => {
          done();
          reject(new E2EEError("storage", `storage operation on ${key} timed out after ${this.timeoutMs} ms`));
        },
      };
      const timer = setTimeout(entry.fail, this.timeoutMs);
      // Node timers would otherwise keep a process alive; browsers return a number.
      (timer as unknown as { unref?: () => void }).unref?.();
      const done = () => {
        this.#ops.delete(entry);
        clearTimeout(timer);
      };
      this.#ops.add(entry);
      // Started synchronously: a write is handed to the storage before the caller continues.
      let p: Promise<T>;
      try {
        p = op();
      } catch (e) {
        p = Promise.reject(e);
      }
      p.then(
        (v) => {
          done();
          resolve(v);
        },
        (e: unknown) => {
          done();
          reject(e instanceof E2EEError ? e : new E2EEError("storage", String(e)));
        },
      );
    });
  }
}
