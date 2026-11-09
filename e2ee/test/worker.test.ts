import { describe, expect, it } from "vitest";
import { randomBytes, toHex } from "../src/bytes.ts";
import type { EncodedFrameLike } from "../src/frame-cryptor.ts";
import { addEncryptedTrack, assertE2EESupported, attachTransform, detectE2EESupport } from "../src/support.ts";
import { installTransformWorker, type ScriptTransformerLike, type WorkerOutMessage, type WorkerScopeLike } from "../src/worker-runtime.ts";
import { FakeFrame } from "./media.ts";

class FakeScope implements WorkerScopeLike {
  onrtctransform: WorkerScopeLike["onrtctransform"] = null;
  onmessage: WorkerScopeLike["onmessage"] = null;
  out: WorkerOutMessage[] = [];
  postMessage(m: unknown) {
    this.out.push(m as WorkerOutMessage);
  }
  send(data: unknown) {
    this.onmessage!({ data });
  }
}

/** A transformer whose input we feed and whose output we collect. */
function transformer(options: unknown) {
  const out: EncodedFrameLike[] = [];
  let push!: (f: EncodedFrameLike) => void;
  let close!: () => void;
  const readable = new ReadableStream<EncodedFrameLike>({
    start(c) {
      push = (f) => c.enqueue(f);
      close = () => c.close();
    },
  });
  const writable = new WritableStream<EncodedFrameLike>({ write: (f) => void out.push(f) });
  const calls = { keyframe: 0, request: 0 };
  const t: ScriptTransformerLike = {
    readable,
    writable,
    options,
    generateKeyFrame: async () => void calls.keyframe++,
    sendKeyFrameRequest: async () => void calls.request++,
  };
  return { t, out, push: (f: EncodedFrameLike) => push(f), close: () => close(), calls };
}

