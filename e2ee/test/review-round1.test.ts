/**
 * Regression tests for the findings of review round 1 (docs/PROTOCOL.md,
 * "Review round 1 fixes"). Each test replays the reported attack; against
 * the code before the fixes (6ce81dd) it fails.
 */
import { describe, expect, it } from "vitest";
import { SenderAdmission, type SenderAdmissionContext, ViewerAdmission } from "../src/admission.ts";
import { GuessBudget } from "../src/budget.ts";
import { type Bytes, randomBytes, toBase64Url, toHex } from "../src/bytes.ts";
import { type EpochKey, SenderSession } from "../src/channel.ts";
import { FrameDecryptor, FrameEncryptor } from "../src/frame-cryptor.ts";
import { GrantStore } from "../src/grants.ts";
import { buildFragment, generateLinkSecret, parseFragment, SenderIdentity } from "../src/identity.ts";
import { ReceiverKeyRing, SenderKeyRing } from "../src/keyring.ts";
import { decodeHeader } from "../src/sframe.ts";
import { MemoryStorage } from "../src/storage.ts";
import { MsgType, peekType } from "../src/wire.ts";
import { expectReject, type Host, join, makeHost, PIN, STREAM, viewer } from "./harness.ts";
import { FakeFrame } from "./media.ts";

const types = (msgs: Bytes[]) => msgs.map((m) => peekType(m));

/** Deliver queued REKEYs to an admitted viewer and ack them. Returns how many arrived. */
async function deliverRekeys(h: Host, peer: string, session: { handle(b: Uint8Array): Promise<{ epoch: EpochKey; ack: Bytes }> }) {
  const got: EpochKey[] = [];
  for (const m of h.take(peer)) {
    if (peekType(m) !== MsgType.Rekey) continue;
    const { epoch, ack } = await session.handle(m);
    got.push(epoch);
    await h.host.receive(peer, ack);
  }
  return got;
}

describe("finding 1: revocation is checked at admission, not at HELLO", () => {
  it("a grant handshake started before a kick cannot finish after it", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    const v = viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined });
    await h.host.receive("p2", v.start());
    const [challenge] = h.take("p2");
    expect(peekType(challenge!)).toBe(MsgType.Challenge); // the grant was valid at HELLO
    const [response] = await v.handle(challenge!);
    await h.host.kick(h.idOf("p1"));
    await h.host.receive("p2", response!);
    expect(types(h.take("p2"))).not.toContain(MsgType.Admit);
    expect(h.peers()).toEqual([]);
  });

  it("a grant revoked between CHALLENGE and RESPONSE is re-checked at commit", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    await h.host.peerClosed("p1");
    const v = viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined });
    await h.host.receive("p2", v.start());
    const [response] = await v.handle(h.take("p2")[0]!);
    // Storage-level revocation with no handshake abort: only the commit-time check can catch it.
    await h.host.grants.revoke(first.grant.id);
    await h.host.receive("p2", response!);
    const [reply] = h.take("p2");
    expect(peekType(reply!)).toBe(MsgType.Reject);
    await expectReject(v.handle(reply!), "grant_invalid");
    expect(h.peers()).toEqual([]);
  });

  it("an old-PIN admission racing rotatePin() is refused and gets no grant", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const v = viewer(h, { viewerSid: sid });
    await h.host.receive("p", v.start());
    const [response] = await v.handle(h.take("p")[0]!);
    // Rotate exactly between key confirmation (budget "success") and the commit.
    let rotation: Promise<void> | null = null;
    h.host.budget.on("success", () => {
      rotation = h.host.rotatePin("9999");
    });
    await h.host.receive("p", response!);
    await rotation;
    expect(rotation).not.toBeNull();
    const out = h.take("p");
    expect(types(out)).not.toContain(MsgType.Admit);
    expect(h.peers()).toEqual([]);
    const stored = JSON.parse(h.storage.dump()[`grants:${STREAM}`] ?? "{}");
    expect(Object.values(stored).filter((e) => (e as { sid: string }).sid === toBase64Url(sid))).toEqual([]);
  });
});

