// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NOW, stream } from "./fixtures";
import { mergeStreams, Streams } from "./Streams";
import type { Stream } from "./types";

// A fake /api/v1/streams with the server's ordering and offset cursors.
let rows: Stream[] = [];
const isEnded = (s: Stream) => s.ended_at !== null;
function serve() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const u = new URL(url, "http://x");
      const state = u.searchParams.get("state");
      const limit = Number(u.searchParams.get("limit"));
      const c = u.searchParams.get("cursor");
      const offset = c ? Number(atob(c)) : 0;
      const all = rows.filter((s) => (state === "ended" ? isEnded(s) : !isEnded(s))).sort((a, b) => b.created_at - a.created_at);
      const page = all.slice(offset, offset + limit);
      const next = offset + limit < all.length ? btoa(String(offset + limit)) : null;
      return new Response(JSON.stringify({ streams: page, next_cursor: next }), { status: 200 });
    }),
  );
}

const mk = (i: number, ended: boolean) =>
  stream({
    id: `s_${String(i).padStart(20, "0")}`,
    label: `S${i}`,
    created_at: NOW - i * 60_000,
    ended_at: ended ? NOW - 1000 : null,
    phase: ended ? "ended" : "live",
    effective: null,
  });

const ids = (testid: string) => within(screen.getByTestId(testid)).getAllByTestId("stream-row").map((r) => r.textContent);

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
  // 60 live (more than one page of 50), 120 ended.
  rows = [...Array.from({ length: 60 }, (_, i) => mk(i, false)), ...Array.from({ length: 120 }, (_, i) => mk(100 + i, true))];
  serve();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Streams list", () => {
  it("shows every live stream, pages history without duplicates across polls", async () => {
    render(<Streams />);
    await waitFor(() => expect(screen.getByTestId("live-streams").children).toHaveLength(60));
    expect(screen.getByText("Live · 60")).toBeInTheDocument();
    expect(screen.getByTestId("ended-streams").children).toHaveLength(50);

    fireEvent.click(screen.getByRole("button", { name: "Older streams" }));
    await waitFor(() => expect(screen.getByTestId("ended-streams").children).toHaveLength(100));

    // Three live streams end: their rows move to history and shift every
    // later offset by three.
    for (const s of rows.slice(0, 3)) Object.assign(s, { ended_at: NOW, phase: "ended" });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    await waitFor(() => expect(screen.getByTestId("live-streams").children).toHaveLength(57));
    // The refresh kept the older page and the pagination cursor.
    expect(screen.getByTestId("ended-streams").children).toHaveLength(103);

    fireEvent.click(screen.getByRole("button", { name: "Older streams" }));
    await waitFor(() => expect(screen.getByTestId("ended-streams").children).toHaveLength(123));
    const ended = ids("ended-streams");
    expect(new Set(ended).size).toBe(ended.length);
    expect(screen.queryByRole("button", { name: "Older streams" })).toBeNull();
  });

  it("mergeStreams replaces by id and keeps server order", () => {
    const a = mk(1, true),
      b = mk(2, true),
      c = mk(3, true);
    const merged = mergeStreams([c, b], [{ ...b, label: "fresh" }, a]);
    expect(merged.map((s) => s.label)).toEqual(["S1", "fresh", "S3"]);
  });
});
