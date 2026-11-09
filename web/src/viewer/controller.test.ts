import { afterEach, describe, expect, it, vi } from "vitest";
import { ViewerController } from "./controller";

/** An admission whose handle() resolves when the test says so. */
function slowAdmission() {
  let resolve!: (outs: Uint8Array[]) => void;
  const adm = {
    state: "hello_sent" as string,
    pending: null,
    result: null,
    abort: vi.fn(),
    handle: vi.fn(
      () =>
        new Promise<Uint8Array[]>((r) => {
          resolve = (outs) => {
            adm.state = "response_sent";
            r(outs);
          };
        }),
    ),
  };
  return { adm, finish: (outs: Uint8Array[]) => resolve(outs) };
}

function controller() {
  const c = new ViewerController();
  const sent: unknown[] = [];
  c.session = { sendSignal: (m: unknown) => (sent.push(m), true), stop: () => {}, startMedia: () => {} } as unknown as ViewerController["session"];
  return { c, sent };
}

afterEach(() => vi.restoreAllMocks());

describe("a stale admission completion has no side effect", () => {
  it("replaced by a rejoin during the await: nothing sent, the typed PIN kept", async () => {
    const { c, sent } = controller();
    const { adm, finish } = slowAdmission();
    c["adm"] = adm as never;
    c["typedPin"] = "4821";
    const p = c["receive"]("AAAA");
    // A rejoin (joined) drops the admission and starts a new one with the same typed PIN.
    const next = slowAdmission();
    c["adm"] = next.adm as never;
    finish([new Uint8Array([1, 2, 3])]);
    await p;
    expect(sent).toEqual([]);
    expect(c["typedPin"]).toBe("4821");
  });
  it("disposed during the await: nothing sent, no view change", async () => {
    const { c, sent } = controller();
    const { adm, finish } = slowAdmission();
    c["adm"] = adm as never;
    c["typedPin"] = "4821";
    c.view = { kind: "verifying" };
    const p = c["receive"]("AAAA");
    c["onTerminal"]("stream_ended");
    finish([new Uint8Array([1])]);
    await p;
    expect(sent).toEqual([]);
    expect(c.view).toEqual({ kind: "ended", reason: "stopped" });
  });
  it("a current admission still answers", async () => {
    const { c, sent } = controller();
    const { adm, finish } = slowAdmission();
    c["adm"] = adm as never;
    c["typedPin"] = "4821";
    const p = c["receive"]("AAAA");
    finish([new Uint8Array([1])]);
    await p;
    expect(sent).toHaveLength(1);
    expect(c["typedPin"]).toBeNull();
  });
});

describe("terminal views are irreversible", () => {
  it("nothing leaves ended or removed", () => {
    const { c } = controller();
    c["onTerminal"]("stream_ended");
    c["setView"]({ kind: "media" });
    c["setView"]({ kind: "removed", reason: "kicked" });
    expect(c.view).toEqual({ kind: "ended", reason: "stopped" });
    const d = controller().c;
    d["lose"]("kicked");
    d["onTerminal"]("stream_ended");
    d["setView"]({ kind: "pin", policy: { kind: "digits", length: 4 } });
    expect(d.view).toEqual({ kind: "removed", reason: "kicked" });
  });
});

describe("the typed name reaches the host before the knock", () => {
  it("a new name rejoins signaling once, before any HELLO, and keeps the PIN", () => {
    const { c, sent } = controller();
    const rejoin = vi.fn(() => true);
    Object.assign(c.session!, { rejoin });
    const handshake = vi.fn();
    (c as unknown as { handshake: typeof handshake }).handshake = handshake;
    c.submitPin("4821", "Ana");
    expect(rejoin).toHaveBeenCalledTimes(1);
    expect(handshake).not.toHaveBeenCalled();
    expect(sent).toEqual([]);
    expect(c["join"].label).toBe("Ana");
    expect(c["typedPin"]).toBe("4821");
  });
  it("the same name as the join goes straight to the HELLO", () => {
    const { c } = controller();
    const rejoin = vi.fn(() => true);
    Object.assign(c.session!, { rejoin });
    c["join"].label = "Ana";
    c["joinedLabel"] = "Ana";
    const handshake = vi.fn();
    (c as unknown as { handshake: typeof handshake }).handshake = handshake;
    c.submitPin("4821", "Ana");
    expect(rejoin).not.toHaveBeenCalled();
    expect(handshake).toHaveBeenCalledWith("pin");
  });
});
