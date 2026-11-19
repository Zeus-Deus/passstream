// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, renderHook, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NO_WEAK_RUN, Player, STALE_SAMPLE_MS, useWakeLock, WEAK_HOLD_MS, weakRun, type Quality } from "./Player";
import { ROW_COLOUR_HEX, rowCodeColour } from "./rowcode";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const controls = () => screen.getByRole("button", { name: "Connection details" }).closest(".transition-opacity")!;

describe("row code", () => {
  it("is a tappable button with its colour at every width, repeated in the HUD", () => {
    render(<Player state="live" rowCode="K7P" autoHideMs={0} />);
    const colour = rowCodeColour("K7P")!;
    // The compact chip has no width breakpoint below 560 px; the labelled one takes over above it.
    const compact = screen.getByTestId("chrome-code-compact");
    expect(compact.tagName).toBe("BUTTON");
    expect(compact.className).not.toMatch(/(^|\s)hidden(\s|$)/);
    expect(compact.parentElement!.className).toContain("min-[560px]:hidden");
    expect(screen.getByTestId("chrome-code").className).toContain("min-[560px]:inline-flex");
    expect(compact).toHaveAccessibleName(`Your code K7P, ${colour}. Connection details`);
    expect((compact.querySelector("[aria-hidden=true]") as HTMLElement).style.background).toBe("rgb(142, 78, 198)"); // purple
    expect(ROW_COLOUR_HEX[colour]).toBe("#8e4ec6");
    fireEvent.click(compact);
    expect(screen.getByTestId("hud-code")).toHaveTextContent(`Your codeK7P· ${colour}`);
  });
});

describe("auto-hide and keyboard focus", () => {
  it("reveals the controls on focus and holds them while focus is inside", () => {
    vi.useFakeTimers();
    render(
      <>
        <Player state="live" autoHideMs={1000} />
        <button type="button">outside</button>
      </>,
    );
    act(() => vi.advanceTimersByTime(1100));
    expect(controls()).toHaveClass("opacity-0");

    act(() => screen.getByRole("button", { name: "Connection details" }).focus());
    expect(controls()).toHaveClass("opacity-100");
    act(() => vi.advanceTimersByTime(5000));
    expect(controls()).toHaveClass("opacity-100");
    // Moving between controls keeps them up.
    act(() => screen.getByRole("button", { name: "Zoom in" }).focus());
    act(() => vi.advanceTimersByTime(5000));
    expect(controls()).toHaveClass("opacity-100");

    // Focus leaves: the timer starts again.
    act(() => screen.getByRole("button", { name: "outside" }).focus());
    act(() => vi.advanceTimersByTime(900));
    expect(controls()).toHaveClass("opacity-100");
    act(() => vi.advanceTimersByTime(200));
    expect(controls()).toHaveClass("opacity-0");
  });
});

describe("gestures", () => {
  it("lifting one finger of a pinch leaves the other panning", () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(
      { left: 0, top: 0, width: 400, height: 300, right: 400, bottom: 300, x: 0, y: 0, toJSON() {} } as DOMRect,
    );
    render(<Player state="live" autoHideMs={0} />);
    const stage = screen.getByTestId("stage");
    const layer = screen.getByTestId("zoom-layer");
    const x = () => Number(/translate3d\((-?[\d.]+)px/.exec(layer.style.transform)![1]);

    fireEvent.pointerDown(stage, { pointerId: 1, clientX: 150, clientY: 150 });
    fireEvent.pointerDown(stage, { pointerId: 2, clientX: 250, clientY: 150 });
    fireEvent.pointerMove(stage, { pointerId: 1, clientX: 50, clientY: 150 });
    fireEvent.pointerMove(stage, { pointerId: 2, clientX: 350, clientY: 150 });
    expect(Number(layer.dataset.scale)).toBeCloseTo(3);
    fireEvent.pointerUp(stage, { pointerId: 2, clientX: 350, clientY: 150 });

    const before = x();
    fireEvent.pointerMove(stage, { pointerId: 1, clientX: 100, clientY: 150 });
    expect(x()).toBeCloseTo(before + 50);
    fireEvent.pointerMove(stage, { pointerId: 1, clientX: 90, clientY: 150 });
    expect(x()).toBeCloseTo(before + 40);
    fireEvent.pointerUp(stage, { pointerId: 1, clientX: 90, clientY: 150 });
    expect(Number(layer.dataset.scale)).toBeCloseTo(3);
  });
});

