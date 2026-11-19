// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { NOW, stream } from "./fixtures";
import { History } from "./History";
import type { HistoryEntry } from "./types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function record(i: number): HistoryEntry {
  return {
    row_id: `row-${i}`,
    viewer_id: null,
    row_code: null,
    admission_ids: [],
    first_at: NOW + i - 1000,
    last_at: NOW + i,
    end: "left",
    connections: [{ connection_id: `viewer-${i}`, joined_at: NOW + i - 1000, left_at: NOW + i, ip: null, geo: null, device: `Viewer ${i}` }],
  };
}

it("starts with the latest history and appends older entries, with a retry for a failed page", async () => {
  const s = stream();
  const all = Array.from({ length: 105 }, (_, i) => record(105 - i));
  const nextCursor = "cursor-for-older";
  let resolveFirstAttempt!: (response: Response) => void;
  const firstAttempt = new Promise<Response>((resolve) => { resolveFirstAttempt = resolve; });
  const loadOlder = vi.fn(async () => Response.json({ entries: all.slice(100), next_cursor: null }));
  loadOlder.mockImplementationOnce(() => firstAttempt);
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    const u = new URL(url, "http://test");
    if (u.pathname === "/api/v1/streams") return Response.json({ streams: [s], next_cursor: null });
    if (u.pathname === "/api/v1/audit") return Response.json({ entries: [], next_cursor: null });
    expect(u.pathname).toBe(`/api/v1/streams/${s.id}/history`);
    expect(u.searchParams.get("order")).toBe("newest");
    expect(u.searchParams.get("limit")).toBe("100");
    const cursor = u.searchParams.get("cursor");
    if (!cursor) return Response.json({ entries: all.slice(0, 100), next_cursor: nextCursor });
    expect(cursor).toBe(nextCursor);
    return loadOlder();
  }));

  render(<History stream={s.id} />);
  const history = within(await screen.findByTestId("history"));
  const rows = () => history.getAllByRole("listitem");
  expect(rows()).toHaveLength(100);
  expect(rows()[0]).toHaveTextContent(/Viewer 105$/);
  expect(rows().at(-1)).toHaveTextContent(/Viewer 6$/);

  const more = screen.getByRole("button", { name: "More" });
  fireEvent.click(more);
  expect(more).toBeDisabled();
  fireEvent.click(more);
  expect(loadOlder).toHaveBeenCalledTimes(1);
  resolveFirstAttempt(Response.json({ error: { code: "internal", message: "Could not load older entries" } }, { status: 500 }));
  await screen.findByRole("alert");
  expect(rows()).toHaveLength(100);
  expect(more).toBeEnabled();

  fireEvent.click(more);
  await waitFor(() => expect(rows()).toHaveLength(105));
  expect(rows()[0]).toHaveTextContent(/Viewer 105$/);
  expect(rows()[99]).toHaveTextContent(/Viewer 6$/);
  expect(rows()[100]).toHaveTextContent(/Viewer 5$/);
  expect(rows().at(-1)).toHaveTextContent(/Viewer 1$/);
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.queryByRole("button", { name: "More" })).toBeNull();
});
