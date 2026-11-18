// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";
import { stream } from "./fixtures";
import { History } from "./History";
import type { AuditEntry, HistoryEntry } from "./types";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("keeps an older deep link selected and loads older options on demand", async () => {
  const first = Array.from({ length: 50 }, (_, i) => stream({ id: `recent-${i}`, label: `Recent ${i}` }));
  const older = stream({ id: "older", label: "Prior design review" });
  const pages = vi.spyOn(api, "streams")
    .mockResolvedValueOnce({ streams: first, next_cursor: "older-page" })
    .mockResolvedValueOnce({ streams: [first[49]!, older], next_cursor: null });
  const detail = vi.spyOn(api, "stream").mockResolvedValue(older);
  vi.spyOn(api, "audit").mockResolvedValue({ entries: [], next_cursor: null });
  vi.spyOn(api, "history").mockResolvedValue({ entries: [], next_cursor: null });

  render(<History stream={older.id} />);
  const filter = screen.getByRole("combobox", { name: "Stream" });
  await screen.findByRole("option", { name: /^Recent 0 ·/ });
  expect(filter).toHaveValue(older.id);
  expect(await screen.findByRole("option", { name: /^Prior design review ·/, selected: true })).toBeVisible();
  expect(detail).toHaveBeenCalledWith(older.id);
  expect(pages).toHaveBeenCalledTimes(1);

  fireEvent.click(screen.getByRole("button", { name: "Older streams" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Older streams" })).toBeNull());
  expect(pages).toHaveBeenLastCalledWith("all", "older-page");
  expect(screen.getAllByRole("option")).toHaveLength(52);
  expect(filter).toHaveValue(older.id);
});

it("keeps recent choices available and retries the same page after a failed request", async () => {
  const recent = stream({ id: "recent", label: "This week" });
  const older = stream({ id: "older", label: "Last week" });
  const pages = vi.spyOn(api, "streams")
    .mockResolvedValueOnce({ streams: [recent], next_cursor: "next" })
    .mockRejectedValueOnce(new Error("Connection interrupted"))
    .mockResolvedValueOnce({ streams: [older], next_cursor: null });
  const detail = vi.spyOn(api, "stream");
  vi.spyOn(api, "audit").mockResolvedValue({ entries: [], next_cursor: null });
  vi.spyOn(api, "history").mockResolvedValue({ entries: [], next_cursor: null });

  render(<History stream={recent.id} />);
  await screen.findByRole("option", { name: /^This week ·/, selected: true });
  fireEvent.click(screen.getByRole("button", { name: "Older streams" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Connection interrupted");
  expect(screen.getByRole("combobox")).toHaveValue(recent.id);
  fireEvent.click(screen.getByRole("button", { name: "Retry loading streams" }));
  await screen.findByRole("option", { name: /^Last week ·/ });
  expect(pages.mock.calls.map(([, cursor]) => cursor)).toEqual([null, "next", "next"]);
  expect(detail).not.toHaveBeenCalled();
});

it("ignores More responses from the previous stream in viewer history and audit", async () => {
  const a = stream({ id: "a" });
  const b = stream({ id: "b" });
  vi.spyOn(api, "streams").mockResolvedValue({ streams: [a, b], next_cursor: null });
  const viewerRow = (code: string): HistoryEntry => ({
    row_id: code, row_code: code, viewer_id: null, admission_ids: [],
    first_at: 1, last_at: 2, end: "left", connections: [],
  });
  const auditRow = (id: string | null, op: string): AuditEntry => ({
    id: op, at: 1, stream_id: id, actor: { kind: "server", key_id: null },
    op, target: {}, result: "ok", error_code: null, idempotency_key: null,
  });
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  vi.spyOn(api, "history").mockImplementation(async (id, cursor) => {
    if (cursor) await pending;
    const code = id === b.id ? "K7P" : cursor ? "M3X" : "PFT";
    return { entries: [viewerRow(code)], next_cursor: cursor ? null : id };
  });
  vi.spyOn(api, "audit").mockImplementation(async (id, cursor) => {
    if (cursor) await pending;
    const op = id === b.id ? "stop" : cursor ? "kick" : "lock";
    return { entries: [auditRow(id ?? null, op)], next_cursor: cursor ? null : "next" };
  });

  const { rerender } = render(<History stream={a.id} />);
  const buttons = await screen.findAllByRole("button", { name: "More" });
  expect(buttons).toHaveLength(2);
  buttons.forEach((button) => fireEvent.click(button));
  rerender(<History stream={b.id} />);
  await screen.findByText("K7P");
  await screen.findByText("Stop");
  await act(async () => { release(); await pending; });

  expect.soft(screen.queryByText("M3X")).toBeNull();
  expect.soft(screen.queryByText("Kick")).toBeNull();
  expect(screen.queryAllByRole("button", { name: "More" })).toHaveLength(2);
});
