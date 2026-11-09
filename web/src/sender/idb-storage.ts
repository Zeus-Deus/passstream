import type { AtomicStorage, Json } from "@passstream/e2ee";

/**
 * Sender-side persistent state in IndexedDB: the page recovery record
 * (`ps:stream:<id>`) and the E2EE library's budget, grants, revocation fence
 * and epoch counter. Each update is one readwrite transaction, so it is
 * atomic against every other tab of this origin, and it resolves only after
 * a strict-durability commit.
 */
const STORE = "kv";

function request<T>(r: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

export class IndexedDBStorage implements AtomicStorage {
  private constructor(private readonly db: IDBDatabase) {}

  static async open(name = "passstream-sender"): Promise<IndexedDBStorage> {
    const req = indexedDB.open(name, 1);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(STORE)) req.result.createObjectStore(STORE);
    };
    return new IndexedDBStorage(await request(req));
  }

  async get(key: string): Promise<Json | undefined> {
    const tx = this.db.transaction(STORE, "readonly");
    const v = await request(tx.objectStore(STORE).get(key));
    return v === undefined ? undefined : (JSON.parse(v as string) as Json);
  }

  update(key: string, fn: (current: Json | undefined) => Json): Promise<Json> {
    return new Promise((resolve, reject) => {
      const tx = this.db.transaction(STORE, "readwrite", { durability: "strict" });
      const store = tx.objectStore(STORE);
      let next: Json = null;
      let failure: unknown = null;
      const get = store.get(key);
      get.onsuccess = () => {
        try {
          const cur = get.result === undefined ? undefined : (JSON.parse(get.result as string) as Json);
          next = fn(cur);
          store.put(JSON.stringify(next), key);
        } catch (e) {
          failure = e;
          tx.abort();
        }
      };
      tx.oncomplete = () => resolve(JSON.parse(JSON.stringify(next)) as Json);
      tx.onabort = () => reject(failure ?? tx.error ?? new Error("storage transaction aborted"));
      tx.onerror = () => {
        /* onabort follows */
      };
    });
  }

  /** Stream ids that have a page recovery record. */
  async streams(): Promise<string[]> {
    const tx = this.db.transaction(STORE, "readonly");
    const keys = (await request(tx.objectStore(STORE).getAllKeys())) as string[];
    return keys.filter((k) => k.startsWith("ps:stream:")).map((k) => k.slice("ps:stream:".length));
  }

  /**
   * Logical removal of everything kept for a stream (HOST-CONTROL §5.7):
   * the page record and the library's budget, grants, fence and epoch.
   * Not secure erasure: the browser's storage may keep old copies.
   */
  async deleteStream(streamId: string): Promise<void> {
    const keys = [`ps:stream:${streamId}`, `ps:seen:${streamId}`, `budget:${streamId}`, `grants:${streamId}`, `revocations:${streamId}`, `epoch:${streamId}`];
    const tx = this.db.transaction(STORE, "readwrite", { durability: "strict" });
    for (const k of keys) tx.objectStore(STORE).delete(k);
    await new Promise<void>((resolve, reject) => {
      tx.oncomplete = () => resolve();
      tx.onabort = () => reject(tx.error);
    });
  }

  /** Note that this stream's sender ran now (abandoned-record pruning). */
  async touch(streamId: string, now = Date.now()): Promise<void> {
    await this.update(`ps:seen:${streamId}`, () => now);
  }

  /**
   * Remove the state of every other stream whose sender hasn't run for
   * `maxIdleMs`: a browser-sender tab closed without ending its stream
   * leaves its record behind, and nothing else ever deletes it.
   */
  async pruneIdle(keep: string, maxIdleMs: number, now = Date.now()): Promise<string[]> {
    const entries: { id: string; seen: number }[] = [];
    for (const id of await this.streams()) {
      if (id === keep) continue;
      const seen = await this.get(`ps:seen:${id}`);
      const rec = (await this.get(`ps:stream:${id}`)) as { created_at?: unknown } | undefined;
      entries.push({ id, seen: typeof seen === "number" ? seen : typeof rec?.created_at === "number" ? rec.created_at : 0 });
    }
    const gone = idleStreams(entries, maxIdleMs, now);
    for (const id of gone) await this.deleteStream(id);
    return gone;
  }
}

/** Streams last seen more than `maxIdleMs` ago. */
export function idleStreams(entries: { id: string; seen: number }[], maxIdleMs: number, now: number): string[] {
  return entries.filter((e) => now - e.seen > maxIdleMs).map((e) => e.id);
}
