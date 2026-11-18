import { afterEach, describe, expect, it, vi } from "vitest";
import { E2EEError } from "@passstream/e2ee";
import { Session, unmeasuredCleared } from "./session";
import { viewerHud } from "./hud/stats";
import { isDegraded } from "./viewer-ui/Player";

class Socket {
 static OPEN=1;
 static all: Socket[]=[];
 readyState=0;
 onclose?: (event: {code:number})=>void;
 constructor(_url:string) { Socket.all.push(this) }
 close() { this.onclose?.({code:1006}) }
 send() {}
}
afterEach(()=>{vi.useRealTimers();vi.unstubAllGlobals();Socket.all=[]});
it("allows one immediate replacement, then backs off while replacement is pending",()=>{
 vi.useFakeTimers();
 vi.stubGlobal("WebSocket",Socket);
 vi.stubGlobal("location",{protocol:"http:",host:"localhost",search:""});
 vi.stubGlobal("document",{createElement:()=>({})});
 const video={videoWidth:0} as HTMLVideoElement;
 const s=new Session("view","id","credential",video,()=>{});
 // Deliberately replace a live connection; failed rejoins retain the flag.
 s["replace"]();
 expect(Socket.all).toHaveLength(2);
 Socket.all.at(-1)!.close();
 expect(Socket.all).toHaveLength(2);
 // Simulate failures over 12 seconds without running unrelated media ticks.
 clearInterval(s["timer"]);
 for(let i=0;i<120;i++) {
  const before=Socket.all.length;
  vi.advanceTimersByTime(100);
  if(Socket.all.length>before) Socket.all.at(-1)!.close();
 }
 expect(Socket.all.length).toBeLessThanOrEqual(10);
 expect(Socket.all.length).toBeGreaterThan(3);
});

it("refuses VP8-only capabilities before negotiation", () => {
  vi.stubGlobal("RTCRtpReceiver", { getCapabilities: () => ({ codecs: [{ mimeType: "video/VP8" }] }) });
  const session = Object.create(Session.prototype) as Session;
  session.role = "view";
  const setCodecPreferences = vi.fn();
  expect(() => session["preferVP9"]({ setCodecPreferences } as unknown as RTCRtpTransceiver)).toThrow("unsupported_codec: PassStream requires VP9");
  expect(setCodecPreferences).not.toHaveBeenCalled();
});

it("ends with a clear codec error instead of retrying negotiation", () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", Socket);
  vi.stubGlobal("location", { protocol: "http:", host: "localhost", search: "" });
  vi.stubGlobal("document", { createElement: () => ({}) });
  const terminal = vi.fn();
  const log = vi.spyOn(console, "error").mockImplementation(() => {});
  const s = new Session("view", "id", "credential", { videoWidth: 0 } as HTMLVideoElement, () => {}, undefined, { onTerminal: terminal });
  s["signalingError"](new E2EEError("unsupported_codec", "PassStream requires VP9 profile 0"));
  expect(terminal).toHaveBeenCalledWith("unsupported_codec");
  expect(s.state).toBe("ended");
  expect(Socket.all).toHaveLength(1);
  log.mockRestore();
});

it("a 1008 close terminates at once and nothing queued revives it", () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", Socket);
  vi.stubGlobal("location", { protocol: "http:", host: "localhost", search: "" });
  vi.stubGlobal("document", { createElement: () => ({}) });
  const terminal = vi.fn();
  const s = new Session("view", "id", "credential", { videoWidth: 0 } as HTMLVideoElement, () => {}, undefined, { onTerminal: terminal });
  s["stateTo"]("live");
  // The relay ends the stream: close 1008 "stream_ended".
  Socket.all[0].onclose?.({ code: 1008 });
  // Synchronous: no wait for a poll.
  expect(terminal).toHaveBeenCalledWith("stream_ended");
  expect(s.state).toBe("ended");
  // A video-frame callback queued before the close lands now.
  s["stateTo"]("live");
  s["stateTo"]("reconnecting");
  expect(s.state).toBe("ended");
  vi.advanceTimersByTime(10_000);
  expect(Socket.all).toHaveLength(1);
  expect(terminal).toHaveBeenCalledTimes(1);
});

describe("a poll carries only what it measured", () => {
  const spike = { rtt: 12, path: "UDP", loss: 14, bitrate: 70_000, fps: 13 };

  it("drops loss, rate and frame rate when the video report is missing", () => {
    expect(unmeasuredCleared(spike, { pair: true, video: false })).toEqual({ rtt: 12, path: "UDP", loss: 0, bitrate: 0, fps: 0 });
  });

  it("reports no path when no pair is selected, so an old round trip isn't shown as current", () => {
    const next = unmeasuredCleared(spike, { pair: false, video: true });
    expect(next).toEqual({ rtt: 12, path: "—", loss: 14, bitrate: 70_000, fps: 13 });
    const hud = viewerHud({ sample: { ...next, timestamp: 2000 }, layer: { rendition: null, temporal: null, temporalMax: null }, freezes: 0, g2g: null, e2ee: { epoch: null, failures: 0, top: null } });
    expect(hud.rtt_ms).toBeNull();
  });

  it("keeps a complete measurement as it is", () => {
    expect(unmeasuredCleared(spike, { pair: true, video: true })).toEqual(spike);
  });

  it("so one weak sample followed by polls without its report doesn't read as weak", () => {
    // What the viewer shows for the cleared sample: no loss, and no frame-rate verdict.
    const next = unmeasuredCleared(spike, { pair: true, video: false });
    const hud = viewerHud({ sample: { ...next, timestamp: 2000 }, layer: { rendition: null, temporal: null, temporalMax: null }, freezes: 0, g2g: null, e2ee: { epoch: null, failures: 0, top: null } });
    expect(hud.loss_pct).toBe(0);
    expect(hud.fps).toBeNull();
    expect(isDegraded({ ...hud, layer: null })).toBe(false);
  });
});
