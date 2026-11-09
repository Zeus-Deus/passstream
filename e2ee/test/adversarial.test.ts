/**
 * The relay (SFU data channel) is the adversary: it sees, drops, tampers
 * with, replays and reorders every handshake message, and can inject its own.
 */
import { hmac } from "@noble/hashes/hmac.js";
import { sha512 } from "@noble/hashes/sha2.js";
import { describe, expect, it } from "vitest";
import {
  _internal,
  challengeSigInput,
  channelIdentifier,
  encodeAda,
  pinPrs,
  rejectSigInput,
  ViewerAdmission,
} from "../src/admission.ts";
import { type Bytes, concat, ctEqual, lvCat, randomBytes, utf8 } from "../src/bytes.ts";
import { open } from "../src/channel.ts";
import { calculateGenerator, deriveIsk, deriveMacKey, IDENTITY, sampleScalar, scalarMult, transcriptIr } from "../src/cpace.ts";
import { E2EEError } from "../src/errors.ts";
import { SenderIdentity, verifySignature } from "../src/identity.ts";
import { AdmissionMode, decodeMessage, encodeMessage, type Hello, MsgType, peekType } from "../src/wire.ts";
import { expectReject, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";

function flip(b: Uint8Array, i: number, mask = 0x01): Bytes {
  const c = Uint8Array.from(b);
  c[i] = (c[i] ?? 0) ^ mask;
  return c;
}

/** Record an honest handshake's four messages. */
async function transcript(h: Awaited<ReturnType<typeof makeHost>>, peer: string, v: ViewerAdmission) {
  const hello = v.start();
  await h.host.receive(peer, hello);
  const [challenge] = h.take(peer);
  const [response] = await v.handle(challenge!);
  await h.host.receive(peer, response!);
  const [admit] = h.take(peer);
  await v.handle(admit!);
  return { hello, challenge: challenge!, response: response!, admit: admit! };
}

describe("relay MITM: tampering", () => {
  it("every single-bit flip in any handshake message breaks the handshake (typed errors, no shared key)", async () => {
    // Each case gets a fresh host, so the guess budget can never run out and
    // turn later cases into no-ops, and each asserts that the targeted message
    // was really produced before it is mutated (review round 1, finding 8).
    const lens = await (async () => {
      const h = await makeHost();
      const t = await transcript(h, "ref", viewer(h));
      return { hello: t.hello.length, challenge: t.challenge.length, response: t.response.length, admit: t.admit.length };
    })();
    const reached = { hello: 0, challenge: 0, response: 0, admit: 0 };
    for (const which of ["hello", "challenge", "response", "admit"] as const) {
      for (let i = 0; i < lens[which]; i++) {
        const h = await makeHost();
        const peer = `t-${which}-${i}`;
        const v = viewer(h);
        const mask = 1 << i % 8;
        let viewerErr: unknown = null;
        try {
          let msg: Uint8Array = v.start();
          if (which === "hello") {
            reached.hello++;
            msg = flip(msg, i, mask);
          }
          await h.host.receive(peer, msg);
          let out = h.take(peer);
          if (which === "hello" && out.length === 0) continue; // sender refused the tampered HELLO
          expect(out).toHaveLength(1);
          let c = out[0]!;
          if (which === "challenge") {
            expect(peekType(c)).toBe(MsgType.Challenge);
            reached.challenge++;
            c = flip(c, i, mask);
          }
          const replies = await v.handle(c);
          let r = replies[0]!;
          if (which === "response") {
            expect(replies.map(peekType)).toEqual([MsgType.Response]);
            reached.response++;
            r = flip(r, i, mask);
          }
          await h.host.receive(peer, r);
          out = h.take(peer);
          if (which === "response") {
            // Malformed → nothing; otherwise a signed REJECT. Never an ADMIT.
            expect(out.map(peekType).filter((t) => t !== MsgType.Reject)).toEqual([]);
            continue;
          }
          expect(out).toHaveLength(1);
          let a = out[0]!;
          if (which === "admit") {
            expect(peekType(a)).toBe(MsgType.Admit);
            reached.admit++;
            a = flip(a, i, mask);
          }
          await v.handle(a);
        } catch (e) {
          viewerErr = e;
        }
        if (viewerErr !== null) expect(viewerErr, `${which}[${i}]`).toBeInstanceOf(E2EEError);
        // The viewer never ends up admitted, and only an untampered RESPONSE admits on the sender.
        expect(v.state, `${which}[${i}]`).not.toBe("admitted");
        if (which !== "admit") expect(h.peers(), `${which}[${i}]`).toEqual([]);
        expect(h.errors.every((e) => e instanceof E2EEError)).toBe(true);
      }
    }
    // Every byte of CHALLENGE, RESPONSE and ADMIT was actually reached and mutated.
    expect(reached).toEqual(lens);
  });

  it("cannot downgrade or rewrite the signed epoch / flags in ADa", async () => {
    const h = await makeHost();
    const v = viewer(h);
    await h.host.receive("p", v.start());
    const c = decodeMessage(h.take("p")[0]!);
    if (c.type !== MsgType.Challenge) throw new Error("no challenge");
    const forged = encodeMessage({ ...c, ada: encodeAda(0, false) });
    await expect(v.handle(forged)).rejects.toMatchObject({ code: "bad_signature" });
  });
});

describe("relay MITM: replay and reorder", () => {
  it("replaying an old CHALLENGE to a new HELLO fails the signature check", async () => {
    const h = await makeHost();
    const old = await transcript(h, "a", viewer(h));
    const v = viewer(h);
    v.start(); // HELLO never delivered; relay answers with the recorded challenge
    await expect(v.handle(old.challenge)).rejects.toMatchObject({ code: "bad_signature" });
  });

  it("replaying an old CHALLENGE to the same viewer session also fails (fresh nV)", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const old = await transcript(h, "a", viewer(h, { viewerSid: sid }));
    const v = viewer(h, { viewerSid: sid });
    v.start();
    await expect(v.handle(old.challenge)).rejects.toMatchObject({ code: "bad_signature" });
  });

  it("replaying an old RESPONSE to a new CHALLENGE is a failed guess, not an admission", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const old = await transcript(h, "a", viewer(h, { viewerSid: sid }));
    const before = (await h.host.budget.status()).streamFailures;
    // The relay replays the recorded HELLO (fresh challenge from the sender), then the old RESPONSE.
    await h.host.receive("b", old.hello);
    expect(peekType(h.take("b")[0]!)).toBe(MsgType.Challenge);
    await h.host.receive("b", old.response);
    const [rej] = h.take("b");
    expect(peekType(rej!)).toBe(MsgType.Reject);
    expect(h.peers()).toEqual(["a"]);
    expect((await h.host.budget.status()).streamFailures).toBe(before + 1);
  });

  it("replaying an old ADMIT into a new handshake fails key confirmation", async () => {
    const h = await makeHost();
    const old = await transcript(h, "a", viewer(h));
    const v = viewer(h);
    await h.host.receive("b", v.start());
    await v.handle(h.take("b")[0]!); // response produced but never delivered
    await expect(v.handle(old.admit)).rejects.toMatchObject({ code: "bad_mac" });
  });

  it("out-of-order messages are rejected with typed errors on both sides", async () => {
    const h = await makeHost();
    const ref = await transcript(h, "ref", viewer(h));
    // Sender: RESPONSE / ADMIT / CHALLENGE before HELLO.
    for (const m of [ref.response, ref.admit, ref.challenge]) await h.host.receive("x", m);
    expect(h.take("x")).toEqual([]);
    expect(h.errors.map((e) => e.code)).toEqual(["unexpected_message", "unexpected_message", "unexpected_message"]);
    // Viewer: ADMIT before CHALLENGE, CHALLENGE twice.
    const v = viewer(h);
    v.start();
    await expect(v.handle(ref.admit)).rejects.toMatchObject({ code: "unexpected_message" });
    const v2 = viewer(h);
    await h.host.receive("y", v2.start());
    const [c] = h.take("y");
    await v2.handle(c!);
    await expect(v2.handle(c!)).rejects.toMatchObject({ code: "unexpected_message" });
  });

  it("a REJECT forged or replayed by the relay is not believed", async () => {
    const h = await makeHost();
    // Capture a genuine signed REJECT (wrong PIN).
    const v1 = viewer(h, { pin: "0000" });
    await h.host.receive("a", v1.start());
    const [resp] = await v1.handle(h.take("a")[0]!);
    await h.host.receive("a", resp!);
    const [genuine] = h.take("a");
    expect(peekType(genuine!)).toBe(MsgType.Reject);
    // Replayed to another viewer (before or after its CHALLENGE): bound to v1's nonces.
    const v2 = viewer(h);
    await h.host.receive("b", v2.start());
    await expect(v2.handle(genuine!)).rejects.toMatchObject({ code: "bad_signature" });
    // Forged by the relay with its own key, claiming the stream is frozen.
    const v3 = viewer(h);
    v3.start();
    const relayIdentity = SenderIdentity.generate();
    const fake = encodeMessage({ type: MsgType.Reject, reason: 3, retryAfterMs: 0, triesLeft: 0, sig: relayIdentity.sign(utf8("x")) });
    await expect(v3.handle(fake)).rejects.toMatchObject({ code: "bad_signature" });
  });
});