describe("transform worker glue", () => {
  it("encrypts on the sender, decrypts on the receiver, keys via postMessage", async () => {
    const sScope = new FakeScope();
    const s = installTransformWorker(sScope);
    const rScope = new FakeScope();
    const r = installTransformWorker(rScope);
    const tx = transformer({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    sScope.onrtctransform!({ transformer: tx.t });
    const rx = transformer({ role: "decrypt", kind: "video", track: 0, codec: "video/VP9" });
    rScope.onrtctransform!({ transformer: rx.t });

    const secret = randomBytes(32);
    sScope.send({ type: "sender-epoch", epoch: 4, secret: secret.slice().buffer });
    rScope.send({ type: "receiver-epoch", epoch: 4, secret: secret.slice() });

    const plain = Array.from({ length: 5 }, () => randomBytes(500));
    for (const p of plain) tx.push(new FakeFrame(p));
    tx.close();
    await s.piped[0];
    expect(tx.out).toHaveLength(5);
    for (const f of tx.out) rx.push(f);
    rx.close();
    await r.piped[0];
    expect(rx.out.map((f) => toHex(new Uint8Array(f.data)))).toEqual(plain.map(toHex));
    expect(tx.calls.keyframe).toBe(1); // epoch switch asked for a keyframe
  });

  it("fails closed: no key, bad options or unknown codec → frames dropped, never passed through", async () => {
    const scope = new FakeScope();
    const w = installTransformWorker(scope, { dropReportMs: 0 });
    const noKey = transformer({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: noKey.t });
    const bad = transformer({ role: "encrypt", kind: "video", track: 99 });
    scope.onrtctransform!({ transformer: bad.t });
    for (const x of [noKey, bad]) {
      x.push(new FakeFrame(randomBytes(100)));
      x.close();
    }
    await Promise.all(w.piped);
    expect(noKey.out).toHaveLength(0);
    expect(bad.out).toHaveLength(0);
    expect(scope.out.some((m) => m.type === "drop" && m.reason === "unknown_key")).toBe(true);
    expect(scope.out.some((m) => m.type === "error")).toBe(true);
  });

  it.each([0, 2])("rejects a second encryptor for track %i (would reuse nonces)", async (track) => {
    const scope = new FakeScope();
    const w = installTransformWorker(scope);
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32) });
    const a = transformer({ role: "encrypt", kind: "video", track, codec: "video/VP9" });
    const b = transformer({ role: "encrypt", kind: "video", track, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: a.t });
    scope.onrtctransform!({ transformer: b.t });
    b.push(new FakeFrame(randomBytes(10)));
    b.close();
    a.close();
    await Promise.all(w.piped);
    expect(b.out).toHaveLength(0);
    expect(scope.out).toContainEqual(expect.objectContaining({ type: "error" }));
  });

  it("asks the sender for a keyframe once a missing epoch's key arrives", async () => {
    const scope = new FakeScope();
    let t = 0;
    const w = installTransformWorker(scope, { now: () => t });
    const sender = installTransformWorker(new FakeScope());
    const k1 = { epoch: 1, secret: randomBytes(32) };
    const k2 = { epoch: 2, secret: randomBytes(32) };
    scope.send({ type: "receiver-epoch", ...k1 });
    const rx = transformer({ role: "decrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: rx.t });
    sender.senderRing.setEpoch(k1);
    sender.senderRing.setEpoch(k2);
    const key = await sender.senderRing.key(0);
    const f = new FakeFrame(await key.encrypt(randomBytes(40)), "video/VP9");
    rx.push(f);
    await new Promise((r) => setTimeout(r, 10));
    expect(rx.out).toHaveLength(0);
    t = 5_000;
    scope.send({ type: "receiver-epoch", ...k2 });
    expect(rx.calls.request).toBe(1);
    rx.close();
    await Promise.all(w.piped);
  });

  it("rejects malformed main-thread messages", () => {
    const scope = new FakeScope();
    installTransformWorker(scope);
    scope.send({ type: "sender-epoch", epoch: 1, secret: new Uint8Array(5) });
    scope.send({ type: "nope" });
    scope.send(null);
    expect(scope.out.filter((m) => m.type === "error")).toHaveLength(3);
  });
});

describe("feature detection (fail closed)", () => {
  const good = { isSecureContext: true, crypto: { subtle: {} }, Worker: function W() {}, RTCRtpScriptTransform: function T() {} };

  it("reports what is missing", () => {
    expect(detectE2EESupport(good)).toEqual({ supported: true });
    expect(detectE2EESupport({ ...good, RTCRtpScriptTransform: undefined })).toEqual({ supported: false, reason: "no_script_transform" });
    expect(detectE2EESupport({ ...good, isSecureContext: false })).toEqual({ supported: false, reason: "insecure_context" });
    expect(detectE2EESupport({ ...good, crypto: {} })).toEqual({ supported: false, reason: "no_webcrypto" });
    expect(() => assertE2EESupported({ ...good, RTCRtpScriptTransform: undefined })).toThrow(expect.objectContaining({ code: "not_supported" }));
  });

  it("throws if the browser does not keep the transform", () => {
    const ignoring = {
      get transform() {
        return null;
      },
      set transform(_v: unknown) {},
    };
    expect(() => attachTransform(ignoring, {} as Worker, { role: "encrypt", kind: "video", track: 0 }, good)).toThrow(
      expect.objectContaining({ code: "not_supported" }),
    );
    const keeping: { transform: unknown } = { transform: null };
    attachTransform(keeping, {} as Worker, { role: "decrypt", kind: "video", track: 0 }, good);
    expect(keeping.transform).toBeInstanceOf(good.RTCRtpScriptTransform);
  });

  it("addEncryptedTrack refuses without RTCRtpScriptTransform (Node has none) before touching the connection", async () => {
    let touched = false;
    const pc = { addTransceiver: () => ((touched = true), {}) } as unknown as RTCPeerConnection;
    await expect(addEncryptedTrack(pc, { kind: "video" } as MediaStreamTrack, {} as Worker, { track: 0 })).rejects.toMatchObject({ code: "not_supported" });
    expect(touched).toBe(false);
  });
});


it("keeps native/thumbnail counters and replay windows separate, and rekeys both encoders", async () => {
  const scope = new FakeScope(), sender = installTransformWorker(scope);
  const rs = new FakeScope(), receiver = installTransformWorker(rs, { okReportMs: 0, dropReportMs: 0 });
  const native = transformer({ role: "encrypt", kind: "video", track: 0, codec: "video/vp9" });
  const low = transformer({ role: "encrypt", kind: "video", track: 2, codec: "video/vp9" });
  const rx = transformer({ role: "decrypt", kind: "video", track: 0, tracks: [0, 2], codec: "video/vp9" });
  scope.onrtctransform!({ transformer: native.t }); scope.onrtctransform!({ transformer: low.t });
  rs.onrtctransform!({ transformer: rx.t });
  const secret = randomBytes(32);
  scope.send({ type: "sender-epoch", epoch: 4, secret });
  rs.send({ type: "receiver-epoch", epoch: 4, secret });
  native.push(new FakeFrame(Uint8Array.of(7, 8, 9))); native.close();
  low.push(new FakeFrame(Uint8Array.of(1, 2, 3))); low.close();
  await Promise.all(sender.piped);
  const { decodeHeader } = await import("../src/sframe.ts");
  const a = decodeHeader(new Uint8Array(native.out[0]!.data)), b = decodeHeader(new Uint8Array(low.out[0]!.data));
  expect(a).toMatchObject({ kid: 64n, ctr: 0n }); expect(b).toMatchObject({ kid: 66n, ctr: 0n });
  // Same counter on two KIDs succeeds; a replay on either KID is refused.
  for (const f of [native.out[0]!, low.out[0]!, native.out[0]!, low.out[0]!]) rx.push(new FakeFrame(new Uint8Array(f.data).slice()));
  const audioKey = await sender.senderRing.key(1);
  rx.push(new FakeFrame(await audioKey.encrypt(randomBytes(40))));
  rx.close(); await Promise.all(receiver.piped);
  expect(rx.out).toHaveLength(2);
  expect(rs.out).toContainEqual(expect.objectContaining({ type: "drop", reason: "wrong_track" }));
  expect(rs.out.filter(m => m.type === "drop" && m.reason === "replay")).toHaveLength(2);
  expect(rs.out).toContainEqual(expect.objectContaining({ type: "decrypted", track: 2 }));
  // Both live transforms get the keyframe hook on every epoch switch.
  const scope2 = new FakeScope(), w2 = installTransformWorker(scope2);
  const tx = [0, 2].map(track => transformer({ role: "encrypt", kind: "video", track, codec: "video/vp9" }));
  for (const t of tx) scope2.onrtctransform!({ transformer: t.t });
  scope2.send({ type: "sender-epoch", epoch: 4, secret });
  scope2.send({ type: "sender-epoch", epoch: 5, secret: randomBytes(32) });
  expect(tx.map(t => t.calls.keyframe)).toEqual([2, 2]);
  tx.forEach(t => t.close()); await Promise.all(w2.piped);
});

it.each([[[0, 0]], [[0, 1.5]], [[]], [[2]], [[0, 16]]])("refuses an invalid receive track allowlist %j", (tracks) => {
  const scope = new FakeScope(); installTransformWorker(scope);
  const rx = transformer({ role: "decrypt", kind: "video", track: 0, tracks, codec: "video/vp9" });
  scope.onrtctransform!({ transformer: rx.t }); rx.close();
  expect(scope.out).toContainEqual(expect.objectContaining({ type: "error" }));
});
