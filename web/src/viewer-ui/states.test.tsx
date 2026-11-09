// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { isDegraded, qualityBars, type Quality } from "./Player";
import { rowCodeColour } from "./rowcode";
import { missingFeatures, ViewerScreen, type ViewerState } from "./states";

afterEach(cleanup);

const GOOD: Quality = { rtt_ms: 40, loss_pct: 0.1, bitrate_kbps: 2000, fps: 15, path: "direct UDP", layer: "L1T3", freezes: 0 };
const WEAK: Quality = { ...GOOD, rtt_ms: 260, loss_pct: 6 };

describe("ViewerScreen draws every state", () => {
  it.each<[ViewerState, RegExp]>([
    [{ kind: "waiting" }, /host is away/i],
    [{ kind: "connecting" }, /opening the stream/i],
    [{ kind: "ended", reason: "stopped" }, /stopped sharing/i],
    [{ kind: "ended", reason: "expired" }, /link has expired/i],
    [{ kind: "ended", reason: "admission_timeout" }, /didn't finish the check/i],
    [{ kind: "removed", reason: "kicked" }, /you were removed/i],
    [{ kind: "removed", reason: "locked" }, /stream is locked/i],
    [{ kind: "removed", reason: "denied" }, /request declined/i],
    [{ kind: "unsupported", missing: ["RTCRtpScriptTransform"] }, /can't decrypt/i],
  ])("%j", (state, text) => {
    render(<ViewerScreen state={state} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(text);
  });

  it("removal describes a session ending, not a permanent ban", () => {
    render(<ViewerScreen state={{ kind: "removed", reason: "kicked" }} />);
    const card = screen.getByTestId("state-removed-kicked");
    expect(card).toHaveTextContent("ended your session");
    expect(card).not.toHaveTextContent(/can't rejoin/i);
  });

  it("labels end-to-end encryption as beta", () => {
    render(<ViewerScreen state={{ kind: "waiting" }} />);
    expect(screen.getByText(/end-to-end encrypted \(beta\)/)).toBeInTheDocument();
  });

  it("knock pending shows the viewer's own row code", () => {
    render(<ViewerScreen state={{ kind: "knock", code: "K7P" }} />);
    expect(screen.getByText(/Tell the host your code:/)).toHaveTextContent("Tell the host your code: K7P");
    expect(screen.getByTestId("own-code")).toHaveTextContent(`K7P· ${rowCodeColour("K7P")}`);
  });

  it("unsupported fails closed and says so", () => {
    render(<ViewerScreen state={{ kind: "unsupported", missing: ["RTCRtpScriptTransform (encrypted media)"] }} />);
    expect(screen.getByText(/never falls back to an unencrypted stream/)).toBeInTheDocument();
    expect(screen.getByText("RTCRtpScriptTransform (encrypted media)")).toBeInTheDocument();
  });

  it("live shows the LIVE badge and no state badge", () => {
    render(<ViewerScreen state={{ kind: "live" }} quality={GOOD} />);
    expect(screen.getByTestId("live-badge")).toBeInTheDocument();
    expect(screen.queryByTestId("state-badge")).toBeNull();
    expect(screen.getByTestId("video")).toHaveAttribute("playsinline");
    expect((screen.getByTestId("video") as HTMLVideoElement).muted).toBe(true);
  });

  it("paused covers the picture opaquely (no last-frame leak); reconnecting keeps the frame", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "paused" }} quality={GOOD} />);
    const cover = screen.getByTestId("paused-cover");
    expect(cover).toHaveTextContent("Paused by the host");
    expect(cover).toHaveClass("bg-night", "absolute", "inset-0");
    // The cover sits above the zoom layer that holds the video, in the same stage.
    const layer = screen.getByTestId("zoom-layer");
    expect(layer.compareDocumentPosition(cover) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(cover.parentElement).toBe(layer.parentElement);
    expect(screen.queryByTestId("live-badge")).toBeNull();
    expect(screen.queryByTestId("state-badge")).toBeNull();
    rerender(<ViewerScreen state={{ kind: "reconnecting" }} quality={GOOD} />);
    expect(screen.queryByTestId("paused-cover")).toBeNull();
    expect(screen.getByTestId("state-badge")).toHaveTextContent("Reconnecting");
    expect(screen.getByTestId("video")).toBeInTheDocument();
  });

  it("reconnect copy is calm and names the cause, never an error", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "reconnecting", cause: "network", long: true }} />);
    expect(screen.getByTestId("state-badge")).toHaveTextContent("Still reconnecting");
    expect(screen.getByTestId("state-badge")).toHaveTextContent("you don't need to reload");
    rerender(<ViewerScreen state={{ kind: "reconnecting", cause: "host", long: false }} />);
    expect(screen.getByTestId("state-badge")).toHaveTextContent("Waiting for the host");
    for (const word of [/error/i, /failed/i, /lost/i]) expect(screen.getByTestId("state-badge")).not.toHaveTextContent(word);
  });

  it("admitted and slow connecting say so without alarm", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "admitted" }} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Starting the picture");
    expect(screen.queryByTestId("slow")).toBeNull();
    rerender(<ViewerScreen state={{ kind: "connecting", slow: true }} />);
    expect(screen.getByTestId("slow")).toHaveTextContent("keeps trying by itself");
  });

  it("moves focus to each new state's heading", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "connecting" }} />);
    expect(document.activeElement).toBe(screen.getByRole("heading", { level: 1 }));
    rerender(<ViewerScreen state={{ kind: "removed", reason: "kicked" }} />);
    expect(document.activeElement).toHaveTextContent("You were removed");
  });
});

