import { afterEach, expect, it, vi } from "vitest";
import { MsgType, peekType } from "../src/wire.ts";
import { expectReject, join, makeHost, viewer } from "./harness.ts";

afterEach(() => vi.restoreAllMocks());

it.each(["close", "peerClosed"] as const)("%s from a real disconnect callback disposes a claimed replacement", async (action) => {
  const h = await makeHost();
  await join(h, "p", viewer(h));
  const old = h.idOf("p");
  let stopping: Promise<void> | undefined;
  h.host.on("disconnected", (event) => {
    if (event.admissionId === old) stopping = action === "close" ? h.host.close() : h.host.peerClosed("p");
  });
  const next = viewer(h);
  await h.host.receive("p", next.start());
  const [response] = await next.handle(h.take("p")[0]!);
  await h.host.receive("p", response!);
  expect(stopping).toBeDefined();
  await stopping;
  const fresh = h.host.viewers().find((record) => record.admissionId !== old)!;
  expect(fresh.connected).toBe(false);
  expect(h.host.annotationAllowed(fresh.admissionId)).toBe(false);
  expect(h.host.keys.sessionIds()).toEqual([]);
  expect(h.take("p")).toEqual([]);
});

it.each([false, true])("terminal close disposes late registration even when it fails (failure=%s)", async (registrationFails) => {
  const h = await makeHost();
  await join(h, "p", viewer(h));
  const old = h.idOf("p"), next = viewer(h);
  await h.host.receive("p", next.start());
  const [response] = await next.handle(h.take("p")[0]!);
  let entered!: () => void, release!: () => void;
  const ready = new Promise<void>((resolve) => { entered = resolve; });
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const remove = h.host.keys.remove.bind(h.host.keys);
  vi.spyOn(h.host.keys, "remove").mockImplementationOnce(async (id) => {
    expect(id).toBe(old);
    await remove(id); entered(); await gate;
  });
  if (registrationFails) {
    const admit = h.host.keys.admit.bind(h.host.keys);
    vi.spyOn(h.host.keys, "admit").mockImplementationOnce(async (session) => {
      await admit(session);
      throw new Error("key registration completion failed");
    });
  }
  const replacing = h.host.receive("p", response!);
  await ready;
  const fresh = h.idOf("p");
  await h.host.close();
  expect(h.host.keys.sessionIds()).toEqual([]);
  release(); await replacing;
  expect(h.errors).toHaveLength(registrationFails ? 1 : 0);
  expect(h.host.viewers().every((record) => !record.connected)).toBe(true);
  expect(h.host.annotationAllowed(fresh)).toBe(false);
  expect(h.host.keys.sessionIds()).toEqual([]);
  expect(h.take("p")).toEqual([]);
  const epochs = h.activated.length;
  await h.host.close();
  await h.host.keys.rekey();
  await h.host.tick();
  expect(h.host.keys.sessionIds()).toEqual([]);
  expect(h.activated).toHaveLength(epochs);
  await expectReject(join(h, "after-close", viewer(h)), "locked");
});

it.each([false, true])("queued key output respects terminal stop while live output continues (close=%s)", async (stop) => {
  const h = await makeHost();
  await join(h, "keep", viewer(h));
  await join(h, "later", viewer(h));
  const laterId = h.idOf("later");
  let allocationEntered!: () => void, releaseAllocation!: () => void, admitQueued!: () => void;
  const allocationReady = new Promise<void>((resolve) => { allocationEntered = resolve; });
  const allocationGate = new Promise<void>((resolve) => { releaseAllocation = resolve; });
  const queued = new Promise<void>((resolve) => { admitQueued = resolve; });
  const update = h.storage.update.bind(h.storage);
  vi.spyOn(h.storage, "update").mockImplementation(async (key, fn) => {
    if (key.startsWith("epoch:")) { allocationEntered(); await allocationGate; }
    return update(key, fn);
  });
  const rekeying = h.host.keys.rekey();
  await allocationReady;
  const admit = h.host.keys.admit.bind(h.host.keys);
  vi.spyOn(h.host.keys, "admit").mockImplementation((session) => {
    const pending = admit(session); admitQueued(); return pending;
  });
  const next = viewer(h);
  await h.host.receive("p", next.start());
  const [response] = await next.handle(h.take("p")[0]!);
  const pending = h.host.receive("p", response!);
  await queued;
  const epochs = h.activated.length;
  let closing: Promise<void> | undefined;
  let stoppedAnnotationAllowed: boolean | undefined, stoppedAnnotationSent: boolean | undefined;
  if (stop) {
    let removalEntered!: () => void;
    const removalReady = new Promise<void>((resolve) => { removalEntered = resolve; });
    const remove = h.host.keys.remove.bind(h.host.keys);
    vi.spyOn(h.host.keys, "remove").mockImplementation((id) => {
      removalEntered(); return remove(id);
    });
    closing = h.host.close();
    expect(h.host.keys.suspended).toBe(true);
    await removalReady;
    await expectReject(join(h, "blocked", viewer(h)), "locked");
    stoppedAnnotationAllowed = h.host.annotationAllowed(laterId);
    stoppedAnnotationSent = await h.host.sendAnnotations(laterId, [{ origin: "host", body: Uint8Array.of(1) }]);
  }
  releaseAllocation();
  await Promise.all([rekeying, pending, closing]);
  const messages = h.take("p"), keepMessages = h.take("keep"), laterMessages = h.take("later");
  expect(h.errors).toEqual([]);
  if (stop) {
    expect(stoppedAnnotationAllowed).toBe(false);
    expect(stoppedAnnotationSent).toBe(false);
    expect(messages).toEqual([]);
    expect(keepMessages).toEqual([]);
    expect(laterMessages).toEqual([]);
    expect(h.host.keys.sessionIds()).toEqual([]);
    expect(h.host.viewers().every((record) => !record.connected)).toBe(true);
    await h.host.keys.rekey();
    h.clock.advance(600_001); await h.host.tick();
    expect(h.activated).toHaveLength(epochs);
    expect(h.host.keys.suspended).toBe(true);
  } else {
    expect(messages.map(peekType)).toEqual([MsgType.Rekey, MsgType.Admit]);
    expect(keepMessages.map(peekType)).toEqual([MsgType.Rekey]);
    expect(laterMessages.map(peekType)).toEqual([MsgType.Rekey]);
    expect(h.host.annotationAllowed(laterId)).toBe(true);
    await next.handle(messages[1]!);
    expect(next.state).toBe("admitted");
    expect(h.host.keys.hasSession(h.idOf("p"))).toBe(true);
  }
});
