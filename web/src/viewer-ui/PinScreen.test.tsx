// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NAME_KEY, PinScreen, validPassphrase } from "./PinScreen";

function memory(init: Record<string, string> = {}) {
  const m = new Map(Object.entries(init));
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
    m,
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const pin = () => screen.getByTestId("pin-input") as HTMLInputElement;

describe("PinScreen (digits)", () => {
  it("is one numeric one-time-code input drawn as 4 boxes", () => {
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={() => undefined} storage={memory()} />);
    const i = pin();
    expect(screen.getAllByRole("textbox")).toHaveLength(2); // the PIN and the optional name
    expect(i).toHaveAttribute("inputmode", "numeric");
    expect(i).toHaveAttribute("pattern", "[0-9]*");
    expect(i).toHaveAttribute("autocomplete", "one-time-code");
    expect(i).toHaveAttribute("enterkeyhint", "go");
    expect(screen.getByTestId("pin-boxes").firstElementChild!.children).toHaveLength(4);
  });

  it("auto-submits once on the last digit, with the optional name", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} storage={memory()} />);
    fireEvent.change(screen.getByTestId("name-input"), { target: { value: "  Ana " } });
    for (const v of ["4", "48", "482"]) fireEvent.change(pin(), { target: { value: v } });
    expect(onSubmit).not.toHaveBeenCalled();
    fireEvent.change(pin(), { target: { value: "4821" } });
    expect(onSubmit).toHaveBeenCalledExactlyOnceWith("4821", "Ana");
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("handles paste with separators and drops non-digits", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} storage={memory()} />);
    fireEvent.change(pin(), { target: { value: "ab" } });
    expect(pin().value).toBe("");
    fireEvent.change(pin(), { target: { value: "48 21-99" } });
    expect(pin().value).toBe("4821");
    expect(onSubmit).toHaveBeenCalledWith("4821", "");
  });

  it("supports longer PINs", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 6 }} onSubmit={onSubmit} storage={memory()} />);
    expect(screen.getByTestId("pin-boxes").firstElementChild!.children).toHaveLength(6);
    fireEvent.change(pin(), { target: { value: "4821" } });
    expect(onSubmit).not.toHaveBeenCalled();
    fireEvent.change(pin(), { target: { value: "482193" } });
    expect(onSubmit).toHaveBeenCalledWith("482193", "");
  });

  it("clears after a failed try and shows tries left", () => {
    const onSubmit = vi.fn();
    const { rerender } = render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} busy storage={memory()} />);
    rerender(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} error="That PIN didn't match." triesLeft={2} storage={memory()} />);
    expect(pin().value).toBe("");
    expect(screen.getByRole("alert")).toHaveTextContent("That PIN didn't match.");
    expect(screen.getByTestId("pin-hint")).toHaveTextContent("2 tries left");
    // The same digits again are a new try.
    fireEvent.change(pin(), { target: { value: "4821" } });
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("shows a lockout countdown and refuses input until it ends", () => {
    vi.useFakeTimers({ now: 1_000_000 });
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} lockedUntil={1_000_000 + 61_000} storage={memory()} />);
    expect(pin()).toBeDisabled();
    expect(screen.getByTestId("pin-hint")).toHaveTextContent("Try again in 1:01");
    act(() => vi.advanceTimersByTime(30_000));
    expect(screen.getByTestId("pin-hint")).toHaveTextContent("Try again in 0:31");
    act(() => vi.advanceTimersByTime(31_500));
    expect(pin()).toBeEnabled();
    expect(screen.queryByTestId("pin-hint")).toBeNull();
  });

  it("announces the lockout once to screen readers; the ticking countdown is visual only", () => {
    vi.useFakeTimers({ now: 1_000_000 });
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={vi.fn()} error="Too many wrong PINs. Wait, then try again." lockedUntil={1_000_000 + 120_000} storage={memory()} />);
    const hint = screen.getByTestId("pin-hint");
    const spoken = hint.querySelector(".sr-only")!;
    expect(spoken).toHaveTextContent("pausing PIN checks for 2 minutes");
    expect(screen.getByTestId("pin-countdown").closest("[aria-hidden=true]")).not.toBeNull();
    act(() => vi.advanceTimersByTime(10_000));
    expect(spoken).toHaveTextContent("pausing PIN checks for 2 minutes");
    expect(screen.getByTestId("pin-countdown")).toHaveTextContent("1:50");
  });

  it("refocuses the PIN field when a lockout ends", () => {
    vi.useFakeTimers({ now: 1_000_000 });
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={vi.fn()} lockedUntil={1_000_000 + 2_000} storage={memory()} />);
    expect(pin()).toBeDisabled();
    act(() => vi.advanceTimersByTime(2_500));
    expect(pin()).toBeEnabled();
    expect(document.activeElement).toBe(pin());
  });

  it("keeps the PIN boxes while the host checks, and won't submit twice", () => {
    const onSubmit = vi.fn();
    const { rerender } = render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} storage={memory()} />);
    fireEvent.change(pin(), { target: { value: "4821" } });
    rerender(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} busy storage={memory()} />);
    expect(screen.getByRole("button", { name: "Checking…" })).toBeDisabled();
    expect(screen.getByTestId("pin-hint")).toHaveTextContent("Checking with the host");
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("remembers the name on this device", () => {
    const storage = memory({ [NAME_KEY]: "Bo" });
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} storage={storage} />);
    expect(screen.getByTestId("name-input")).toHaveValue("Bo");
    fireEvent.change(screen.getByTestId("name-input"), { target: { value: "Bo K" } });
    fireEvent.change(pin(), { target: { value: "1234" } });
    expect(storage.m.get(NAME_KEY)).toBe("Bo K");
  });

  it.each(["getItem", "setItem", "removeItem"] as const)("joins without remembering the name when storage.%s throws", (broken) => {
    const storage = {
      getItem: () => "Bo",
      setItem: () => undefined,
      removeItem: () => undefined,
      [broken]: () => {
        throw new DOMException("blocked", "SecurityError");
      },
    };
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={onSubmit} storage={storage} />);
    expect(screen.getByTestId("name-input")).toHaveValue(broken === "getItem" ? "" : "Bo");
    // removeItem runs for an empty name, setItem for a non-empty one.
    fireEvent.change(screen.getByTestId("name-input"), { target: { value: broken === "removeItem" ? "" : "Ana" } });
    fireEvent.change(pin(), { target: { value: "1234" } });
    expect(onSubmit).toHaveBeenCalledExactlyOnceWith("1234", broken === "removeItem" ? "" : "Ana");
  });

  it("links the privacy notice", () => {
    render(<PinScreen policy={{ kind: "digits", length: 4 }} onSubmit={() => undefined} storage={memory()} />);
    expect(screen.getByRole("link", { name: /who sees what/i })).toHaveAttribute("href", "/privacy");
  });
});

