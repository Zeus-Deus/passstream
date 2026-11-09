/**
 * Command results by id, for the daemon and server channels (HOST-CONTROL
 * §4): a retried command (same id, same op and arguments) gets the first
 * execution's answer instead of running twice.
 *
 * - The key is the id *and* a fingerprint of (op, canonical args). An id
 *   that comes back with another op or other arguments is refused with
 *   `conflict`, never answered from the cache: a restarted daemon reusing
 *   "d1" for a pause must not get the cached result of "d1" = resume.
 * - A command still running is shared, not run again.
 * - Only terminal outcomes stay cached. `storage`, `internal` and
 *   `rotation_pending` failures are dropped once settled, so a retry with
 *   the same id runs again.
 * - At most `max` ids are kept. Only settled entries are evicted (oldest
 *   first): an in-flight command is never forgotten, so replaying its id
 *   can't run it twice. With `max` commands all still running, a new id is
 *   refused with `rate_limited` instead.
 * - The fingerprint is a process-keyed HMAC-SHA-256 of canonical JSON. PIN
 *   arguments are never retained in plaintext or as guessable unkeyed hashes.
 */
import { sha256, toBase64Url } from "@passstream/e2ee";
import { fail, type Result } from "./authority";

const RETRIABLE = new Set(["storage", "internal", "rotation_pending"]);

function canonical(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(canonical);
  if (v && typeof v === "object") {
    const o = v as Record<string, unknown>;
    return Object.fromEntries(
      Object.keys(o)
        .sort()
        .map((k) => [k, canonical(o[k])]),
    );
  }
  return v;
}

const fingerprintKey = crypto.getRandomValues(new Uint8Array(32));

export function commandFingerprint(op: string, args: Record<string, unknown>): string {
  const message = new TextEncoder().encode(JSON.stringify([op, canonical(args)]));
  // RFC 2104: the random 32-byte key fits SHA-256's 64-byte block.
  const inner = new Uint8Array(64).fill(0x36);
  const outer = new Uint8Array(64).fill(0x5c);
  for (let i = 0; i < fingerprintKey.length; i++) {
    inner[i] ^= fingerprintKey[i];
    outer[i] ^= fingerprintKey[i];
  }
  return toBase64Url(sha256.create().update(outer).update(sha256.create().update(inner).update(message).digest()).digest());
}

export function retriable(r: Result): boolean {
  return !r.ok && RETRIABLE.has(r.error.code);
}

export class CommandCache {
  readonly #entries = new Map<string, { fp: string; result: Promise<Result>; settled: boolean }>();
  constructor(readonly max = 256) {}

  /** Run `exec` once per (id, op, args); see the header. */
  run(id: string, op: string, args: Record<string, unknown>, exec: () => Promise<Result>): Promise<Result> {
    const fp = commandFingerprint(op, args);
    const hit = this.#entries.get(id);
    if (hit) return hit.fp === fp ? hit.result : Promise.resolve(fail("conflict", "command id reused for a different command"));
    if (this.#entries.size >= this.max && !this.#evictSettled()) {
      return Promise.resolve(fail("rate_limited", "too many commands in flight"));
    }
    let result: Promise<Result>;
    try {
      result = exec().catch(() => fail("internal", "sender error"));
    } catch {
      result = Promise.resolve(fail("internal", "sender error"));
    }
    const entry = { fp, result, settled: false };
    this.#entries.set(id, entry);
    void result.then((r) => {
      entry.settled = true;
      if (retriable(r) && this.#entries.get(id) === entry) this.#entries.delete(id);
    });
    return result;
  }

  /** Drop the oldest settled entry; false if every entry is still running. */
  #evictSettled(): boolean {
    for (const [k, e] of this.#entries) {
      if (e.settled) {
        this.#entries.delete(k);
        return true;
      }
    }
    return false;
  }

  keys(): IterableIterator<string> {
    return this.#entries.keys();
  }
  get size(): number {
    return this.#entries.size;
  }
}
