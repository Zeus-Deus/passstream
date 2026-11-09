/**
 * The privacy curtain is the pause latch (FEATURES.md "Track D2" §2):
 * tightening applies at once from any channel; loosening is local only and
 * applies only after it is stored.
 */
import { describe, expect, it } from "vitest";
import { generateLinkSecret, HostRecovery, MemoryStorage, SenderIdentity, type AtomicStorage, type Json, type Latches } from "@passstream/e2ee";
import { checkLocal, checkRemote } from "./authority";

const refused = (op: string, args: Record<string, unknown> = {}) => {
  const r = checkRemote(op, args);
  return r && !r.ok ? r.error.code : null;
};

describe("curtain and annotation commands: tighten from anywhere, loosen locally", () => {
  it("curtain on (pause) runs from the server; curtain off (resume) is local_owner_required", () => {
    expect(refused("pause")).toBeNull();
    expect(refused("resume")).toBe("local_owner_required");
    expect(checkLocal("pause", {})).toBe(true);
    expect(checkLocal("resume", {})).toBe(true);
  });
  it("viewer drawing off and clearing run from the server; on is local only", () => {
    expect(refused("annotations", { viewers: false })).toBeNull();
    expect(refused("annotations.clear")).toBeNull();
    expect(refused("annotations", { viewers: true })).toBe("local_owner_required");
    expect(refused("annotations", {})).toBe("invalid_argument");
    expect(refused("annotations", { viewers: false, extra: 1 })).toBe("invalid_argument");
    expect(refused("annotations.clear", { scope: "all" })).toBe("invalid_argument");
    expect(checkLocal("annotations", { viewers: true })).toBe(true);
    expect(checkLocal("annotations", { viewers: "yes" })).toBe(false);
    expect(checkLocal("annotations.clear", {})).toBe(true);
    expect(checkLocal("annotations.clear", { x: 1 })).toBe(false);
  });
});

/** Storage whose writes can be held, to see what applies before the write lands. */
class HeldStorage implements AtomicStorage {
  inner = new MemoryStorage();
  held: (() => void)[] = [];
  hold = false;
  get(key: string) {
    return this.inner.get(key);
  }
  async update(key: string, fn: (c: Json | undefined) => Json) {
    if (this.hold) await new Promise<void>((r) => this.held.push(r));
    return this.inner.update(key, fn);
  }
  release() {
    this.hold = false;
    this.held.splice(0).forEach((r) => r());
  }
}

async function recovery(storage: HeldStorage, applied: Latches[]) {
  return HostRecovery.create({
    storage,
    streamId: "stream-curtain",
    initial: { pin: "4821", policy: { kind: "digits", length: 4 } },
    onLatches: (l) => applied.push({ ...l }),
    host: { identity: SenderIdentity.generate(), linkSecret: generateLinkSecret(), send: () => undefined, onEpoch: () => undefined, onSuspend: () => undefined },
  });
}

describe("the pause latch behind the curtain", () => {
  it("curtain on applies synchronously, before its storage write (within one frame at the sender)", async () => {
    const storage = new HeldStorage();
    const applied: Latches[] = [];
    const r = await recovery(storage, applied);
    applied.length = 0;
    storage.hold = true;
    const write = r.setLatch("paused", true);
    // No await yet: the page's track policy already has paused = true.
    expect(applied.at(-1)?.paused).toBe(true);
    storage.release();
    await write;
    expect(r.record.latches.paused).toBe(true);
    r.host.close();
  });

  it("curtain off applies only after it is stored; a reload in between stays curtained", async () => {
    const storage = new HeldStorage();
    const applied: Latches[] = [];
    const r = await recovery(storage, applied);
    await r.setLatch("paused", true);
    applied.length = 0;
    storage.hold = true;
    const write = r.setLatch("paused", false);
    await Promise.resolve();
    expect(applied.some((l) => !l.paused)).toBe(false);
    storage.release();
    await write;
    expect(applied.at(-1)?.paused).toBe(false);
    r.host.close();
  });

  it("a failed write keeps the curtain down and reports it applied", async () => {
    const applied: Latches[] = [];
    const storage = new HeldStorage();
    const r = await recovery(storage, applied);
    storage.update = async () => {
      throw new Error("disk full");
    };
    await expect(r.setLatch("paused", true)).rejects.toMatchObject({ code: "storage", details: { applied: true } });
    expect(applied.at(-1)?.paused).toBe(true);
    // Lifting it fails too, and the curtain stays.
    await expect(r.setLatch("paused", false)).rejects.toMatchObject({ code: "storage" });
    expect(applied.at(-1)?.paused).toBe(true);
    r.host.close();
  });
});
