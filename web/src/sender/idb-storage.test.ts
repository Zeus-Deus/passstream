import { describe, expect, it } from "vitest";
import { idleStreams } from "./idb-storage";

describe("abandoned sender records", () => {
  it("prunes only streams unseen for longer than the limit", () => {
    const now = 1_000_000_000;
    const day = 24 * 3600_000;
    expect(
      idleStreams(
        [
          { id: "fresh", seen: now - day },
          { id: "old", seen: now - 8 * day },
          { id: "never", seen: 0 },
        ],
        7 * day,
        now,
      ),
    ).toEqual(["old", "never"]);
  });
});