describe("finding 2: a kick removes every connection admitted with the grant", () => {
  it("cloned grant on other peers, and a pending grant handshake, are all cut off", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    const clone = () => viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined });
    const c2 = await join(h, "p2", clone());
    const c3 = await join(h, "p3", clone());
    const honest = await join(h, "q", viewer(h));
    const pending = clone();
    await h.host.receive("p4", pending.start());
    const [resp4] = await pending.handle(h.take("p4")[0]!);
    expect(h.peers()).toEqual(["p1", "p2", "p3", "q"]);

    const kicked: string[][] = [];
    h.host.on("kicked", (e) => kicked.push(e.peerIds));
    const ids = await h.host.kick(h.idOf("p2"));
    expect(ids).toHaveLength(3);
    expect(kicked).toEqual([["p1", "p2", "p3"]]);
    expect(h.peers()).toEqual(["q"]);
    // Only the honest viewer gets the new epoch; the clones get nothing.
    for (const p of ["p1", "p2", "p3"]) expect(h.take(p)).toEqual([]);
    const got = await deliverRekeys(h, "q", honest.session);
    expect(got).toHaveLength(1);
    expect(h.host.keys.activeEpoch).toBe(got[0]!.epoch);
    // The pending handshake was aborted with the grant.
    await h.host.receive("p4", resp4!);
    expect(types(h.take("p4"))).not.toContain(MsgType.Admit);
    expect(c2.grant.id).toEqual(c3.grant.id);
  });
});

describe("finding 3: forged HELLOs cannot detach an admission from revocation", () => {
  it("a bare or well-formed HELLO on an admitted peer leaves the admission kickable", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    const id = h.idOf("p1");
    await h.host.receive("p1", Uint8Array.of(1, MsgType.Hello)); // bare, unparsable
    await h.host.receive("p1", viewer(h).start()); // valid HELLO the relay made up
    expect(h.idOf("p1")).toBe(id);
    // Still receiving rekeys.
    await h.host.keys.rekey("manual");
    expect(types(h.take("p1"))).toContain(MsgType.Rekey);
    // The owner's kick finds the admission and revokes its grant.
    await h.host.kick(id);
    await expectReject(join(h, "p9", viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined })), "grant_invalid");
  });

  it("a closed transport keeps the admission record, so a later kick still revokes the grant", async () => {
    const h = await makeHost();
    const sid = randomBytes(16);
    const first = await join(h, "p1", viewer(h, { viewerSid: sid }));
    const id = h.idOf("p1");
    await h.host.peerClosed("p1"); // the relay can force this
    expect(h.host.viewers()).toEqual([expect.objectContaining({ admissionId: id, connected: false })]);
    await h.host.kick(id);
    await expectReject(join(h, "p9", viewer(h, { viewerSid: sid, grant: first.grant, pin: undefined })), "grant_invalid");
  });
});

describe("finding 4: replacing the encryptor never reuses an epoch (RFC 9605 §9.1)", () => {
  it("the active secret is not readable, and replaceEncryptor() gives the new ring a fresh epoch", async () => {
    const h = await makeHost();
    const a = await join(h, "a", viewer(h));
    for (const ack of a.acks) await h.host.receive("a", ack);
    expect("active" in h.host.keys).toBe(false);
    expect(Object.values(h.host.keys).some((v) => v instanceof Uint8Array)).toBe(false);

    const seen = new Set<string>();
    const pump = async (ring: SenderKeyRing, n: number) => {
      const enc = new FrameEncryptor(ring, { track: 0, kind: "video", codec: "video/VP9" });
      const frames: FakeFrame[] = [];
      for (let i = 0; i < n; i++) {
        const f = new FakeFrame(randomBytes(32), "video/VP9");
        expect(await enc.encrypt(f)).toBe(true);
        const hd = decodeHeader(f.bytes());
        const key = `${hd.kid}:${hd.ctr}`;
        expect(seen.has(key), `(KID, CTR) ${key} reused`).toBe(false);
        seen.add(key);
        frames.push(f);
      }
      return frames;
    };
    // Worker 1 gets its key the only way there is: onEpoch.
    const ring1 = new SenderKeyRing();
    ring1.setEpoch(h.active());
    await pump(ring1, 20);
    // Worker 1 dies; worker 2 is created and the host told.
    const before = h.activated.length;
    await h.host.replaceEncryptor();
    expect(h.activated).toHaveLength(before); // waits for the viewer's ack (or 300 ms)
    expect(h.host.keys.pendingDeadline).toBe(h.clock.t + 300);
    const got = await deliverRekeys(h, "a", a.session);
    expect(h.activated).toHaveLength(before + 1);
    const ring2 = new SenderKeyRing();
    ring2.setEpoch(h.active());
    const frames = await pump(ring2, 20);
    // Every epoch went to an encryptor once, and the viewer can decrypt the new one.
    const numbers = h.activated.map((k) => k.epoch);
    expect(new Set(numbers).size).toBe(numbers.length);
    const rr = new ReceiverKeyRing();
    for (const e of [...a.epochs, ...got]) rr.addEpoch(e);
    const dec = new FrameDecryptor(rr, { track: 0, kind: "video", codec: "video/VP9" });
    expect(await dec.decrypt(frames[0]!.clone())).toBe(true);
  });

  it("an unacknowledged replacement still activates within 300 ms", async () => {
    const h = await makeHost();
    await join(h, "a", viewer(h)); // never acks anything
    const e = h.host.keys.activeEpoch;
    await h.host.replaceEncryptor();
    h.clock.advance(299);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBe(e);
    h.clock.advance(1);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e);
  });
});

