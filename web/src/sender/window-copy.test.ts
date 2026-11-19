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

  it("distinguishes current capture protection from saving it for reload", () => {
    expect(commandError("safe", "storage", { applied: true })).toMatch(/on for this capture.*after reload/i);
    expect(commandError("safe", "storage", { applied: false })).toMatch(/wasn't applied/);
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

  it("says a recording was saved only when one was", () => {
    expect(ownerActionMessage("record.stop", { ok: true, data: { recording: false, completed: true, bytes: 4096, error: "" } })).toBe("Recording saved on this machine.");
    // The privacy curtain erased it: record.stop still answers ok.
    expect(ownerActionMessage("record.stop", { ok: true, data: { recording: false, completed: false, bytes: 0, error: "" } })).toBe("No recording was saved.");
    expect(ownerActionMessage("record.stop", { ok: true, data: { completed: false, error: "recording_too_large" } })).toMatch(/too large/);
  });

  it("reports a publish as published only with a link", () => {
    expect(ownerActionMessage("publish", { ok: true, data: { status: "refused", uploading: false } })).toBe("Not published.");
    expect(ownerActionMessage("publish", { ok: true, data: { status: "timeout", uploading: false } })).toMatch(/confirmation ran out/);
    const failed = ownerActionMessage("publish", { ok: true, data: { status: "approved", uploading: false, error: { code: "upload_failed", message: "Pass service returned HTTP 500" } } });
    expect(failed).toMatch(/upload failed/i);
    expect(failed).not.toMatch(/Published|Done/);
  });
});
