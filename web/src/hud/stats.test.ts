import { describe, expect, it } from "vitest";
import { e2eeHud, FreezeCounter, HudSampler, kbps, latestG2G, parseLayer, pathLabel, senderHud, SessionLayerSource, viewerHud } from "./stats";

describe("HUD stats mapping", () => {
  it("parses the heartbeat layer into rendition and temporal layer", () => {
    expect(parseLayer("L1T3")).toEqual({ rendition: "native", temporal: 3, temporalMax: 3 });
    expect(parseLayer("L1T1")).toEqual({ rendition: "native", temporal: 1, temporalMax: 3 });
    expect(parseLayer("180p/L1T2")).toEqual({ rendition: "low", temporal: 2, temporalMax: 3 });
    for (const s of ["L1T3 requested", "", null, undefined, "garbage", "L1T0", "L1T99"]) expect(parseLayer(s)).toEqual({ rendition: null, temporal: null, temporalMax: null });
    expect(new SessionLayerSource(() => "180p/L1T1").layer().rendition).toBe("low");
  });

  it("labels the selected candidate pair's path", () => {
    expect(pathLabel("UDP")).toBe("UDP");
    expect(pathLabel("TCP")).toBe("TCP");
    expect(pathLabel("TURN/UDP")).toBe("TURN/UDP");
    expect(pathLabel("TURN/tls")).toBe("TURN/TLS");
    expect(pathLabel("TURN/TCP")).toBe("TURN/TCP");
    expect(pathLabel("—")).toBeNull();
    expect(pathLabel("TURN/")).toBeNull();
    expect(pathLabel(undefined)).toBeNull();
  });

  it("counts gaps over 500 ms incrementally, across a timeline that drops its head", () => {
    const f = new FreezeCounter();
    const tl = [0, 66, 133, 200].map((t) => ({ at: 1000 + t }));
    f.observe(tl);
    expect(f.renderGaps).toBe(0);
    tl.push({ at: 1200 + 501 }); // 501 ms gap
    tl.push({ at: 1200 + 501 + 500 }); // exactly 500 ms: not one
    f.observe(tl);
    expect(f.renderGaps).toBe(1);
    f.observe(tl); // nothing new: no double count
    expect(f.renderGaps).toBe(1);
    tl.splice(0, 4); // the session shifts old entries out
    tl.push({ at: 4000 });
    f.observe(tl);
    expect(f.renderGaps).toBe(2);
  });

  const act = (at: number, ingress: number, packets = 0, o: { paused?: boolean; hidden?: boolean } = {}) => ({ at, ingress, packets, paused: !!o.paused, hidden: !!o.hidden });

  it("a static source (unchanged clock, RTP, no ingress) is a render gap, not a freeze (review D2 #6)", () => {
    const f = new FreezeCounter();
    for (let t = 1000; t <= 2500; t += 500) f.sample(act(t, 40, 900));
    const timeline: { at: number; frame: number; rtpTimestamp: number }[] = [{ at: 1000, frame: 1, rtpTimestamp: 9000 }, { at: 2500, frame: 1, rtpTimestamp: 9000 }];
    f.observe(timeline);
    expect(f.freezes).toBe(0);
    expect(f.renderGaps).toBe(1);
    // The reviewer's exact case, with no activity samples at all: still not a freeze.
    const g = new FreezeCounter();
    g.observe([{ at: 1000 }, { at: 2500 }]);
    expect(g.freezes).toBe(0);
    expect(g.renderGaps).toBe(1);
  });

  it("a gap during which the SFU kept receiving frames, or packets kept arriving, is a freeze", () => {
    const f = new FreezeCounter();
    f.sample(act(1000, 40, 900));
    f.sample(act(1500, 47, 900)); // ingress advanced inside the gap
    f.sample(act(2000, 55, 900));
    f.observe([{ at: 1000 }, { at: 2200 }]);
    expect(f.freezes).toBe(1);
    const g = new FreezeCounter();
    g.sample(act(1100, 40, 900));
    g.sample(act(1600, 40, 960)); // packets arrived but nothing rendered
    g.observe([{ at: 1000 }, { at: 1800 }]);
    expect(g.freezes).toBe(1);
    // Ingress that only changes at the gap's edge (one sample inside) proves nothing.
    const h = new FreezeCounter();
    h.sample(act(900, 30, 900));
    h.sample(act(1500, 40, 900));
    h.observe([{ at: 1000 }, { at: 1800 }]);
    expect(h.freezes).toBe(0);
    expect(h.renderGaps).toBe(1);
  });

  it("gaps while paused (the curtain) or hidden count as neither", () => {
    const f = new FreezeCounter();
    f.sample(act(1000, 40, 900, { paused: true }));
    f.sample(act(1500, 50, 990, { paused: true }));
    f.observe([{ at: 1000 }, { at: 2000 }]);
    f.sample(act(2600, 60, 1100, { hidden: true }));
    f.sample(act(3100, 70, 1200, { hidden: true }));
    f.observe([{ at: 1000 }, { at: 2000 }, { at: 3300 }]);
    expect(f.freezes).toBe(0);
    expect(f.renderGaps).toBe(0);
  });

  it("G2G only from a recent frame that carried the clock row", () => {
    expect(latestG2G([{ at: 1000, delay: 87.4 }], 1500)).toBe(87);
    expect(latestG2G([{ at: 1000, delay: 87 }], 4000)).toBeNull();
    expect(latestG2G([{ at: 1000, delay: null }], 1100)).toBeNull();
    expect(latestG2G([], 1100)).toBeNull();
    expect(latestG2G([{ at: 1000, delay: -5 }], 1100)).toBeNull();
  });

  it("E2EE state: epoch, total drops and the top reason", () => {
    expect(e2eeHud(12, { unknown_key: 3, decrypt_failed: 7, replay: 0 })).toEqual({ epoch: 12, failures: 10, top: "decrypt_failed" });
    expect(e2eeHud(null, {})).toEqual({ epoch: null, failures: 0, top: null });
  });

  it("maps a session sample; missing or zero values are unknown, not zero", () => {
    const layer = parseLayer("L1T2");
    const e2ee = e2eeHud(4, {});
    const hud = viewerHud({ sample: { rtt: 41.6, loss: 0.5, bitrate: 1_234_567, path: "TURN/TLS", fps: 14.6, timestamp: 1 }, layer, freezes: 2, g2g: 120, e2ee });
    expect(hud).toEqual({ rtt_ms: 42, loss_pct: 0.5, bitrate_kbps: 1235, fps: 15, path: "TURN/TLS", rendition: "native", temporal: "T2 of 3", freezes: 2, render_gaps: 2, g2g_ms: 120, e2ee });
    const empty = viewerHud({ sample: { rtt: 0, loss: 0, bitrate: 0, path: "—", fps: 0, timestamp: 0 }, layer: parseLayer(null), freezes: 0, g2g: null, e2ee });
    expect(empty).toMatchObject({ rtt_ms: null, loss_pct: null, bitrate_kbps: null, fps: null, path: null, rendition: null, temporal: null });
    expect(viewerHud({ sample: { rtt: Number.NaN, loss: 400, bitrate: -1, path: "UDP", fps: 3, timestamp: 1 }, layer, freezes: 0, g2g: null, e2ee })).toMatchObject({ rtt_ms: null, loss_pct: null, bitrate_kbps: null, fps: 3, path: "UDP" });
    // Loopback: a selected pair with a sub-millisecond RTT is 0, not unknown; without a pair it is unknown.
    expect(viewerHud({ sample: { rtt: 0, loss: 0, bitrate: 1000, path: "UDP", fps: 15, timestamp: 1 }, layer, freezes: 0, g2g: null, e2ee }).rtt_ms).toBe(0);
    expect(viewerHud({ sample: { rtt: 12, loss: 0, bitrate: 1000, path: "—", fps: 15, timestamp: 1 }, layer, freezes: 0, g2g: null, e2ee }).rtt_ms).toBeNull();
  });

  it("maps sender health and audio", () => {
    const h = { uplink_kbps: 900, encode_ms: 3.2, encode_cpu_pct: 4.6, fps: 14.8, width: 1920, height: 1080, quality_limitation: "bandwidth" };
    expect(senderHud(h, { present: true, muted: false, kbps: 32 })).toEqual({ uplink_kbps: 900, encode_ms: 3.2, encode_cpu_pct: 5, quality_limitation: "bandwidth", fps: 15, resolution: "1920×1080", audio: "on · 32 kb/s" });
    expect(senderHud(h, { present: true, muted: true, kbps: 9 }).audio).toBe("muted (curtain)");
    expect(senderHud(null, { present: false, muted: false, kbps: null })).toEqual({ uplink_kbps: null, encode_ms: null, encode_cpu_pct: null, quality_limitation: "none", fps: null, resolution: null, audio: "none" });
    expect(kbps({ bytes: 0, at: 0 }, { bytes: 4000, at: 1000 })).toBe(32);
    expect(kbps(null, { bytes: 4000, at: 1000 })).toBeNull();
    expect(kbps({ bytes: 9000, at: 0 }, { bytes: 4000, at: 1000 })).toBeNull();
  });

  it("samples at most every 500 ms (2 Hz)", () => {
    let t = 0;
    let computed = 0;
    const s = new HudSampler(() => ++computed, 500, () => t);
    for (t = 0; t < 2000; t += 16) s.get(); // a 60 fps render loop for 2 s
    expect(computed).toBe(4);
    t = 2100; // ≥ 500 ms after the last compute (1536)
    expect(s.get()).toBe(5);
  });
});
