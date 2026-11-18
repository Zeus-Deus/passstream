// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";
import { stream } from "./fixtures";
import { History } from "./History";

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
