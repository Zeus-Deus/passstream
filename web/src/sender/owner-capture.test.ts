import { describe, expect, it } from "vitest";
import { base64, captureAllowed, pictureChanged } from "./owner-capture";

describe("owner picture change IDs", () => {
  it("emits the first picture and ignores identical pixels", () => {
    const p = new Uint8ClampedArray([1, 2, 3, 255]);
    expect(pictureChanged(undefined, p)).toBe(true);
    expect(pictureChanged(p, p.slice())).toBe(false);
  });
  it("detects a single pixel and native resolution changes", () => {
    expect(pictureChanged(new Uint8ClampedArray([1, 2, 3, 255]), new Uint8ClampedArray([1, 2, 4, 255]))).toBe(true);
    expect(pictureChanged(new Uint8ClampedArray(4), new Uint8ClampedArray(8))).toBe(true);
  });
  it("encodes large byte arrays without a spread stack overflow", () => {
    const b = new Uint8Array(200_000).fill(19);
    const encoded = base64(b);
    expect(atob(encoded).length).toBe(b.length); expect(atob(encoded).charCodeAt(199999)).toBe(19);
  });
});

describe("every video privacy gate closes the owner capture", () => {
  const g = { live: true, paused: false, suspended: false, attached: true };
  const t = { enabled: true, muted: false, readyState: "live" as MediaStreamTrackState };
  it("allows a safe live source", () => expect(captureAllowed(g, t)).toBe(true));
  it.each([ ["paused", true], ["suspended", true], ["attached", false], ["live", false] ] as const)("closes for %s=%s", (k, v) => expect(captureAllowed({ ...g, [k]: v }, t)).toBe(false));
  it.each([ ["enabled", false], ["muted", true], ["readyState", "ended"] ] as const)("closes for source %s=%s", (k, v) => expect(captureAllowed(g, { ...t, [k]: v })).toBe(false));
  it("closes without a source", () => expect(captureAllowed(g, undefined)).toBe(false));
});

it("lock only gates admission and does not stop owner capture",()=>{
 const gate={live:true,paused:false,suspended:false,attached:true,locked:true};
 expect(captureAllowed(gate,{enabled:true,muted:false,readyState:"live"})).toBe(true);
});
