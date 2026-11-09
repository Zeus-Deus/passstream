import { describe, expect, it } from "vitest";
import { CONNECT_SLOW_MS, OutageClock, RECONNECT_GRACE_MS, RECONNECT_LONG_MS, TerminalLatch, screenFor, trouble, type Screen, type Snapshot, type View } from "./machine";

const T0 = 1_000_000;
type Sess = NonNullable<Snapshot["session"]>;
const LIVE: Sess = { state: "live", marker: null, firstFrame: 420, hasStream: true, signalingOpen: true };

function snap(view: View, o: Partial<Omit<Snapshot, "session">> & { session?: Partial<Sess> | null } = {}): Snapshot {
  const { session, ...rest } = o;
  return {
    view,
    checkingPin: false,
    pinPolicy: { kind: "digits", length: 4 },
    lockedUntil: null,
    hostAway: false,
    readmitting: false,
    startedAt: T0,
    now: T0 + 1000,
    session: session === null ? null : { ...LIVE, ...session },
    ...rest,
  };
}
const media: View = { kind: "media" };

/** Drive the machine over time the way the viewer does (one clock, many renders). */
function drive(steps: [number, Snapshot][]): Screen[] {
  const clock = new OutageClock();
  return steps.map(([at, s]) => {
    const x = { ...s, now: T0 + at };
    return screenFor(x, clock.update(trouble(x), x.now));
  });
}
const kinds = (xs: Screen[]) => xs.map((x) => x.kind);

describe("before any network", () => {
  it("unsupported fails closed with the missing features", () => {
    expect(screenFor(snap({ kind: "unsupported", missing: ["RTCRtpScriptTransform (encrypted media)"] }, { session: null }), null)).toEqual({
      kind: "unsupported",
      missing: ["RTCRtpScriptTransform (encrypted media)"],
    });
  });
  it("an incomplete link is invalid", () => {
    expect(screenFor(snap({ kind: "invalid" }, { session: null }), null).kind).toBe("invalid");
  });
});

describe("connecting → PIN → checking → admitted → live", () => {
  it("connecting while the join and the signed CHALLENGE are in flight", () => {
    expect(screenFor(snap({ kind: "verifying" }, { session: { firstFrame: 0, hasStream: false } }), null)).toEqual({ kind: "connecting", slow: false });
  });
  it("says it's slow after CONNECT_SLOW_MS, still not an error", () => {
    expect(screenFor(snap({ kind: "verifying" }, { now: T0 + CONNECT_SLOW_MS + 1 }), null)).toEqual({ kind: "connecting", slow: true });
  });
  it("the PIN screen, with the sender's policy", () => {
    const s = screenFor(snap({ kind: "pin", policy: { kind: "digits", length: 6 } }), null);
    expect(s).toEqual({ kind: "pin", policy: { kind: "digits", length: 6 }, checking: false, lockout: false });
  });
  it("a typed PIN stays on the PIN screen while the host checks it", () => {
    const s = screenFor(snap({ kind: "verifying" }, { checkingPin: true, pinPolicy: { kind: "passphrase" } }), null);
    expect(s).toEqual({ kind: "pin", policy: { kind: "passphrase" }, checking: true, lockout: false });
  });
  it("admitted: keys in, no first frame yet", () => {
    expect(screenFor(snap(media, { session: { firstFrame: 0 } }), null)).toEqual({ kind: "admitted", slow: false });
    expect(screenFor(snap(media, { session: { hasStream: false, firstFrame: 0 } }), null).kind).toBe("admitted");
    expect(screenFor(snap(media, { session: null }), null).kind).toBe("admitted");
  });
  it("live on the first decoded frame", () => {
    expect(screenFor(snap(media), null)).toEqual({ kind: "live" });
  });
  it("the whole happy path in order", () => {
    const pin: View = { kind: "pin", policy: { kind: "digits", length: 4 } };
    expect(
      kinds(
        drive([
          [0, snap({ kind: "verifying" }, { session: { firstFrame: 0, hasStream: false } })],
          [200, snap(pin)],
          [3000, snap({ kind: "verifying" }, { checkingPin: true })],
          [3400, snap(media, { session: { firstFrame: 0 } })],
          [3900, snap(media)],
        ]),
      ),
    ).toEqual(["connecting", "pin", "pin", "admitted", "live"]);
  });
});

