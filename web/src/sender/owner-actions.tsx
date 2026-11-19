import { useLayoutEffect, useRef, useState } from "react";
import { PublishConfirmation } from "./publish-confirmation";
import type { LocalChannel } from "./local-channel";

type Reply = Awaited<ReturnType<LocalChannel["ownerAction"]>>;

const ERRORS: Record<string, string> = {
  paused: "There's no picture to capture while the screen is paused or not shared.",
  not_live: "This stream isn't live on this machine any more.",
  conflict: "That's already in progress.",
  not_found: "There's no recording to stop or publish yet.",
  not_supported: "This browser can't record.",
  sender_offline: "This window lost its connection to PassStream on this machine.",
  sender_timeout: "That took too long and was given up.",
  upload_failed: "The upload failed. Check the Pass service, then try again.",
  recording_too_large: "The recording got too large and was dropped.",
  recording_failed: "The recording failed.",
  invalid_argument: "That request was refused. Check the Pass service settings on this machine.",
};

const explain = (code: string) => ERRORS[code] ?? `That didn't work (${code}).`;

/** What happened, in words. Captures never show their raw reply. */
export function ownerActionMessage(op: string, r: Reply): string {
  if (!r.ok) return explain(r.error?.code ?? "failed");
  // A reply can succeed and still carry a failure: an upload error from the
  // Pass service (an object with a code), or a recording error (the code itself).
  const failure = r.data?.error;
  const code = typeof failure === "string" ? failure : typeof (failure as { code?: unknown } | undefined)?.code === "string" ? (failure as { code: string }).code : "";
  if (code) return explain(code);
  if (op === "record.start") return "Recording. It stops by itself after two minutes.";
  if (op === "record.stop") return r.data?.completed === true ? "Recording saved on this machine." : "No recording was saved.";
  // Only a link means it was published.
  if (typeof r.data?.url === "string") return "Published.";
  if (r.data?.status === "timeout") return "The confirmation ran out, so nothing was published.";
  return "Not published.";
}

const ACTIONS = [
  ["Start recording", "record.start", {}],
  ["Stop recording", "record.stop", {}],
  ["Snapshot → PassPage", "publish", { kind: "snapshot", service: "passpage" }],
  ["Snapshot → PassFile", "publish", { kind: "snapshot", service: "passfile" }],
  ["Recording → PassFile", "publish", { kind: "recording", service: "passfile" }],
] as const;

/**
 * Recording and publishing, out of the way until asked for. Each click is a
 * distinct, explicit owner action on the local channel.
 */
export function OwnerActions({ channel, capturing }: { channel: LocalChannel; capturing: boolean }) {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [link, setLink] = useState("");
  const publishTrigger = useRef<HTMLButtonElement | null>(null);
  useLayoutEffect(() => {
    if (busy) return;
    // The dialog can close while its trigger is still disabled by the status poll.
    // Keep any focus the user moved elsewhere while waiting.
    if (document.activeElement === document.body) publishTrigger.current?.focus();
    publishTrigger.current = null;
  }, [busy]);

  const run = async (op: string, args: Record<string, unknown>, trigger: HTMLButtonElement) => {
    publishTrigger.current = op === "publish" ? trigger : null;
    setBusy(true); setMessage(""); setLink("");
    try {
      const r = await channel.ownerAction(op, args);
      let result = r;
      if (r.ok && op === "publish") {
        setMessage("Confirm the upload above to publish.");
        while (result.ok && (result.data?.status === "pending" || result.data?.uploading === true)) {
          await new Promise((resolve) => setTimeout(resolve, 500));
          result = await channel.ownerAction("publish.status", { request_id: r.data?.request_id, wait: 0 });
        }
      }
      setMessage(ownerActionMessage(op, result));
      if (result.ok && typeof result.data?.url === "string") setLink(result.data.url);
    } finally { setBusy(false); }
  };
  return (
    <>
      {/* Outside the disclosure: a pending upload must be seen to be confirmed. */}
      <PublishConfirmation channel={channel} />
      <details className="rounded-md border border-paper-edge px-3 py-2" data-testid="owner-captures">
        <summary className="min-h-9 cursor-pointer content-center text-[0.8rem] tracking-[0.12em] text-ink-mute uppercase">Record and publish</summary>
        <section aria-label="Owner captures" className="mt-2 space-y-3 pb-1">
          <p className="text-sm text-ink-mute">
            Recordings and snapshots stay on this machine. Publishing uploads the one you choose to your Pass service, after you confirm it here.
          </p>
          {!capturing && <p className="text-sm text-ink-mute">Available once you're sharing.</p>}
          <div className="flex flex-wrap gap-2">
            {ACTIONS.map(([label, op, args]) => (
              <button type="button" key={label} disabled={busy || !capturing} className="btn" onClick={(event) => void run(op, args, event.currentTarget)}>
                {label}
              </button>
            ))}
          </div>
          <p role="status" className="min-h-5 text-sm">
            {message} {link && <a className="underline" href={link} target="_blank" rel="noreferrer">Open published capture</a>}
          </p>
        </section>
      </details>
    </>
  );
}