describe("finding 6: replay check is atomic with the commit", () => {
  it("two concurrent decryptions of the same frame: exactly one succeeds", async () => {
    const epoch = { epoch: 1, secret: randomBytes(32) };
    const sring = new SenderKeyRing();
    sring.setEpoch(epoch);
    const rring = new ReceiverKeyRing();
    rring.addEpoch(epoch);
    const enc = new FrameEncryptor(sring, { track: 0, kind: "video", codec: "video/VP9" });
    const dec = new FrameDecryptor(rring, { track: 0, kind: "video", codec: "video/VP9" });
    const warm = new FakeFrame(randomBytes(40), "video/VP9");
    await enc.encrypt(warm);
    expect(await dec.decrypt(warm)).toBe(true); // key and window exist, so both calls race the AEAD
    for (let i = 0; i < 20; i++) {
      const f = new FakeFrame(randomBytes(200), "video/VP9");
      await enc.encrypt(f);
      const copies = Array.from({ length: 4 }, () => f.clone());
      const ok = await Promise.all(copies.map((c) => dec.decrypt(c)));
      expect(ok.filter(Boolean)).toHaveLength(1);
    }
    expect(dec.stats.dropped.replay).toBe(60);
  });
});

describe("finding 7: the fragment link secret is mandatory", () => {
  it("buildFragment/parseFragment round-trip a canonical s= and refuse anything else", () => {
    const id = SenderIdentity.generate();
    const s = generateLinkSecret();
    const frag = buildFragment("cap_1-x", id.publicKey, s);
    expect(frag).toBe(`c=cap_1-x&k=${toBase64Url(id.publicKey)}&s=${toBase64Url(s)}`);
    const p = parseFragment(`#${frag}`);
    expect(toHex(p.linkSecret)).toBe(toHex(s));
    expect(toHex(p.senderKey)).toBe(toHex(id.publicKey));
    expect(buildFragment(p.capability, p.senderKey, p.linkSecret)).toBe(frag);
    const k = toBase64Url(id.publicKey);
    for (const bad of [
      `c=cap&k=${k}`, // no secret
      `c=cap&k=${k}&s=${toBase64Url(randomBytes(31))}`,
      `c=cap&k=${k}&s=${toBase64Url(randomBytes(33))}`,
      `c=cap&k=${k}&s=${toBase64Url(s).slice(0, -1)}B`, // non-canonical last char
      `c=cap&s=${toBase64Url(s)}&k=${k}`, // reordered
      `c=cap&k=${k}&s=${toBase64Url(s)}&s=${toBase64Url(s)}`, // duplicated
      `c=cap&k=${k}&s=${toBase64Url(s)}&x=1`, // extra
    ]) {
      expect(() => parseFragment(bad), bad).toThrow(expect.objectContaining({ code: "malformed" }));
    }
    expect(() => buildFragment("cap", id.publicKey, randomBytes(16))).toThrow(expect.objectContaining({ code: "invalid_argument" }));
  });

  it("both ends refuse to run without exactly 32 bytes", async () => {
    for (const ls of [undefined, new Uint8Array(0), randomBytes(31), randomBytes(33)]) {
      await expect(makeHost({ linkSecret: ls as Uint8Array })).rejects.toMatchObject({ code: "invalid_argument" });
    }
    const h = await makeHost();
    for (const ls of [undefined, randomBytes(16)]) {
      expect(() => viewer(h, { linkSecret: ls as Uint8Array })).toThrow(expect.objectContaining({ code: "invalid_argument" }));
    }
  });

  it("a server that knows (or guesses) the PIN but not the fragment cannot admit itself", async () => {
    const h = await makeHost();
    await expectReject(join(h, "server", viewer(h, { pin: PIN, linkSecret: randomBytes(32) })), "wrong_pin");
    expect(h.peers()).toEqual([]);
  });
});

