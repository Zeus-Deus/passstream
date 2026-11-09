// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { OwnerCapture } from "./owner-capture";

let encode: BlobCallback | undefined;
let active: OwnerCapture | undefined;
const gate = { live: true, paused: false, suspended: false, attached: true };
const send = vi.fn((_v: unknown) => true);
const transfer = vi.fn(async () => true);
let latestRecorder: FakeRecorder;
class FakeRecorder {
  static isTypeSupported() { return true; }
  state = "inactive";
  ondataavailable?: (e: { data: Blob }) => void;
  onstop?: () => void;
  onerror?: () => void;
  constructor() { latestRecorder = this; }
  start() { this.state = "recording"; }
  stop() { this.state = "inactive"; this.onstop?.(); }
}

beforeEach(() => {
  vi.useFakeTimers();
  Object.assign(gate, { live: true, paused: false, suspended: false, attached: true });
  send.mockClear(); transfer.mockReset().mockResolvedValue(true); encode = undefined;
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue();
 vi.spyOn(HTMLMediaElement.prototype,"pause").mockImplementation(()=>{});
  vi.spyOn(HTMLMediaElement.prototype, "readyState", "get").mockReturnValue(2);
  vi.spyOn(HTMLVideoElement.prototype, "videoWidth", "get").mockReturnValue(2);
  vi.spyOn(HTMLVideoElement.prototype, "videoHeight", "get").mockReturnValue(2);
  const context: Pick<CanvasRenderingContext2D, "drawImage" | "getImageData"> = { drawImage: () => {}, getImageData: () => ({ data: new Uint8ClampedArray(16), width: 2, height: 2, colorSpace: "srgb" }) };
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(context as CanvasRenderingContext2D);
  vi.spyOn(HTMLCanvasElement.prototype, "toBlob").mockImplementation((cb) => { encode = cb; });
  vi.stubGlobal("MediaRecorder", FakeRecorder);
  vi.stubGlobal("MediaStreamTrack", class extends EventTarget { enabled = true; muted = false; readyState = "live"; });
  vi.stubGlobal("MediaStream", class { private track = new MediaStreamTrack(); getVideoTracks() { return [this.track]; } getTracks() { return [this.track]; } });
  active = new OwnerCapture(() => gate, send, transfer);
  active.source(new MediaStream()); active.sync(); active.demand(Date.now()+30_000);
});
afterEach(() => { active?.stop(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it("invalidates an in-flight PNG before a pause acknowledgement", async () => {
  vi.advanceTimersByTime(500);
  expect(encode).toBeTypeOf("function");
  gate.paused = true; active!.sync();
  const blob = { size: 4, arrayBuffer: async () => new Uint8Array([1, 2, 3, 4]).buffer } as Blob;
  encode!(blob);
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  expect(send.mock.calls.map(([v]) => v)).not.toEqual(expect.arrayContaining([expect.objectContaining({ status: "frame" })]));
  expect(send).toHaveBeenLastCalledWith({ type: "capture", status: "paused" });
});

it("retries unchanged pixels after the daemon rejects a PNG", async () => {
  transfer.mockResolvedValueOnce(false);
  const blob = { size: 4, arrayBuffer: async () => new Uint8Array([1, 2, 3, 4]).buffer } as Blob;
  vi.advanceTimersByTime(500); encode!(blob);
  await vi.advanceTimersByTimeAsync(0);
  vi.advanceTimersByTime(500);
  expect(HTMLCanvasElement.prototype.toBlob).toHaveBeenCalledTimes(2);
  encode!(blob); await vi.advanceTimersByTimeAsync(0);
  vi.advanceTimersByTime(500);
  expect(HTMLCanvasElement.prototype.toBlob).toHaveBeenCalledTimes(2);
});

it("stops and erases recording when the privacy curtain closes", async () => {
  expect((await active!.command("record.start", { duration: 10 })).ok).toBe(true);
  latestRecorder.ondataavailable?.({ data: new Blob(["private pixels"]) });
  gate.paused = true; active!.sync();
  await Promise.resolve();
  expect(latestRecorder.state).toBe("inactive");
  expect(active!.status()).toMatchObject({ recording: false, completed: false, bytes: 0 });
  expect(transfer).not.toHaveBeenCalled();
});

it("discards an oversized recording instead of transferring a truncated file", async () => {
  await active!.command("record.start", { max_bytes: 1024 });
  latestRecorder.ondataavailable?.({ data: new Blob([new Uint8Array(1025)]) });
  await Promise.resolve();
  expect(active!.status()).toMatchObject({ completed: false, bytes: 0, error: "recording_too_large" });
  expect(transfer).not.toHaveBeenCalled();
});

it("stops recording at its owner-selected duration", async () => {
  await active!.command("record.start", { duration: 1 });
  expect(latestRecorder.state).toBe("recording");
  vi.advanceTimersByTime(1000);
  expect(latestRecorder.state).toBe("inactive");
});

it("refuses recording while paused", async () => {
  gate.paused = true;
  expect(await active!.command("record.start", {})).toMatchObject({ ok: false, error: { code: "paused" } });
});

it("does no readback or PNG encoding without demand and drops the expired feed", async()=>{
 active!.demand(0);vi.advanceTimersByTime(5000);
 expect(HTMLCanvasElement.prototype.getContext).not.toHaveBeenCalled();
 expect(HTMLCanvasElement.prototype.toBlob).not.toHaveBeenCalled();
 active!.demand(Date.now()+1000);vi.advanceTimersByTime(500);
 expect(HTMLCanvasElement.prototype.toBlob).toHaveBeenCalledTimes(1);
 encode!({size:4,arrayBuffer:async()=>new Uint8Array([1,2,3,4]).buffer} as Blob);
 await vi.advanceTimersByTimeAsync(0);vi.advanceTimersByTime(1000);
 const reads=vi.mocked(HTMLCanvasElement.prototype.getContext).mock.calls.length;
 vi.advanceTimersByTime(5000);
 expect(HTMLCanvasElement.prototype.getContext).toHaveBeenCalledTimes(reads);
 expect(send).toHaveBeenLastCalledWith({type:"capture",status:"idle"});
});
it("invalidates a previous picture when the source exceeds 3840 pixels",()=>{
 vi.spyOn(HTMLVideoElement.prototype,"videoWidth","get").mockReturnValue(3841);
 vi.advanceTimersByTime(500);
 expect(send).toHaveBeenLastCalledWith({type:"capture",status:"too_large"});
 expect(HTMLCanvasElement.prototype.toBlob).not.toHaveBeenCalled();
});
it("reports an oversized PNG instead of retaining an unchanged picture",async()=>{
 vi.advanceTimersByTime(500);encode!({size:(4<<20)+1} as Blob);
 await vi.advanceTimersByTimeAsync(0);
 expect(send).toHaveBeenLastCalledWith({type:"capture",status:"too_large"});
 expect(transfer).not.toHaveBeenCalled();
});
