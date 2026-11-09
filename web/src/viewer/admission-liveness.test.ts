import { afterEach, describe, expect, it, vi } from "vitest";
import { fromBase64Url, toBase64Url } from "../../../e2ee/src/bytes";
import { makeHost, PIN, STREAM } from "../../../e2ee/test/harness";
import { ViewerController } from "./controller";

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

async function setup(knock = false) {
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  const h = await makeHost({ now: Date.now, knock });
  const data = new Map<string, string>();
  vi.stubGlobal("sessionStorage", { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => data.set(k, v) });
  let peer = "initial";
  const pending: Uint8Array[] = [];
  const create = () => {
    const c = new ViewerController();
    c["frag"] = { capability: "test-only", senderKey: h.identity.publicKey, linkSecret: new Uint8Array(h.linkSecret) };
    c.session = {
      sendSignal: (m: { b?: string }) => { if (m.b) pending.push(fromBase64Url(m.b)); return true; },
      stop: () => {}, startMedia: () => {}, rejoin: () => false,
    } as unknown as ViewerController["session"];
    return c;
  };
  const joined = (c: ViewerController) => c["onSignal"]({ type: "joined", stream_id: STREAM, connection_id: peer, sender_online: true, sender_instance: "same", pin_hint: { kind: "digits", length: 4 } });
  const pump = async (c: ViewerController, drop = false) => {
    for (let n = 0; n < 8; n++) {
      const sends = pending.splice(0);
      for (const b of sends) await h.host.receive(peer, b);
      const replies = h.take(peer);
      if (!drop) for (const b of replies) await c["receive"](toBase64Url(b));
      if (!pending.length) return;
    }
    throw Error("relay did not settle");
  };
  const initial = create(); joined(initial); initial.submitPin(PIN); await pump(initial);
  if (!knock) expect(initial.admitted).toBe(true);
  const reload = async () => {
    const id = h.idOf(peer);
    await h.host.peerClosed(peer); peer = "replacement";
    const c = create(); joined(c);
    await h.host.receive(peer, pending.shift()!);
    await c["receive"](toBase64Url(h.take(peer)[0]!));
    expect(c["adm"]?.state).toBe("response_sent");
    return { c, id };
  };
  return { h, initial, create, joined, pump, reload, pending, peer: () => peer };
}