describe("PinScreen (passphrase)", () => {
  it("submits on go/enter once it's at least 8 bytes", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "passphrase" }} onSubmit={onSubmit} storage={memory()} />);
    expect(pin()).toHaveAttribute("type", "password");
    fireEvent.change(pin(), { target: { value: "short" } });
    fireEvent.submit(pin().form!);
    expect(onSubmit).not.toHaveBeenCalled();
    fireEvent.change(pin(), { target: { value: "correct horse" } });
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenCalledWith("correct horse", "");
  });

  it("can show the passphrase while typing it on a phone", () => {
    render(<PinScreen policy={{ kind: "passphrase" }} onSubmit={vi.fn()} storage={memory()} />);
    const reveal = screen.getByRole("button", { name: "Show" });
    expect(reveal).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(reveal);
    expect(pin()).toHaveAttribute("type", "text");
    expect(screen.getByRole("button", { name: "Hide" })).toHaveAttribute("aria-pressed", "true");
  });

  it("accepts a pasted passphrase and submits on go", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "passphrase" }} onSubmit={onSubmit} storage={memory()} />);
    fireEvent.change(pin(), { target: { value: "Tr0ub4dor & 3" } });
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenCalledWith("Tr0ub4dor & 3", "");
  });

  const join = () => screen.getByRole("button", { name: "Join" });
  const DECOMPOSED_E = "e\u0301"; // 3 UTF-8 bytes; NFC "é" is 2

  it("counts NFC UTF-8 bytes for both Join and submit, and submits the NFC form", () => {
    const onSubmit = vi.fn();
    render(<PinScreen policy={{ kind: "passphrase" }} onSubmit={onSubmit} storage={memory()} />);
    // 10 raw bytes, 7 after NFC: too short.
    fireEvent.change(pin(), { target: { value: DECOMPOSED_E.repeat(3) + "a" } });
    expect(join()).toBeDisabled();
    fireEvent.submit(pin().form!);
    expect(onSubmit).not.toHaveBeenCalled();
    // Four characters, 8 bytes after NFC: long enough, though only 4 UTF-16 units.
    fireEvent.change(pin(), { target: { value: "éééé" } });
    expect(join()).toBeEnabled();
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenLastCalledWith("éééé", "");
    // 384 raw bytes but exactly 256 after NFC: allowed, and sent normalized.
    fireEvent.change(pin(), { target: { value: DECOMPOSED_E.repeat(128) } });
    expect(join()).toBeEnabled();
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenLastCalledWith("é".repeat(128), "");
    // 258 bytes after NFC: too long, with a hint.
    fireEvent.change(pin(), { target: { value: "é".repeat(129) } });
    expect(join()).toBeDisabled();
    expect(screen.getByTestId("pin-hint")).toHaveTextContent("at most 256 bytes");
    fireEvent.submit(pin().form!);
    expect(onSubmit).toHaveBeenCalledTimes(2);
  });

  it("validPassphrase is the one 8–256 byte NFC rule", () => {
    expect(validPassphrase("1234567")).toBe(false);
    expect(validPassphrase("12345678")).toBe(true);
    expect(validPassphrase("🔑🔑")).toBe(true); // 2 code points, 8 bytes
    expect(validPassphrase("x".repeat(256))).toBe(true);
    expect(validPassphrase("x".repeat(257))).toBe(false);
  });
});