describe("finding 9: finished handshakes drop their secrets", () => {
  async function senderCtx() {
    const storage = new MemoryStorage();
    const identity = SenderIdentity.generate();
    const linkSecret = generateLinkSecret();
    const grants = new GrantStore({ storage, streamId: STREAM });
    const issued: Bytes[] = [];
    const ctx: SenderAdmissionContext = {
      identity,
      streamId: STREAM,
      pin: () => PIN,
      pinGeneration: () => 0,
      linkSecret,
      budget: new GuessBudget({ storage, streamId: STREAM }),
      grants,
      currentEpoch: () => 1,
      commit: async (_adm, c) => {
        const grant = await grants.issue(c.viewerSid);
        issued.push(grant.secret);
        expect(c.claim()).toBe(true);
        const session = new SenderSession({ id: "x", viewerSid: c.viewerSid, grantId: grant.id, keys: c.keys });
        return { ok: true, admissionId: "x", session, epoch: { epoch: 1, secret: randomBytes(32) }, grant };
      },
    };
    return { ctx, issued, linkSecret };
  }

  async function run(s: SenderAdmission, v: ViewerAdmission) {
    const [challenge] = await s.handle(v.start());
    expect(s._retainedSecrets()).toEqual(["ya"]);
    const [response] = await v.handle(challenge!);
    expect(v._retainedSecrets()).toEqual(["sessionKeys"]); // PIN and link secret already gone
    const [reply] = await s.handle(response!);
    await v.handle(reply!).catch(() => undefined);
  }

  it("after admission: sender scalar and grant secret wiped, viewer holds nothing", async () => {
    const { ctx, issued, linkSecret } = await senderCtx();
    const s = new SenderAdmission(ctx, "p");
    const callerSecret = Uint8Array.from(linkSecret);
    const v = new ViewerAdmission({ streamId: STREAM, senderKey: ctx.identity.publicKey, viewerSid: randomBytes(16), linkSecret: callerSecret, pin: PIN });
    expect(v._retainedSecrets()).toEqual(["pin", "linkSecret"]);
    await run(s, v);
    expect(s.outcome?.kind).toBe("admitted");
    expect(v.state).toBe("admitted");
    expect(s._retainedSecrets()).toEqual([]);
    expect(v._retainedSecrets()).toEqual([]);
    expect(issued[0]!.every((b) => b === 0)).toBe(true); // the issued grant secret, after sealing
    expect(toHex(callerSecret)).toBe(toHex(linkSecret)); // the caller's own buffer is untouched
    expect(toHex(v.result!.grant.secret)).not.toBe(toHex(new Uint8Array(32)));
  });

  it("after a wrong PIN or an abort: nothing retained either", async () => {
    const { ctx } = await senderCtx();
    const s = new SenderAdmission(ctx, "p");
    const v = new ViewerAdmission({ streamId: STREAM, senderKey: ctx.identity.publicKey, viewerSid: randomBytes(16), linkSecret: ctx.linkSecret, pin: "0000" });
    await run(s, v);
    expect(s.outcome?.kind).toBe("rejected");
    expect(v.state).toBe("failed");
    expect(s._retainedSecrets()).toEqual([]);
    expect(v._retainedSecrets()).toEqual([]);
    const s2 = new SenderAdmission(ctx, "q");
    const v2 = new ViewerAdmission({ streamId: STREAM, senderKey: ctx.identity.publicKey, viewerSid: randomBytes(16), linkSecret: ctx.linkSecret, pin: PIN });
    await s2.handle(v2.start());
    await s2.abort("closed");
    expect(s2._retainedSecrets()).toEqual([]);
  });
});