describe("relay cannot run PIN guesses against viewers", () => {
  it("a CHALLENGE not signed by the pinned key gets no RESPONSE", async () => {
    const h = await makeHost();
    const relay = SenderIdentity.generate();
    const v = viewer(h);
    const hello = decodeMessage(v.start()) as Hello;
    // The relay plays sender with its own share for a guessed PIN and its own key.
    const ids = { streamId: hello.streamId, senderKey: h.identity.publicKey, viewerSid: hello.viewerSid, mode: AdmissionMode.Pin, grantId: new Uint8Array(0) };
    const nS = randomBytes(32);
    const g = calculateGenerator(pinPrs("1234", h.linkSecret), channelIdentifier(ids), concat(hello.nonce, nS));
    const ya = scalarMult(sampleScalar(), g);
    const c = { nonce: nS, ada: encodeAda(1, true), ya };
    for (const sig of [
      relay.sign(challengeSigInput(ids, hello.nonce, c)), // wrong key
      new Uint8Array(64), // garbage
      h.identity.sign(challengeSigInput({ ...ids, streamId: utf8("other") }, hello.nonce, c)), // real key, other stream
    ]) {
      const v2 = new ViewerAdmission({ streamId: STREAM, senderKey: h.identity.publicKey, viewerSid: hello.viewerSid, linkSecret: h.linkSecret, pin: PIN });
      v2.start();
      let out: Bytes[] | null = null;
      await expect(
        (async () => {
          out = await v2.handle(encodeMessage({ type: MsgType.Challenge, ...c, sig }));
        })(),
      ).rejects.toMatchObject({ code: "bad_signature" });
      expect(out).toBeNull();
      expect(v2.state).toBe("failed");
    }
  });

  it("a signature from the real sender for a different viewer nonce is useless", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const a = viewer(h, { viewerSid: sid });
    await h.host.receive("a", a.start());
    const challengeForA = h.take("a")[0]!;
    const b = viewer(h, { viewerSid: sid });
    b.start();
    await expect(b.handle(challengeForA)).rejects.toMatchObject({ code: "bad_signature" });
  });
});

