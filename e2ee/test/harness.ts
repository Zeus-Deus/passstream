import { ViewerAdmission, type ViewerAdmissionOptions, type ViewerAdmissionResult } from "../src/admission.ts";
import { type Bytes, randomBytes } from "../src/bytes.ts";
import type { EpochKey } from "../src/channel.ts";
import { E2EEError } from "../src/errors.ts";
import { SenderHost, type SenderHostOptions } from "../src/host.ts";
import { generateLinkSecret, SenderIdentity } from "../src/identity.ts";
import { MemoryStorage } from "../src/storage.ts";

export class FakeClock {
  t = 1_700_000_000_000;
  now = (): number => this.t;
  advance(ms: number): void {
    this.t += ms;
  }
}

export const STREAM = "stream-abc123";
export const PIN = "4821";

/**
 * A sender host whose outgoing messages are captured per peer, so a test can
 * play the relay: deliver, drop, tamper, replay or reorder.
 */
export async function makeHost(over: Partial<SenderHostOptions> = {}) {
  const clock = new FakeClock();
  const outbox = new Map<string, Bytes[]>();
  const activated: EpochKey[] = [];
  let keyframes = 0;
  let suspends = 0;
  const storage = (over.storage as MemoryStorage | undefined) ?? new MemoryStorage();
  const identity = over.identity ?? SenderIdentity.generate();
  const linkSecret = over.linkSecret ?? generateLinkSecret();
  const host = await SenderHost.create({
    identity,
    streamId: STREAM,
    pin: PIN,
    linkSecret,
    storage,
    now: clock.now,
    send: (peer, b) => {
      const q = outbox.get(peer) ?? [];
      q.push(b);
      outbox.set(peer, q);
    },
    onEpoch: (k) => activated.push(k),
    onKeyframeNeeded: () => keyframes++,
    onSuspend: () => suspends++,
    ...over,
  });
  const errors: E2EEError[] = [];
  host.on("protocolError", (e) => errors.push(e.error));
  return {
    host,
    clock,
    storage,
    identity,
    linkSecret,
    activated,
    errors,
    keyframes: () => keyframes,
    /** How often encryption was suspended (onSuspend). */
    suspends: () => suspends,
    /** The active epoch key, as delivered to the (single) encryptor via onEpoch. */
    active(): EpochKey {
      return activated.at(-1)!;
    },
    /** Admission id of the connected admission on a transport peer. */
    idOf(peer: string): string {
      const v = host.viewers().find((x) => x.peerId === peer && x.connected);
      if (!v) throw new Error(`no connected admission on ${peer}`);
      return v.admissionId;
    },
    /** Transport peers that currently receive rekeys. */
    peers(): string[] {
      return host
        .viewers()
        .filter((x) => x.connected)
        .map((x) => x.peerId)
        .sort();
    },
    /** Take everything queued for a peer. */
    take(peer: string): Bytes[] {
      const q = outbox.get(peer) ?? [];
      outbox.set(peer, []);
      return q;
    },
  };
}

export type Host = Awaited<ReturnType<typeof makeHost>>;

export function viewer(h: Host, over: Partial<ViewerAdmissionOptions> = {}): ViewerAdmission {
  return new ViewerAdmission({
    streamId: STREAM,
    senderKey: h.identity.publicKey,
    viewerSid: randomBytes(16),
    linkSecret: h.linkSecret,
    pin: PIN,
    ...over,
  });
}

/** Run a full honest handshake through the relay. Throws the viewer's error. */
export async function join(h: Host, peer: string, v: ViewerAdmission): Promise<ViewerAdmissionResult> {
  await h.host.receive(peer, v.start());
  for (let i = 0; i < 4 && v.state !== "admitted"; i++) {
    const msgs = h.take(peer);
    if (msgs.length === 0) throw new Error(`sender sent nothing (state ${v.state}); errors: ${h.errors.map(String).join(", ")}`);
    for (const m of msgs) {
      const replies = await v.handle(m);
      for (const r of replies) await h.host.receive(peer, r);
    }
  }
  return v.result!;
}

export async function expectReject(p: Promise<unknown>, reason: string): Promise<E2EEError> {
  try {
    await p;
  } catch (e) {
    if (e instanceof E2EEError && e.code === "rejected" && e.details?.reason === reason) return e;
    throw new Error(`expected rejected/${reason}, got ${String(e)}`);
  }
  throw new Error(`expected rejected/${reason}, but succeeded`);
}