describe("useWakeLock", () => {
  function fakeNav() {
    const locks: { release: ReturnType<typeof vi.fn> }[] = [];
    const resolvers: (() => void)[] = [];
    const nav = {
      wakeLock: {
        request: vi.fn(
          () =>
            new Promise<{ release(): Promise<void>; addEventListener(): void }>((resolve) => {
              const l = { release: vi.fn(async () => undefined), addEventListener: () => undefined };
              locks.push(l);
              resolvers.push(() => resolve(l));
            }),
        ),
      },
    };
    return { nav: nav as unknown as Navigator, locks, resolvers };
  }

  it("releases the lock on unmount", async () => {
    const { nav, locks, resolvers } = fakeNav();
    const { unmount } = renderHook(() => useWakeLock(true, nav));
    await act(async () => resolvers[0]());
    expect(locks[0].release).not.toHaveBeenCalled();
    unmount();
    expect(locks[0].release).toHaveBeenCalledOnce();
  });

  it("releases a lock that's granted after unmount", async () => {
    const { nav, locks, resolvers } = fakeNav();
    const { unmount } = renderHook(() => useWakeLock(true, nav));
    unmount();
    await act(async () => resolvers[0]());
    expect(locks[0].release).toHaveBeenCalledOnce();
  });

  it("releases when the player stops being live, and asks once at a time", async () => {
    const { nav, locks, resolvers } = fakeNav();
    const { rerender, result } = renderHook(({ on }) => useWakeLock(on, nav), { initialProps: { on: true } });
    result.current();
    result.current();
    expect(nav.wakeLock!.request).toHaveBeenCalledOnce();
    await act(async () => resolvers[0]());
    rerender({ on: false });
    expect(locks[0].release).toHaveBeenCalledOnce();
  });
});

describe("fit, fill and zoom readout", () => {
  it("toggles fit (whole picture) and fill (cover, cropped)", () => {
    render(<Player state="live" autoHideMs={0} />);
    const video = screen.getByTestId("video");
    expect(video).toHaveClass("object-contain");
    const t = screen.getByTestId("fill-toggle");
    expect(t).toHaveAccessibleName("Fill the screen (crops the edges)");
    expect(t).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(t);
    expect(video).toHaveClass("object-cover");
    expect(t).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("zoom-layer")).toHaveAttribute("data-fit", "fill");
  });

  it("shows the zoom level while zoomed; tapping it fits", () => {
    render(<Player state="live" autoHideMs={0} />);
    expect(screen.queryByTestId("zoom-level")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(screen.getByTestId("zoom-level")).toHaveTextContent("2.5×");
    fireEvent.click(screen.getByTestId("zoom-level"));
    expect(screen.getByTestId("zoom-layer")).toHaveAttribute("data-scale", "1.00");
    expect(screen.queryByTestId("zoom-level")).toBeNull();
  });
});

describe("paused", () => {
  it("covers the video, drops the LIVE badge and Tap to unmute, and lifts on resume", () => {
    const { rerender } = render(<Player state="paused" hasAudio={false} autoHideMs={0} />);
    expect(screen.getByTestId("paused-cover")).toHaveAttribute("role", "status");
    expect(screen.queryByTestId("live-badge")).toBeNull();
    expect(screen.queryByTestId("unmute")).toBeNull();
    rerender(<Player state="live" hasAudio autoHideMs={0} />);
    expect(screen.queryByTestId("paused-cover")).toBeNull();
    expect(screen.getByTestId("live-badge")).toBeInTheDocument();
  });
});