describe("kick timing: 300 ms switch, absolute deadline across supersession", () => {
  it("a kick switches within 300 ms even if a remaining viewer never acks", async () => {
    const h = await makeHost();
    await join(h, "a", viewer(h)); // never acks
    await join(h, "c", viewer(h));
    const e = h.host.keys.activeEpoch;
    await h.host.kick(h.idOf("c"));
    h.clock.advance(299);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBe(e);
    h.clock.advance(1);
    await h.host.tick();
    expect(h.host.keys.activeEpoch).toBeGreaterThan(e);
  });

  it("superseding rekeys keep the earliest deadline instead of restarting it", async () => {
    const h = await makeHost();
    await join(h, "a", viewer(h)); // never acks
    await join(h, "c", viewer(h));
    const t0 = h.clock.t;
    await h.host.keys.rekey("manual");
    expect(h.host.keys.pendingDeadline).toBe(t0 + 1500);
    h.clock.advance(1000);
    await h.host.keys.rekey("manual");
    expect(h.host.keys.pendingDeadline).toBe(t0 + 1500); // not t0 + 2500
    h.clock.advance(100);
    await h.host.kick(h.idOf("c"));
    expect(h.host.keys.pendingDeadline).toBe(t0 + 1400); // the kick's own 300 ms is earlier
    h.clock.advance(300);
    await h.host.tick();
    expect(h.host.keys.pendingEpoch).toBeNull();
  });
});

describe("owner API: admission ids, kick + rotate, unlock policy, viewer-reported labels", () => {
  it("owner actions take admission ids, never transport ids; labels are display-only", async () => {
    const h = await makeHost();
    await join(h, "p1", viewer(h));
    await join(h, "p2", viewer(h));
    await expect(h.host.kick("p1")).rejects.toMatchObject({ code: "invalid_argument" });
    // The relay swaps the labels: the owner still kicks the admission it picked.
    h.host.setViewerReportedLabel(h.idOf("p1"), "Bob");
    h.host.setViewerReportedLabel(h.idOf("p2"), "Alice");
    const target = h.idOf("p1");
    await h.host.kick(target);
    expect(h.host.viewers().map((v) => [v.peerId, v.viewerReportedLabel])).toEqual([["p2", "Alice"]]);
    expect(() => h.host.setViewerReportedLabel(h.idOf("p2"), "x".repeat(65))).toThrow();
  });

  it("a kicked viewer who knows the PIN can come back; kickAndRotatePin keeps them out", async () => {
    const h = await makeHost();
    const stay = await join(h, "stay", viewer(h));
    await join(h, "bad", viewer(h));
    await h.host.kick(h.idOf("bad"));
    await join(h, "bad", viewer(h)); // documented limitation of kick() alone
    h.take("stay");
    await h.host.kickAndRotatePin(h.idOf("bad"), "1357");
    await expectReject(join(h, "bad2", viewer(h)), "wrong_pin");
    expect(h.peers()).toEqual(["stay"]);
    // One fast rekey; the remaining viewer gets it and the switch completes.
    expect((await deliverRekeys(h, "stay", stay.session)).length).toBeGreaterThanOrEqual(1);
    expect(h.host.keys.pendingEpoch).toBeNull();
    await join(h, "new", viewer(h, { pin: "1357" }));
  });

  it("unlock after a freeze rotates the PIN unless keepPin is explicit", async () => {
    const h = await makeHost({ limits: { perStream: 2 } });
    for (let i = 0; i < 2; i++) await expectReject(join(h, `b${i}`, viewer(h, { pin: "0000" })), i === 1 ? "frozen" : "wrong_pin");
    await expect(h.host.unlock(undefined as never)).rejects.toMatchObject({ code: "invalid_argument" });
    await expect(h.host.unlock({} as never)).rejects.toMatchObject({ code: "invalid_argument" });
    expect((await h.host.budget.status()).frozen).toBe(true);
    await h.host.unlock({ keepPin: true });
    await join(h, "ok", viewer(h)); // explicit override: same PIN
    for (let i = 0; i < 2; i++) await expectReject(join(h, `c${i}`, viewer(h, { pin: "0000" })), i === 1 ? "frozen" : "wrong_pin");
    await h.host.unlock({ newPin: "2468" });
    await expectReject(join(h, "old", viewer(h)), "wrong_pin");
    await join(h, "new", viewer(h, { pin: "2468" }));
  });
});