describe("wrong PIN and lockout", () => {
  const pin: View = { kind: "pin", policy: { kind: "digits", length: 4 } };
  it("is the PIN screen in lockout until the backoff ends, then the PIN screen again", () => {
    const until = T0 + 60_000;
    expect(screenFor(snap(pin, { lockedUntil: until, now: T0 + 1000 }), null)).toMatchObject({ kind: "pin", lockout: true });
    expect(screenFor(snap(pin, { lockedUntil: until, now: until - 1 }), null)).toMatchObject({ kind: "pin", lockout: true });
    expect(screenFor(snap(pin, { lockedUntil: until, now: until }), null)).toMatchObject({ kind: "pin", lockout: false });
  });
  it("wrong PIN → PIN (no lockout); the fifth → lockout", () => {
    const xs = drive([
      [0, snap({ kind: "verifying" }, { checkingPin: true })],
      [300, snap(pin)],
      [900, snap({ kind: "verifying" }, { checkingPin: true })],
      [1200, snap(pin, { lockedUntil: T0 + 61_200 })],
    ]);
    expect(xs.map((x) => (x.kind === "pin" ? `pin:${x.checking ? "checking" : x.lockout ? "lockout" : "entry"}` : x.kind))).toEqual([
      "pin:checking",
      "pin:entry",
      "pin:checking",
      "pin:lockout",
    ]);
  });
  it("twenty wrong PINs on the stream: removed (frozen)", () => {
    expect(screenFor(snap({ kind: "removed", reason: "frozen" }), null)).toEqual({ kind: "removed", reason: "frozen" });
  });
});

describe("knock and waiting for the host", () => {
  it("knock pending shows the row code", () => {
    expect(screenFor(snap({ kind: "knock", code: "K7P" }), null)).toEqual({ kind: "knock", code: "K7P" });
  });
  it("knock → admitted → live on approval; knock → removed on denial", () => {
    expect(kinds(drive([[0, snap({ kind: "knock", code: "K7P" })], [1000, snap(media, { session: { firstFrame: 0 } })], [1400, snap(media)]]))).toEqual([
      "knock",
      "admitted",
      "live",
    ]);
    expect(kinds(drive([[0, snap({ kind: "knock", code: "K7P" })], [1000, snap({ kind: "removed", reason: "denied" })]]))).toEqual(["knock", "removed"]);
  });
  it("the host offline before admission is 'waiting', then the PIN check runs", () => {
    expect(kinds(drive([[0, snap({ kind: "waiting" })], [5000, snap({ kind: "verifying" }, { checkingPin: true })], [5300, snap(media)]]))).toEqual([
      "waiting",
      "pin",
      "live",
    ]);
  });
});

describe("paused by the host", () => {
  it("live ⇄ paused follows the sender's marker", () => {
    expect(kinds(drive([[0, snap(media)], [1000, snap(media, { session: { marker: "paused" } })], [9000, snap(media)]]))).toEqual(["live", "paused", "live"]);
  });
  it("paused wins over a reconnect: the cover stays up", () => {
    expect(kinds(drive([[0, snap(media, { session: { marker: "paused" } })], [5000, snap(media, { session: { marker: "paused", state: "reconnecting" } })]]))).toEqual([
      "paused",
      "paused",
    ]);
  });
});

