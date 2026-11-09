/** Minimal typed event emitter (works in workers and Node without polyfills). */
export class Emitter<Events extends Record<string, unknown>> {
  readonly #handlers = new Map<keyof Events, Set<(e: never) => void>>();

  on<K extends keyof Events>(type: K, fn: (e: Events[K]) => void): () => void {
    let set = this.#handlers.get(type);
    if (!set) {
      set = new Set();
      this.#handlers.set(type, set);
    }
    set.add(fn as (e: never) => void);
    return () => set.delete(fn as (e: never) => void);
  }

  protected emit<K extends keyof Events>(type: K, e: Events[K]): void {
    for (const fn of this.#handlers.get(type) ?? []) {
      try {
        (fn as (e: Events[K]) => void)(e);
      } catch {
        // A failing UI listener must not break the security state machine.
      }
    }
  }
}
