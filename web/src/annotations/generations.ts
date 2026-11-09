import { MAX_STATE_ORIGINS, ORIGIN_COUNT, originFromIndex, originIndex } from "./codec";

/** Retained for this router's lifetime: exactly 10,649 counters (42,596 bytes), even through admission churn. */
export class ClearGenerations {
  private readonly values = new Uint32Array(ORIGIN_COUNT);
  private cursor = 0;

  get(origin: string): number {
    return this.values[originIndex(origin)]!;
  }

  advance(origin: string): number {
    const index = originIndex(origin);
    // Never wrap a generation back to an older value.
    if (this.values[index] === 0xffff_ffff) throw new RangeError("clear generation exhausted");
    return ++this.values[index]!;
  }

  observe(origin: string, gen: number): boolean {
    const index = originIndex(origin);
    if (gen <= this.values[index]!) return false;
    this.values[index] = gen;
    return true;
  }

  /** A fair page of restrictive state, independent of ink. New clears never reset the cursor. */
  page(): { origins: { origin: string; gen: number }[]; wrapped: boolean } {
    const origins: { origin: string; gen: number }[] = [];
    let wrapped = false;
    for (let scanned = 0; scanned < ORIGIN_COUNT && origins.length < MAX_STATE_ORIGINS; scanned++) {
      const index = this.cursor;
      this.cursor = (index + 1) % ORIGIN_COUNT;
      if (this.cursor === 0) wrapped = true;
      const gen = this.values[index]!;
      if (gen) origins.push({ origin: originFromIndex(index)!, gen });
    }
    return { origins, wrapped };
  }
}
