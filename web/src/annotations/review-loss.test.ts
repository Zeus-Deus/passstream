import { fixtureBatch, payloadRecords } from "./test-input";
import { viewerBody } from "./test-input";
/**
 * Regressions for the D2 review round 2 (GPT-6.1-Sol), finding 1: a clear
 * whose batch is lost must not leave the recipient's ink forever. The first
 * two tests are the reviewer's probes as written.
 */
import { describe, expect, it } from "vitest";
import { encodeAnnotation } from "./codec";
import type { HostLink } from "./host";
import { HostAnnotations } from "./test-input";
import { ViewerAnnotations } from "./viewer";

describe("lost clears converge (review D2 r2 #1)", () => {
  it("review: a rejected send must not permanently lose clear-all", async () => {
    let t = 0,
      drop = false;
    const recipient = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => "AAA" }, { now: () => t });
    const l: HostLink = {
      onAnnotation: () => () => {},
      onAdmitted: () => () => {},
      onRevoked: () => () => {},
      allowed: () => true,
      recipients: () => [{ admissionId: "b", rowCode: "AAA" }],
      sendAnnotations: async (_, records) => {
        if (drop) return false;
        recipient.receive(records);
        return true;
      },
    };
    const h = new HostAnnotations(l, { now: () => t });
    h.draw({ kind: "stroke", id: 1, end: true, points: [{ x: 0.2, y: 0.3 }] });
    await h.flush();
    expect(recipient.store.origins.has("host")).toBe(true);
    t = 1000;
    drop = true;
    h.clear();
    await h.flush();
    drop = false;
    for (t = 2000; t < 40000; t += 1000) await h.flush();
    expect(recipient.store.origins.has("host")).toBe(false);
    recipient.dispose();
  });

  it("review: a rejected send must not permanently lose a kicked origin clear", async () => {
    let t = 0,
      drop = false;
    const recipient = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => "AAA" }, { now: () => t });
    const allowed = new Set(["a", "b"]);
    const l: HostLink = {
      onAnnotation: () => () => {},
      onAdmitted: () => () => {},
      onRevoked: () => () => {},
      allowed: (id) => allowed.has(id),
      recipients: () => [{ admissionId: "b", rowCode: "AAA" }],
      sendAnnotations: async (_, records) => {
        if (drop) return false;
        recipient.receive(records);
        return true;
      },
    };
    const h = new HostAnnotations(l, { now: () => t });
    h.setViewers(true);
    await h.flush();
    t = 1000;
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation({ kind: "stroke", id: 1, end: true, points: [{ x: 0.2, y: 0.3 }] })) });
    await h.flush();
    expect(recipient.store.origins.has("K7P")).toBe(true);
    t = 2000;
    drop = true;
    allowed.delete("a");
    h.revoke({ admissionIds: ["a"], rowCode: "K7P" });
    await h.flush();
    drop = false;
    for (t = 3000; t < 40000; t += 1000) await h.flush();
    expect(recipient.store.origins.has("K7P")).toBe(false);
    recipient.dispose();
  });

  function pair() {
    const clock = { t: 0, drop: false };
    const recipient = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => "AAA" }, { now: () => clock.t });
    const l: HostLink = {
      onAnnotation: () => () => {},
      onAdmitted: () => () => {},
      onRevoked: () => () => {},
      allowed: () => true,
      recipients: () => [{ admissionId: "b", rowCode: "AAA" }],
      sendAnnotations: async (_, records) => (clock.drop ? false : (recipient.receive(records), true)),
    };
    return { clock, recipient, h: new HostAnnotations(l, { now: () => clock.t }) };
  }
  const stroke = (id: number) => ({ kind: "stroke" as const, id, end: true, points: [{ x: 0.5, y: 0.5 }] });

  it("a lost clear followed by new ink from the same origin: the recipient ends with exactly the new ink", async () => {
    const { clock, recipient, h } = pair();
    h.draw(stroke(1));
    await h.flush();
    clock.t = 1000;
    clock.drop = true;
    h.draw({ kind: "clear", scope: "own" }); // the host clears its own ink; this batch is lost
    await h.flush();
    clock.drop = false;
    clock.t = 2000;
    h.draw(stroke(2)); // new ink right after
    await h.flush(); // the state record leads this batch: the missed clear is applied first
    const strokes = [...(recipient.store.origins.get("host")?.strokes.keys() ?? [])];
    expect(strokes).toEqual([2]);
    expect([...h.store.origins.get("host")!.strokes.keys()]).toEqual([2]);
    recipient.dispose();
  });

  it("a stale or duplicate clear never erases newer ink", () => {
    const recipient = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => "AAA" });
    const rec = (origin: string, m: Parameters<typeof encodeAnnotation>[0]) => ({ origin, body: encodeAnnotation(m) });
    recipient.receive(fixtureBatch([rec("host", { kind: "clear", scope: "all", gen: 3 })]));
    recipient.receive(fixtureBatch([rec("K7P", stroke(1))]));
    recipient.receive(fixtureBatch([rec("host", { kind: "clear", scope: "all", gen: 3 })])); // duplicate
    recipient.receive(fixtureBatch([rec("host", { kind: "clear", scope: "all", gen: 2 })])); // stale, delayed
    expect(recipient.store.origins.has("K7P")).toBe(true);
    recipient.receive(fixtureBatch([rec("K7P", { kind: "clear", scope: "own", gen: 1 })]));
    recipient.receive(fixtureBatch([rec("K7P", stroke(2))]));
    recipient.receive(fixtureBatch([rec("K7P", { kind: "clear", scope: "own", gen: 1 })])); // replayed
    expect([...recipient.store.origins.get("K7P")!.strokes.values()].map(s => s.id)).toEqual([2]);
    // A state record with the same generations changes nothing; one that no longer lists K7P drops it.
    recipient.receive(fixtureBatch([rec("host", { kind: "state", gen: 3, partial: false, origins: [{ origin: "K7P", gen: 1 }] })]));
    expect(recipient.store.origins.has("K7P")).toBe(true);
    recipient.receive(fixtureBatch([rec("host", { kind: "state", gen: 3, partial: false, origins: [] })]));
    expect(recipient.store.origins.has("K7P")).toBe(false);
    recipient.dispose();
  });

  it("only the host can send a state record; a partial list drops nothing unlisted", () => {
    const recipient = new ViewerAnnotations({ seal: async (b) => b, send: () => {}, rowCode: () => "AAA" });
    const rec = (origin: string, m: Parameters<typeof encodeAnnotation>[0]) => ({ origin, body: encodeAnnotation(m) });
    recipient.receive(fixtureBatch([rec("K7P", stroke(1)), rec("M3X", stroke(1))]));
    recipient.receive(fixtureBatch([rec("M3X", { kind: "state", gen: 9, partial: false, origins: [] })])); // forged by a viewer: ignored
    expect(recipient.store.origins.size).toBe(2);
    recipient.receive(fixtureBatch([rec("host", { kind: "state", gen: 0, partial: true, origins: [{ origin: "K7P", gen: 0 }] })]));
    expect(recipient.store.origins.size).toBe(2);
    recipient.dispose();
  });

  it("an idle host-only stream with no ink sends no resync traffic", async () => {
    const { clock, h } = pair();
    let sends = 0;
    (h as unknown as { link: HostLink }).link.sendAnnotations = async () => (sends++, true);
    for (clock.t = 40_000; clock.t < 100_000; clock.t += 1000) await h.flush();
    expect(sends).toBe(0);
  });

  it("the drawer gets policy-off and clears too, though its own ink is filtered out (e2e finding, round 2)", async () => {
    const clock = { t: 0 };
    const sent: { to: string; kinds: string[] }[] = [];
    const l: HostLink = {
      onAnnotation: () => () => {},
      onAdmitted: () => () => {},
      onRevoked: () => () => {},
      allowed: () => true,
      recipients: () => [{ admissionId: "a", rowCode: "K7P" }, { admissionId: "b", rowCode: "AAA" }],
      sendAnnotations: async (to, records) => (sent.push({ to, kinds: records.map((r) => r.origin) }), true),
    };
    const h = new HostAnnotations(l, { now: () => clock.t });
    h.setViewers(true);
    await h.flush();
    clock.t = 1000;
    h.receive({ admissionId: "a", rowCode: "K7P", body: viewerBody(encodeAnnotation(stroke(1))) });
    await h.flush();
    h.clear(); // clears every origin: no ink left, and no state record needed beyond the clear's own
    clock.t = 2000;
    await h.flush();
    sent.length = 0;
    clock.t = 3000;
    h.setViewers(false); // no ink, no clears: the batch is the policy alone
    await h.flush();
    expect(sent.map((s) => s.to).sort()).toEqual(["a", "b"]);
  });
});
