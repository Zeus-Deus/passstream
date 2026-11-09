// Capabilities never cross the local wire or appear on the channel object.
// The daemon authenticates the page; this boundary enforces its UI gesture.
export type PublishApproval = Readonly<{ requestId: string }>;

type Grant = { channel: object; requestId: string };
const grants = new WeakMap<object, Grant>();
const receivers = new WeakMap<object, (approval: unknown) => boolean>();
const usedClicks = new WeakSet<Event>();
const composedPath = Event.prototype.composedPath;

export function registerPublishReceiver(channel: object, current: () => string | undefined, send: (id: string) => boolean) {
  receivers.set(channel, (approval) => {
    if (typeof approval !== "object" || approval === null) return false;
    const grant = grants.get(approval);
    grants.delete(approval);
    if (!grant || grant.channel !== channel || grant.requestId !== current()) return false;
    return send(grant.requestId);
  });
}

/** Called synchronously by the confirmation button's trusted click handler. */
export function mintPublishApproval(channel: object, requestId: string, event: MouseEvent, button: HTMLButtonElement): PublishApproval | null {
  try {
    // Calling the native method also rejects objects that only pretend to be
    // trusted Events. A completed/replayed event has no active dispatch path.
    if (!(event instanceof MouseEvent) || !event.isTrusted || event.type !== "click" ||
        event.eventPhase === Event.NONE || usedClicks.has(event) || !button.isConnected ||
        !composedPath.call(event).includes(button) || !(event.target instanceof Node) || !button.contains(event.target)) return null;
  } catch { return null; }
  usedClicks.add(event);
  const approval = Object.freeze({ requestId });
  grants.set(approval, { channel, requestId });
  // A capability is handed directly to the receiver, never saved for later.
  queueMicrotask(() => grants.delete(approval));
  return approval;
}

export function deliverPublishApproval(channel: object, approval: unknown): boolean {
  return receivers.get(channel)?.(approval) ?? false;
}