describe("relay cannot learn the PIN or the key offline", () => {
  const ALL_PINS = Array.from({ length: 10_000 }, (_, i) => String(i).padStart(4, "0"));

  it("passive transcript: no PIN candidate can be confirmed and the key box stays sealed", async () => {
    const h = await makeHost();
    const v = viewer(h);
    const t = await transcript(h, "p", v);
    const hello = decodeMessage(t.hello) as Hello;
    const c = decodeMessage(t.challenge);
    const r = decodeMessage(t.response);
    const a = decodeMessage(t.admit);
    if (c.type !== MsgType.Challenge || r.type !== MsgType.Response || a.type !== MsgType.Admit) throw new Error("bad transcript");
    const ids = { streamId: hello.streamId, senderKey: h.identity.publicKey, viewerSid: hello.viewerSid, mode: AdmissionMode.Pin, grantId: new Uint8Array(0) };
    const sid = concat(hello.nonce, c.nonce);
    const ci = channelIdentifier(ids);
    const tr = transcriptIr(c.ya, c.ada, r.yb, r.adb);
    let confirmed = 0;
    for (const pin of ALL_PINS) {
      const prs = pinPrs(pin, h.linkSecret);
      const g = calculateGenerator(prs, ci, sid).toBytes();
      // Everything the relay can compute without a secret scalar: K candidates
      // from public points, and MAC keys straight from the password.
      const kCandidates = [IDENTITY, c.ya, r.yb, g];
      for (const k of kCandidates) {
        const isk = deriveIsk(sid, k, tr);
        const mk = deriveMacKey(sid, isk);
        if (ctEqual(hmac(sha512, mk, lvCat(r.yb, r.adb)), r.tb)) confirmed++;
        if (ctEqual(hmac(sha512, mk, lvCat(c.ya, c.ada)), a.ta)) confirmed++;
      }
      if (ctEqual(hmac(sha512, sha512(prs), lvCat(r.yb, r.adb)), r.tb)) confirmed++;
    }
    expect(confirmed).toBe(0);
    // The box key derives from the ISK, which needs K: the relay's best guess can't open it.
    const conf = await _internal.confirmKeys(sid, c.ya, c.ya, c.ada, r.yb, r.adb);
    await expect(open(conf.keys.box, new Uint8Array(12), utf8("PassStream v1 admit"), a.box)).rejects.toMatchObject({ code: "decrypt_failed" });
  });

  it("active MITM on the sender: one injected guess, budget charged, nothing keyed comes back", async () => {
    const h = await makeHost();
    const v = viewer(h);
    const hello = decodeMessage(v.start()) as Hello;
    await h.host.receive("p", encodeMessage(hello));
    const c = decodeMessage(h.take("p")[0]!);
    if (c.type !== MsgType.Challenge) throw new Error("no challenge");
    // Relay answers itself with a share for guess "1234".
    const ids = { streamId: hello.streamId, senderKey: h.identity.publicKey, viewerSid: hello.viewerSid, mode: AdmissionMode.Pin, grantId: new Uint8Array(0) };
    const sid = concat(hello.nonce, c.nonce);
    const g = calculateGenerator(pinPrs("1234", h.linkSecret), channelIdentifier(ids), sid);
    const yb = sampleScalar();
    const Yb = scalarMult(yb, g);
    const conf = await _internal.confirmKeys(sid, scalarMult(yb, (await import("@noble/curves/ed25519.js")).ristretto255.Point.fromBytes(c.ya)), c.ya, c.ada, Yb, Uint8Array.of(1));
    await h.host.receive("p", encodeMessage({ type: MsgType.Response, yb: Yb, adb: Uint8Array.of(1), tb: conf.tb }));
    const [reply] = h.take("p");
    const rej = decodeMessage(reply!);
    expect(rej.type).toBe(MsgType.Reject);
    if (rej.type !== MsgType.Reject) return;
    // The REJECT is a signature over public, PIN-independent values only: verify it from them.
    expect(verifySignature(h.identity.publicKey, rejectSigInput(ids, hello.nonce, c.nonce, rej), rej.sig)).toBe(true);
    expect((await h.host.budget.status()).streamFailures).toBe(1);
    // The original viewer, whose RESPONSE the relay dropped, is not admitted either.
    expect(h.peers()).toEqual([]);
  });

  it("a relay without the link secret cannot admit itself to a no-PIN stream", async () => {
    const h = await makeHost({ pin: null });
    // The relay knows everything except the fragment-only link secret.
    await expectReject(join(h, "relay", viewer(h, { pin: null, linkSecret: new Uint8Array(32) })), "wrong_pin");
    expect(h.peers()).toEqual([]);
  });
});
