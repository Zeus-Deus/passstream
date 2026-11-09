// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, renderHook, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Player, useWakeLock } from "./Player";
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