describe("weak connection pill", () => {
  const FINE = { rtt_ms: 1, loss_pct: 0, bitrate_kbps: 70, fps: 14, path: "direct" as const, layer: null, freezes: 0 };

  it("ignores a blip: one sample of loss or a slow frame rate on a good link", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Player state="live" quality={{ ...FINE, loss_pct: 14, fps: null }} autoHideMs={0} />);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
    act(() => void vi.advanceTimersByTime(1000));
    rerender(<Player state="live" quality={FINE} autoHideMs={0} />);
    act(() => void vi.advanceTimersByTime(WEAK_HOLD_MS * 2));
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  it("shows once the connection has stayed weak, and clears as soon as it recovers", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Player state="live" quality={{ ...FINE, rtt_ms: 400 }} autoHideMs={0} />);
    act(() => void vi.advanceTimersByTime(WEAK_HOLD_MS - 1));
    expect(screen.queryByTestId("quality-pill")).toBeNull();
    // New samples that are still weak don't restart the wait.
    rerender(<Player state="live" quality={{ ...FINE, rtt_ms: 420 }} autoHideMs={0} />);
    act(() => void vi.advanceTimersByTime(1));
    expect(screen.getByTestId("quality-pill")).toHaveTextContent("Weak connection");
    rerender(<Player state="live" quality={FINE} autoHideMs={0} />);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  // Timestamped measurements, as the real viewer delivers them: one every 500 ms.
  const feed = (rerender: (ui: React.ReactElement) => void, q: () => Quality, polls: number) => {
    for (let i = 0; i < polls; i++) {
      act(() => void vi.advanceTimersByTime(500));
      rerender(<Player state="live" quality={{ ...q(), at: Date.now() }} autoHideMs={0} />);
    }
  };
  const SLOW = { ...FINE, rtt_ms: 400 };

  it("doesn't let one weak sample become a warning when sampling stalls", () => {
    vi.useFakeTimers();
    const spike = { ...FINE, loss_pct: 14, at: Date.now() };
    const { rerender } = render(<Player state="live" quality={spike} autoHideMs={0} />);
    // The same measurement keeps being handed over; nothing newer arrives.
    for (let i = 0; i < 12; i++) {
      act(() => void vi.advanceTimersByTime(500));
      rerender(<Player state="live" quality={{ ...spike }} autoHideMs={0} />);
      expect(screen.queryByTestId("quality-pill")).toBeNull();
    }
  });

  it("doesn't finish the hold on the clock when sampling stalls partway through", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Player state="live" quality={{ ...SLOW, at: Date.now() }} autoHideMs={0} />);
    feed(rerender, () => SLOW, 3); // weak at 0, 0.5, 1 and 1.5 s, then nothing
    for (let i = 0; i < 10; i++) {
      act(() => void vi.advanceTimersByTime(500));
      expect(screen.queryByTestId("quality-pill")).toBeNull();
    }
    // Sampling resumes, still weak: the hold starts over from the first new measurement.
    feed(rerender, () => SLOW, 6);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
    feed(rerender, () => SLOW, 1);
    expect(screen.getByTestId("quality-pill")).toHaveTextContent("Weak connection");
  });

  it("counts weak samples that keep arriving, and drops the warning when they stop", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Player state="live" quality={{ ...SLOW, at: Date.now() }} autoHideMs={0} />);
    feed(rerender, () => SLOW, 5);
    expect(screen.queryByTestId("quality-pill")).toBeNull(); // 2.5 s of evidence
    feed(rerender, () => SLOW, 1);
    expect(screen.getByTestId("quality-pill")).toHaveTextContent("Weak connection");
    act(() => void vi.advanceTimersByTime(STALE_SAMPLE_MS));
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  it("drops a frame-rate warning the moment the host pauses, on the same sample", () => {
    vi.useFakeTimers();
    const JERKY = { ...FINE, fps: 2 };
    const { rerender } = render(<Player state="live" quality={{ ...JERKY, at: Date.now() }} autoHideMs={0} />);
    feed(rerender, () => JERKY, 6);
    expect(screen.getByTestId("quality-pill")).toHaveTextContent("Weak connection");
    // Paused with the last measurement unchanged: no new sample, no clock advance.
    const last = { ...JERKY, at: Date.now() };
    rerender(<Player state="paused" quality={last} autoHideMs={0} />);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
    // Resuming on that same sample doesn't bring it back: the run starts over.
    rerender(<Player state="live" quality={{ ...last }} autoHideMs={0} />);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  it("a healthy or unknown measurement ends the run", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Player state="live" quality={{ ...SLOW, at: Date.now() }} autoHideMs={0} />);
    feed(rerender, () => SLOW, 4);
    feed(rerender, () => ({ ...FINE, rtt_ms: null, loss_pct: null }), 1);
    feed(rerender, () => SLOW, 5);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  it("doesn't read a paused host's slow still picture as a weak connection", () => {
    vi.useFakeTimers();
    render(<Player state="paused" quality={{ ...FINE, fps: 1 }} autoHideMs={0} />);
    act(() => void vi.advanceTimersByTime(WEAK_HOLD_MS * 2));
    expect(screen.queryByTestId("quality-pill")).toBeNull();
  });

  it("shows Reconnecting at once", () => {
    render(<Player state="reconnecting" quality={FINE} autoHideMs={0} />);
    expect(screen.getByTestId("quality-pill")).toHaveTextContent("Reconnecting");
  });
});

describe("weakRun", () => {
  it("holds only once weak measurements span the hold by their own timestamps", () => {
    let run = NO_WEAK_RUN;
    for (const at of [0, 500, 1000, 1500, 2000, 2500]) run = weakRun(run, at, true);
    expect(run.held).toBe(false);
    expect(weakRun(run, 3000, true)).toEqual({ since: 0, last: 3000, held: true });
  });

  it("restarts after a gap, however late the next measurement is handed over", () => {
    let run = NO_WEAK_RUN;
    for (const at of [0, 500, 1000, 1500]) run = weakRun(run, at, true);
    run = weakRun(run, 1500 + STALE_SAMPLE_MS, true);
    expect(run).toEqual({ since: 3500, last: 3500, held: false });
  });

  it("ends on a measurement that isn't weak", () => {
    const run = weakRun(weakRun(weakRun(NO_WEAK_RUN, 0, true), 3000, true), 3500, false);
    expect(run).toEqual({ since: null, last: 3500, held: false });
  });
});
