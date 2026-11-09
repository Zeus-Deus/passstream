/**
 * Batches failed-PIN events for the host UI, e.g. "7 failed PIN tries from 2
 * sources". `source` is whatever the app can attribute (viewer session id,
 * server-reported IP); it is never used for security decisions.
 */
export interface AlertBatch {
  failures: number;
  sources: number;
  frozen: boolean;
  firstAt: number;
  lastAt: number;
}

export class AlertBatcher {
  readonly #windowMs: number;
  readonly #onFlush: (b: AlertBatch) => void;
  readonly #setTimeout: (fn: () => void, ms: number) => unknown;
  #pending: { failures: number; sources: Set<string>; frozen: boolean; firstAt: number; lastAt: number } | null = null;

  constructor(opts: { windowMs?: number; onFlush: (b: AlertBatch) => void; setTimeout?: (fn: () => void, ms: number) => unknown }) {
    this.#windowMs = opts.windowMs ?? 10_000;
    this.#onFlush = opts.onFlush;
    this.#setTimeout = opts.setTimeout ?? ((fn, ms) => setTimeout(fn, ms));
  }

  failure(source: string, at: number = Date.now()): void {
    this.#open(at);
    const p = this.#pending!;
    p.failures += 1;
    p.sources.add(source);
    p.lastAt = at;
  }

  frozen(at: number = Date.now()): void {
    this.#open(at);
    this.#pending!.frozen = true;
    this.flush(); // freezing is urgent
  }

  #open(at: number): void {
    if (this.#pending) return;
    this.#pending = { failures: 0, sources: new Set(), frozen: false, firstAt: at, lastAt: at };
    this.#setTimeout(() => this.flush(), this.#windowMs);
  }

  flush(): void {
    const p = this.#pending;
    if (!p) return;
    this.#pending = null;
    this.#onFlush({ failures: p.failures, sources: p.sources.size, frozen: p.frozen, firstAt: p.firstAt, lastAt: p.lastAt });
  }
}
