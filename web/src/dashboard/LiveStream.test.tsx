// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LOCAL_ONLY } from "./actions";
import { connection, list, NOW, stream, viewer } from "./fixtures";
import { LiveStream, mergeFailed } from "./LiveStream";
import type { Stream, ViewerList } from "./types";
import { ViewerTable } from "./ViewerTable";

class FakeES {
  static last: FakeES | null = null;
  listeners = new Map<string, ((e: Event) => void)[]>();
  constructor(public url: string) {
    FakeES.last = this;
  }
  addEventListener(t: string, f: (e: Event) => void) {
    this.listeners.set(t, [...(this.listeners.get(t) ?? []), f]);
  }
  close() {}
  emit(t: string, data: unknown) {
    this.listeners.get(t)?.forEach((f) => f(new MessageEvent(t, { data: JSON.stringify(data) })));
  }
}

type Reply = { status: number; body?: unknown };
let commands: { op: string; args: unknown }[] = [];
let commandReply: Reply = { status: 200, body: { id: "x", ok: true, result: {} } };

function serve(s: Stream, v: ViewerList) {
  commands = [];
  vi.stubGlobal("EventSource", FakeES);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const json = (r: Reply) => new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status });
      if (url.endsWith("/commands")) {
        commands.push(JSON.parse(String(init?.body)));
        return json(commandReply);
      }
      if (url.endsWith("/viewers")) return json({ status: 200, body: v });
      if (url.includes("/api/v1/streams/")) return json({ status: 200, body: s });
      return json({ status: 404, body: { error: { code: "not_found", message: "not found" } } });
    }),
  );
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
  commandReply = { status: 200, body: { id: "x", ok: true, result: {} } };
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

async function renderLive(s = stream(), v = list()) {
  serve(s, v);
  render(<LiveStream id={s.id} onSignedOut={() => undefined} />);
  await screen.findByRole("heading", { name: s.label ?? "" });
}