describe("reconnect: never scary for a short outage", () => {
  const blip = snap(media, { session: { state: "reconnecting" } });
  it("a blip shorter than the grace stays live", () => {
    expect(kinds(drive([[0, snap(media)], [1000, blip], [1000 + RECONNECT_GRACE_MS - 1, blip], [1000 + RECONNECT_GRACE_MS + 200, snap(media)]]))).toEqual([
      "live",
      "live",
      "live",
      "live",
    ]);
  });
  it("after the grace: reconnecting (network); after LONG: still reconnecting; then live", () => {
    const xs = drive([
      [0, snap(media)],
      [1000, blip],
      [1000 + RECONNECT_GRACE_MS, blip],
      [1000 + RECONNECT_LONG_MS, blip],
      [1000 + RECONNECT_LONG_MS + 500, snap(media)],
    ]);
    expect(xs).toEqual([
      { kind: "live" },
      { kind: "live" },
      { kind: "reconnecting", cause: "network", long: false },
      { kind: "reconnecting", cause: "network", long: true },
      { kind: "live" },
    ]);
  });
  it("the grace restarts after a recovery", () => {
    expect(kinds(drive([[0, blip], [RECONNECT_GRACE_MS + 1, blip], [RECONNECT_GRACE_MS + 2, snap(media)], [RECONNECT_GRACE_MS + 3, blip]]))).toEqual([
      "live",
      "reconnecting",
      "live",
      "live",
    ]);
  });
  it.each<[string, Partial<Omit<Snapshot, "session">> & { session?: Partial<Sess> }, "network" | "host"]>([
    ["peer replacement", { session: { state: "reconnecting" } }, "network"],
    ["ICE restart after the first frame", { session: { state: "connecting" } }, "network"],
    ["signaling down (server restart)", { session: { signalingOpen: false } }, "network"],
    ["re-admitting with the grant", { readmitting: true }, "network"],
    ["the host's sender away (reload, network)", { hostAway: true }, "host"],
  ])("%s counts as trouble", (_, o, cause) => {
    const s = snap(media, o);
    expect(trouble(s)).toBe(cause);
    expect(drive([[0, s], [RECONNECT_GRACE_MS, s]])[1]).toEqual({ kind: "reconnecting", cause, long: false });
  });
  it("nothing before admission counts as trouble", () => {
    expect(trouble(snap({ kind: "verifying" }, { session: { state: "reconnecting" } }))).toBeNull();
    expect(trouble(snap(media, { session: { state: "connecting", firstFrame: 0 } }))).toBeNull();
  });
});

describe("terminal states", () => {
  it.each<[string, View]>([
    ["kicked", { kind: "removed", reason: "kicked" }],
    ["locked", { kind: "removed", reason: "locked" }],
    ["denied", { kind: "removed", reason: "denied" }],
    ["full", { kind: "removed", reason: "full" }],
    ["ended", { kind: "ended", reason: "stopped" }],
    ["expired", { kind: "ended", reason: "expired" }],
  ])("live → %s replaces the picture at once (no grace)", (_, v) => {
    const xs = drive([[0, snap(media)], [10, snap(v, { session: { state: "ended" } })]]);
    expect(xs[1]).toEqual(v);
  });
  it("the sender's Ended picture ends the stream before the terminal signal", () => {
    expect(screenFor(snap(media, { session: { marker: "ended" } }), null)).toEqual({ kind: "ended", reason: "stopped" });
  });
  it.each<View>([media, { kind: "pin", policy: { kind: "digits", length: 4 } }, { kind: "knock", code: "K7P" }, { kind: "waiting" }, { kind: "verifying" }])(
    "a closed session (server close 1008 on stream end) ends %j",
    (v) => {
      expect(screenFor(snap(v, { session: { state: "ended" } }), null)).toEqual({ kind: "ended", reason: "stopped" });
    },
  );
  it("a closed session doesn't turn a kick into an end", () => {
    expect(screenFor(snap({ kind: "removed", reason: "kicked" }, { session: { state: "ended" } }), null)).toEqual({ kind: "removed", reason: "kicked" });
  });
});

describe("terminal states are irreversible", () => {
  function render(latch: TerminalLatch, s: Snapshot) {
    return latch.hold(screenFor(s, null), s).kind;
  }
  it("signaling closed for good stays ended when a late frame says live", () => {
    const latch = new TerminalLatch();
    expect(render(latch, snap(media))).toBe("live");
    expect(render(latch, snap(media, { session: { state: "ended" } }))).toBe("ended");
    // A queued callback flips the session back, and the controller's view never changed.
    expect(render(latch, snap(media, { session: { state: "live" } }))).toBe("ended");
    expect(render(latch, snap({ kind: "pin", policy: { kind: "digits", length: 4 } }))).toBe("ended");
  });
  it("removed stays removed", () => {
    const latch = new TerminalLatch();
    expect(render(latch, snap({ kind: "removed", reason: "kicked" }))).toBe("removed");
    expect(render(latch, snap(media))).toBe("removed");
    expect(render(latch, snap({ kind: "ended", reason: "stopped" }))).toBe("removed");
  });
  it("the in-picture ended marker alone is a hint, not a latch", () => {
    const latch = new TerminalLatch();
    expect(render(latch, snap(media, { session: { marker: "ended" } }))).toBe("ended");
    expect(render(latch, snap(media))).toBe("live");
  });
});