describe("Player controls", () => {
  it("offers Tap to unmute only when there's an audio track", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "live" }} quality={GOOD} />);
    expect(screen.queryByTestId("unmute")).toBeNull();
    rerender(<ViewerScreen state={{ kind: "live" }} quality={GOOD} hasAudio />);
    fireEvent.click(screen.getByTestId("unmute"));
    expect((screen.getByTestId("video") as HTMLVideoElement).muted).toBe(false);
    expect(screen.queryByTestId("unmute")).toBeNull();
  });

  it("shows the quality pill only when degraded, with the HUD on tap", () => {
    const { rerender } = render(<ViewerScreen state={{ kind: "live" }} quality={GOOD} />);
    expect(screen.queryByTestId("quality-pill")).toBeNull();
    rerender(<ViewerScreen state={{ kind: "live" }} quality={WEAK} />);
    expect(screen.queryByTestId("hud")).toBeNull();
    fireEvent.click(screen.getByTestId("quality-pill"));
    expect(screen.getByTestId("hud")).toHaveTextContent("260 ms");
  });

  it("keys: m toggles sound, z zooms, Escape closes the HUD", () => {
    render(<ViewerScreen state={{ kind: "live" }} quality={WEAK} hasAudio initialHud />);
    const v = screen.getByTestId("video") as HTMLVideoElement;
    fireEvent.keyDown(window, { key: "m" });
    expect(v.muted).toBe(false);
    fireEvent.keyDown(window, { key: "m" });
    expect(v.muted).toBe(true);
    fireEvent.keyDown(window, { key: "z" });
    expect(Number(screen.getByTestId("zoom-layer").dataset.scale)).toBeGreaterThan(1);
    fireEvent.keyDown(window, { key: "z" });
    expect(screen.getByTestId("zoom-layer").dataset.scale).toBe("1.00");
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByTestId("hud")).toBeNull();
  });

  it("uses no hover-only controls: every control is a real button", () => {
    render(<ViewerScreen state={{ kind: "live" }} quality={WEAK} hasAudio rowCode="K7P" />);
    for (const name of ["Connection details", "Zoom in", "Unmute"]) expect(screen.getByRole("button", { name })).toBeInTheDocument();
    expect(screen.getByTestId("chrome-code")).toHaveTextContent("K7P");
  });
});

describe("quality", () => {
  it("bars and degraded", () => {
    expect(qualityBars(null)).toBe(-1);
    expect(qualityBars(GOOD)).toBe(4);
    expect(isDegraded(GOOD)).toBe(false);
    expect(isDegraded(WEAK)).toBe(true);
    expect(isDegraded({ ...GOOD, fps: 2 })).toBe(true);
  });
});

describe("missingFeatures", () => {
  it("fails closed without RTCRtpScriptTransform", () => {
    expect(missingFeatures({ RTCPeerConnection: function () {}, crypto: { subtle: {} } } as never)).toEqual(["RTCRtpScriptTransform (encrypted media)"]);
    expect(missingFeatures({ RTCPeerConnection: function () {}, RTCRtpScriptTransform: function () {}, crypto: { subtle: {} } } as never)).toEqual([]);
  });
});