describe("admission progress with real CPace and the real controller", () => {
  it("keeps an unanswered PIN HELLO waiting through a known offline interval and checks on return", async () => {
    const s = await setup();
    sessionStorage.setItem(`ps:viewer:${STREAM}`, JSON.stringify({ sid: toBase64Url(new Uint8Array(16).fill(9)) }));
    const c = s.create(); s.joined(c); c.submitPin(PIN);
    s.pending.splice(0); // HELLO lost before the host reserves a guess.
    c["onSignal"]({ type: "e2ee.sender_away" });
    await vi.advanceTimersByTimeAsync(60000); c["tick"]();
    expect(c.view.kind).toBe("waiting"); expect(c["typedPin"]).toBe(PIN);
    expect(c.events.some((e) => e.kind === "admission_timeout")).toBe(false);
    c["onSignal"]({ type: "e2ee.sender_back", instance: "same" }); await s.pump(c);
    expect(c.admitted).toBe(true); expect(c.busy).toBe(false);
    expect(c.events.filter((e) => e.kind === "hello")).toHaveLength(2);
  });

  it("does not retain or automatically reuse an answered PIN challenge during a known offline interval", async () => {
    const s = await setup();
    sessionStorage.setItem(`ps:viewer:${STREAM}`, JSON.stringify({ sid: toBase64Url(new Uint8Array(16).fill(9)) }));
    const c = s.create(); s.joined(c); c.submitPin(PIN);
    await s.h.host.receive(s.peer(), s.pending.shift()!);
    await c["receive"](toBase64Url(s.h.take(s.peer())[0]!));
    expect(c["adm"]?.state).toBe("response_sent"); expect(c["typedPin"]).toBeNull();
    s.pending.splice(0); c["onSignal"]({ type: "e2ee.sender_away" });
    await vi.advanceTimersByTimeAsync(5001); c["tick"]();
    expect(c.view.kind).toBe("pin"); expect(c.busy).toBe(false);
    c["onSignal"]({ type: "e2ee.sender_back", instance: "same" });
    expect(s.pending).toEqual([]); expect(c["typedPin"]).toBeNull();
    c.submitPin(PIN); await s.pump(c); expect(c.admitted).toBe(true);
  });

  it("pending knock replacement never sends the previous transcript's denial into the new check", async () => {
    const s = await setup(true), c = s.initial;
    const previous = s.h.host.knocks()[0]!.knockId;
    c["onSignal"]({ type: "e2ee.sender_away" });
    c["onSignal"]({ type: "e2ee.sender_back", instance: "same" });
    c.submitPin(PIN); await s.pump(c);
    expect(c.view.kind).toBe("knock");
    expect(c.events.filter((e) => e.kind === "refused")).toEqual([]);
    const next = s.h.host.knocks()[0]!; expect(next.knockId).not.toBe(previous);
    await s.h.host.approve(next.knockId, { rowCode: next.rowCode }); await s.pump(c);
    expect(c.admitted).toBe(true);
  });

  it.each(["issue-refused", "issue-throws", "admit-seal"] as const)("superseded approval %s cannot corrupt the replacement or undo a claimed record", async (stage) => {
    const s = await setup(true), c = s.initial, knock = s.h.host.knocks()[0]!;
    let release!: () => void, entered!: () => void;
    const gate = new Promise<void>((r) => { release = r; }), ready = new Promise<void>((r) => { entered = r; });
    if (stage === "admit-seal") {
      const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
      vi.spyOn(crypto.subtle, "encrypt").mockImplementation(async (...args) => {
        const algorithm = args[0] as AesGcmParams;
        if (algorithm.additionalData && new TextDecoder().decode(algorithm.additionalData) === "PassStream v1 admit") { entered(); await gate; }
        return encrypt(...args);
      });
    } else {
      const issue = s.h.host.grants.issue.bind(s.h.host.grants);
      vi.spyOn(s.h.host.grants, "issue").mockImplementationOnce(async (...args) => {
        entered(); await gate;
        if (stage === "issue-throws") throw Error("injected grant storage failure");
        return issue(...args);
      });
    }
    const approving = s.h.host.approve(knock.knockId, { rowCode: knock.rowCode }).catch((e: unknown) => e);
    await ready;
    const claimed = s.h.host.viewers()[0];
    c["onSignal"]({ type: "e2ee.sender_away" });
    c["onSignal"]({ type: "e2ee.sender_back", instance: "same" });
    c.submitPin(PIN);
    await s.h.host.receive(s.peer(), s.pending.shift()!);
    await c["receive"](toBase64Url(s.h.take(s.peer())[0]!));
    const current = c["adm"]!; expect(current.state).toBe("response_sent");
    release(); await approving;
    for (const b of s.h.take(s.peer())) await c["receive"](toBase64Url(b));
    expect(c["adm"]).toBe(current); expect(c.events.filter((e) => e.kind === "refused")).toEqual([]);
    if (stage === "admit-seal") {
      expect(claimed?.connected).toBe(true);
      expect(s.h.host.viewers().find((v) => v.admissionId === claimed!.admissionId)?.connected).toBe(true);
      expect(s.h.host.keys.hasSession(claimed!.admissionId)).toBe(true);
    }
    await s.pump(c); expect(c.view.kind).toBe("knock");
    const next = s.h.host.knocks()[0]!;
    await s.h.host.approve(next.knockId, { rowCode: next.rowCode }); await s.pump(c);
    expect(c.admitted).toBe(true);
  });

  it.each(["kick", "rotate"] as const)("%s cancellation immediately returns a reload to the PIN screen", async (action) => {
    const s = await setup(), { c, id } = await s.reload();
    const previous = c["adm"]!;
    if (action === "kick") await s.h.host.kickAndRotatePin(id, "1357");
    else await s.h.host.rotatePin("1357");
    await s.pump(c);
    expect(c.view.kind).toBe("pin"); expect(c.busy).toBe(false);
    expect(c["stored"]?.grant).toBeUndefined();
    expect(previous._retainedSecrets()).toEqual([]);
    c.submitPin(PIN); await s.pump(c);
    expect(c.events.some((e) => e.kind === "refused" && e.detail === "wrong_pin")).toBe(true);
    c.submitPin("1357"); await s.pump(c);
    expect(c.admitted).toBe(true);
  });

  it.each(["kick", "rotate"] as const)("a dropped %s cancellation gets a bounded grant retry, never a PIN replay", async (action) => {
    const s = await setup(), { c, id } = await s.reload();
    if (action === "kick") await s.h.host.kickAndRotatePin(id, "1357");
    else await s.h.host.rotatePin("1357");
    s.h.take(s.peer()); // lose the authentic cancellation
    await s.h.host.receive(s.peer(), s.pending.shift()!);
    await vi.advanceTimersByTimeAsync(5001); c["tick"]();
    expect(c.busy).toBe(false);
    await vi.runOnlyPendingTimersAsync(); await s.pump(c);
    expect(c.view.kind).toBe("pin"); expect(c.busy).toBe(false);
    expect(c.events.filter((e) => e.kind === "hello").map((e) => e.detail)).toEqual(["grant", "grant"]);
    expect(c.events.some((e) => e.kind === "refused" && e.detail === "grant_invalid")).toBe(true);
  });

  it("persistent lost grant answers end visibly with busy cleared, retaining the unauthenticated grant for a manual reload", async () => {
    const s = await setup(), { c } = await s.reload();
    for (let n = 0; n < 8 && !c.disposed; n++) {
      await s.pump(c, true);
      await vi.advanceTimersByTimeAsync(5001); c["tick"]();
      expect(c.busy).toBe(false);
      await vi.runOnlyPendingTimersAsync();
    }
    expect(c.view).toEqual({ kind: "ended", reason: "admission_timeout" });
    expect(c.busy).toBe(false); expect(c["adm"]).toBeNull();
    expect(c["stored"]?.grant).toBeTruthy();
    expect(c.events.filter((e) => e.kind === "hello").length).toBeLessThanOrEqual(6);
  });

  it("a PIN answer timeout drops the attempt, never auto-reuses the PIN, and a late valid ADMIT cannot re-admit", async () => {
    const s = await setup();
    // A new viewer has no remembered grant.
    sessionStorage.setItem(`ps:viewer:${STREAM}`, JSON.stringify({ sid: toBase64Url(new Uint8Array(16).fill(8)) }));
    const c = s.create(); s.joined(c); c.submitPin(PIN);
    await s.h.host.receive(s.peer(), s.pending.shift()!);
    await c["receive"](toBase64Url(s.h.take(s.peer())[0]!));
    await s.h.host.receive(s.peer(), s.pending.shift()!);
    const admit = s.h.take(s.peer())[0]!;
    const old = c["adm"]!;
    const decrypt = crypto.subtle.decrypt.bind(crypto.subtle);
    let entered!: () => void, release!: () => void;
    const ready = new Promise<void>((r) => { entered = r; }), gate = new Promise<void>((r) => { release = r; });
    vi.spyOn(crypto.subtle, "decrypt").mockImplementationOnce(async (...args) => { entered(); await gate; return decrypt(...args); });
    const held = c["receive"](toBase64Url(admit)); await ready;
    await vi.advanceTimersByTimeAsync(5001); c["tick"]();
    expect(c.view.kind).toBe("pin"); expect(c.busy).toBe(false); expect(c.pinError).toBeTruthy();
    expect(s.pending).toEqual([]); expect(old._retainedSecrets()).toEqual([]);
    release(); await held;
    expect(c.admissions).toBe(0); expect(c.admitted).toBe(false); expect(old.result).toBeNull();
    c.submitPin(PIN); await s.pump(c); expect(c.admitted).toBe(true);
  });

  it("normal PIN and grant checks succeed, while a legitimate pending knock keeps its signed deadline", async () => {
    const s = await setup(); const { c } = await s.reload(); await s.pump(c);
    expect(c.admitted).toBe(true); expect(c.busy).toBe(false);
    const k = await setup(true), waiting = k.initial;
    expect(waiting.view.kind).toBe("knock");
    await vi.advanceTimersByTimeAsync(6000); waiting["tick"]();
    expect(waiting.view.kind).toBe("knock"); expect(waiting.busy).toBe(false);
    const knock = k.h.host.knocks()[0]!;
    await k.h.host.approve(knock.knockId, { rowCode: knock.rowCode }); await k.pump(waiting);
    expect(waiting.admitted).toBe(true);
  });

  it("admitted-viewer missing-key recovery still re-admits with its grant", async () => {
    const s = await setup(), c = s.initial;
    c["missingKeySince"] = Date.now() - 2500;
    c["lastMissingAt"] = Date.now();
    c["tick"](); await s.pump(c);
    expect(c.admissions).toBe(2); expect(c.admitted).toBe(true); expect(c.busy).toBe(false);
    expect(c.events.some((e) => e.kind === "readmit" && e.detail === "missing epoch")).toBe(true);
  });

  it("a pending knock expires on its signed deadline, without admission progress retries", async () => {
    const s = await setup(true), c = s.initial;
    await vi.advanceTimersByTimeAsync(125001); c["tick"]();
    expect(c.view).toEqual({ kind: "removed", reason: "denied" });
    expect(c.busy).toBe(false);
    expect(c.events.some((e) => e.kind === "admission_timeout")).toBe(false);
  });
});
