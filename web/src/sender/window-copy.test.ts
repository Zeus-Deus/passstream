import { describe, expect, it } from "vitest";
import { commandError, PHASE_COPY, shareError } from "./index";
import { ownerActionMessage } from "./owner-actions";

describe("sender window copy", () => {
  it("treats a cancelled picker as a choice, not an error", () => {
    const r = shareError(new DOMException("Permission denied", "NotAllowedError"));
    expect(r.error).toBe(false);
    expect(r.text).not.toMatch(/NotAllowedError|Permission denied/);
  });

  it("reports a real capture failure as an error", () => {
    expect(shareError(new DOMException("Could not start video source", "NotReadableError")).error).toBe(true);
    const other = shareError(new Error("boom"));
    expect(other.error).toBe(true);
    expect(other.text).toContain("boom");
  });

  it("explains refused commands in words and keeps unknown codes visible", () => {
    expect(commandError("record.start", "paused")).toMatch(/Start sharing first/);
    expect(commandError("kick", "unknown_admission")).toBe("That viewer already left.");
    expect(commandError("pin.rotate", "brand_new_code")).toBe("Couldn't pin rotate (brand_new_code).");
  });

  it("labels every sender phase", () => {
    for (const phase of ["starting", "picking", "live", "paused", "ended"]) expect(PHASE_COPY[phase]?.label).toBeTruthy();
    expect(PHASE_COPY.picking.label).toBe("Not sharing yet");
  });

  it("never shows a raw capture reply", () => {
    expect(ownerActionMessage("record.start", { ok: true, data: { recording: true, bytes: 0 } })).toMatch(/^Recording\./);
    expect(ownerActionMessage("record.start", { ok: false, error: { code: "paused" } })).toMatch(/no picture to capture/);
    expect(ownerActionMessage("publish", { ok: true, data: { url: "https://example.test/p/1" } })).toBe("Published.");
    expect(ownerActionMessage("publish", { ok: false, error: { code: "mystery" } })).toBe("That didn't work (mystery).");
  });
});
