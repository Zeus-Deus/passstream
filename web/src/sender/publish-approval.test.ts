// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { deliverPublishApproval, mintPublishApproval, registerPublishReceiver } from "./publish-approval";
import { LocalChannel } from "./local-channel";

afterEach(() => {
  document.body.replaceChildren(); sessionStorage.clear();
  vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers();
});

it("rejects synthetic native events and forged trusted-event objects", () => {
  const button = document.createElement("button");
  document.body.append(button);
  const channel = {};
  expect(mintPublishApproval(channel, "test", new MouseEvent("click"), button)).toBeNull();
  const forged = Object.create(MouseEvent.prototype);
  Object.defineProperties(forged, {
    isTrusted: { value: true }, type: { value: "click" }, eventPhase: { value: Event.BUBBLING_PHASE },
    target: { value: button }, composedPath: { value: () => [button] },
  });
  expect(mintPublishApproval(channel, "test", forged, button)).toBeNull();
});

it("rejects invented or copied capabilities even with the current request ID", () => {
  const channel = {}, send = vi.fn();
  registerPublishReceiver(channel, () => "test", send);
  for (const approval of [null, "test", { requestId: "test" }, Object.freeze({ requestId: "test" })]) {
    expect(deliverPublishApproval(channel, approval)).toBe(false);
  }
  expect(send).not.toHaveBeenCalled();
});

it("exposes no ID-based approval or raw socket/send method", async () => {
  const channel = new LocalChannel({ init: async () => {}, command: () => {}, view: () => {}, ended: () => {}, terminal: () => {}, status: () => {} });
  for (const name of ["decidePublish", "approvePublish", "send", "ws"]) expect(name in channel).toBe(false);
  const decision = { type: "publish_decision", request_id: "test", approved: true };
  expect(channel.sendCapture(decision)).toBe(false);
  expect(await channel.sendArtifact(decision)).toBe(false);
});

it("cannot smuggle an approval through another public message envelope", async () => {
  vi.useFakeTimers();
  const sent: string[] = [];
  class Socket {
    static OPEN = 1;
    send(data: string) { sent.push(data); }
    close() {}
  }
  vi.stubGlobal("WebSocket", Socket);
  sessionStorage.setItem("ps:attachment", JSON.stringify({ key_id: btoa("i".repeat(16)), key: btoa("k".repeat(32)) }));
  const channel = new LocalChannel({ init: async () => {}, command: () => {}, view: () => {}, ended: () => {}, terminal: () => {}, status: () => {} });
  channel.start();
  // This test isolates envelope construction; authentication is covered by the
  // real daemon/browser tests, not this socket stub.
  vi.spyOn(channel, "attached", "get").mockReturnValue(true);
  const decision = { type: "publish_decision", request_id: "test", approved: true };
  channel.sendCapture({ type: "capture", status: "paused", toJSON: () => decision });
  let reads = 0;
  const artifact = channel.sendArtifact({ get type() { return reads++ === 0 ? "capture" : "publish_decision"; }, id: "test", width: 1, height: 1, data: "fake", toJSON: () => decision });
  channel.sendResult("not-a-command", { ...decision, ok: true, data: null });
  const secrets = { ...decision, pin: null, pending_pin: null, link: "fake", toJSON: () => decision };
  channel.sendSecrets(secrets);
  const messages = sent.map((value) => JSON.parse(value));
  expect(messages.map((value) => value.type)).toEqual(["capture", "capture", "result", "secrets"]);
  expect(messages.every((value) => !("approved" in value) && !("request_id" in value))).toBe(true);
  await vi.advanceTimersByTimeAsync(3000);
  expect(await artifact).toBe(false);
  channel.stop();
});