describe("LiveStream", () => {
  it("offers only tightening actions; loosening ones are disabled with the reason", async () => {
    await renderLive(stream({ effective: { ...stream().effective!, access: { ...stream().effective!.access, latches: { locked: true, paused: true, frozen: true } } } }));
    const actions = screen.getByTestId("stream-actions");
    for (const name of ["Resume", "Unlock", "Unfreeze", "Rotate PIN"]) {
      const b = within(actions).getByRole("button", { name });
      expect(b).toBeDisabled();
      expect(b.getAttribute("title")).toBe(LOCAL_ONLY);
    }
    expect(within(actions).getByRole("button", { name: "Stop" })).toBeEnabled();
    expect(within(actions).queryByRole("button", { name: "Pause" })).toBeNull();
    expect(within(actions).queryByRole("button", { name: "Lock" })).toBeNull();
  });

  it("never shows a PIN value or a share link", async () => {
    await renderLive();
    expect(screen.getByText("PIN · 4 digits")).toBeTruthy();
    expect(screen.getByText(/Link is on the host machine/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /copy link/i })).toBeNull();
    expect(screen.queryByText(/#c=|\/v#/)).toBeNull();
  });

  it("renders relayed health and live health updates", async () => {
    const s = stream();
    s.effective!.health = { uplink_kbps: 2400, encode_ms: 4.5, encode_cpu_pct: 13.5, fps: 30, width: 1920, height: 1080, quality_limitation: "cpu" };
    await renderLive(s);
    const health = screen.getByTestId("sender-health");
    expect(health).toHaveTextContent("13.5%");
    expect(health).toHaveTextContent("1920 × 1080");
    expect(health).toHaveTextContent("4.5 ms/frame");
    expect(health).toHaveTextContent("30 fps");
    act(() => FakeES.last!.emit("stream", { ...s, effective: { ...s.effective, health: { ...s.effective!.health, encode_cpu_pct: 20 } } }));
    expect(health).toHaveTextContent("20.0%");
    expect(health).toHaveTextContent("Reported by the sender, via the server");
  });

  it("sends lock and pause as restrictive commands", async () => {
    await renderLive();
    fireEvent.click(screen.getByRole("button", { name: "Lock" }));
    await screen.findByText("Locked: no new viewers.");
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await screen.findByText(/Paused\. Resume on the host machine/);
    expect(commands.map((c) => [c.op, c.args])).toEqual([
      ["lock", { locked: true }],
      ["pause", {}],
    ]);
  });

  it("handles a 403 local_owner_required gracefully", async () => {
    await renderLive();
    commandReply = { status: 403, body: { error: { code: "local_owner_required", message: "local owner required" } } };
    fireEvent.click(screen.getByRole("button", { name: "Lock" }));
    expect(await screen.findByRole("status")).toHaveTextContent(LOCAL_ONLY);
  });

  it("shows the knock queue view-only with Deny", async () => {
    await renderLive();
    const knocks = screen.getByTestId("knocks");
    expect(within(knocks).getByText("Approve on your computer (plugin/CLI)")).toBeTruthy();
    expect(within(knocks).queryByRole("button", { name: /approve/i })).toBeNull();
    expect(within(knocks).getByText(/UPV/)).toBeTruthy();
    fireEvent.click(within(knocks).getByRole("button", { name: "Deny" }));
    await waitFor(() => expect(commands[0]).toEqual(expect.objectContaining({ op: "knock.deny", args: { knock_id: "knock000000000000000000A" } })));
  });

  it("kicks and bans by admission and connection id from the expanded row", async () => {
    await renderLive();
    fireEvent.click(screen.getByRole("button", { name: "Show details for Ana" }));
    fireEvent.click(screen.getByRole("button", { name: "Kick + lock" }));
    await screen.findByText(/Kicked Ana and locked/);
    fireEvent.click(screen.getByRole("button", { name: "Ban IP" }));
    await screen.findByText(/Banned Ana's IP/);
    expect(commands.map((c) => [c.op, c.args])).toEqual([
      ["kick", { admission_id: "adm00000000000000000000A", lock: true }],
      ["ban", { connection_id: "c_conn0000000000000000000A" }],
    ]);
    expect(screen.getByRole("button", { name: "Kick + new PIN" })).toBeDisabled();
  });

  it("batches failed-PIN alerts from the live feed", async () => {
    await renderLive();
    act(() => {
      FakeES.last!.emit("alert", { kind: "failed_pin", count: 4, sources: 1, from_at: NOW - 5000, to_at: NOW - 4000 });
      FakeES.last!.emit("alert", { kind: "failed_pin", count: 3, sources: 2, from_at: NOW - 3000, to_at: NOW - 1000 });
    });
    expect(screen.getByRole("alert")).toHaveTextContent("7 wrong PINs from 2+ sources");
  });

  it("applies viewer snapshots from SSE", async () => {
    await renderLive();
    act(() => FakeES.last!.emit("viewers", list([viewer({ connections: [connection({ reported: { label: "Zed", tab_hidden: false } })] })])));
    expect(screen.getByText("Zed")).toBeTruthy();
    expect(screen.queryByText("Ana")).toBeNull();
  });
});

describe("ViewerTable", () => {
  it("dims left rows, shows eye-off for hidden tabs, and labels names as viewer-reported", () => {
    render(<ViewerTable viewers={list().viewers} now={NOW} onKick={() => undefined} onBan={() => undefined} />);
    const left = document.querySelector('tr[data-status="left"]')!;
    expect(left.className).toContain("opacity-55");
    expect(within(left as HTMLElement).getByText(/left 1m ago/)).toBeTruthy();
    expect(within(left as HTMLElement).getByRole("img", { name: /Tab hidden/ })).toBeTruthy();
    expect(screen.getAllByText("viewer-reported").length).toBeGreaterThan(0);
    // Knocks live in their own queue, not in the table.
    expect(screen.queryByText("Hana")).toBeNull();
  });

  it("shows every field's provenance when expanded", () => {
    render(<ViewerTable viewers={[viewer()]} now={NOW} onKick={() => undefined} onBan={() => undefined} />);
    fireEvent.click(screen.getByRole("button", { name: "Show details for Ana" }));
    const d = screen.getByTestId("viewer-details");
    for (const [label, value, source] of [
      ["IP", "203.0.113.20", "server-observed"],
      ["ASN", "AS3320 Deutsche Telekom AG", "server-observed"],
      ["Path", "direct UDP", "server-observed"],
      ["RTT", "95 ms", "server-observed"],
      ["Loss", "0.4 %", "server-observed"],
      ["Freezes", "1", "server-observed"],
      ["Name", "Ana", "viewer-reported"],
      ["Row code", "PFT", "sender, via server"],
    ]) {
      const dt = within(d).getByText(label, { selector: "dt" });
      expect(dt.nextElementSibling).toHaveTextContent(value);
      expect(dt.nextElementSibling!.nextElementSibling).toHaveTextContent(source);
    }
  });

  it("renders cards instead of a table below 640 px", () => {
    vi.stubGlobal("matchMedia", (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} }));
    render(<ViewerTable viewers={list().viewers} now={NOW} onKick={() => undefined} onBan={() => undefined} />);
    expect(screen.queryByTestId("viewer-table")).toBeNull();
    expect(screen.getByTestId("viewer-cards").children).toHaveLength(2);
  });
});

describe("mergeFailed", () => {
  it("sums counts and treats sources as a lower bound", () => {
    const a = mergeFailed(null, { kind: "failed_pin", count: 2, sources: 2, from_at: 10, to_at: 20 });
    expect(mergeFailed(a, { kind: "failed_pin", count: 5, sources: 1, from_at: 30, to_at: 40 })).toEqual({ count: 7, sources: 2, from_at: 10, to_at: 40 });
  });
});
