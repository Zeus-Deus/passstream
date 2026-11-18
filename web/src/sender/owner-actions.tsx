import { useLayoutEffect, useRef, useState } from "react";
import { PublishConfirmation } from "./publish-confirmation";
import type { LocalChannel } from "./local-channel";

/** Each click is a distinct, explicit owner action on the local channel. */
export function OwnerActions({ channel }: { channel: LocalChannel }) {
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
      let result=r;
      if(r.ok && op==="publish") {
        setMessage("Waiting for owner confirmation in this sender window");
        while(result.ok && (result.data?.status==="pending" || result.data?.uploading===true)) {
          await new Promise((resolve)=>setTimeout(resolve,500));
          result=await channel.ownerAction("publish.status",{request_id:r.data?.request_id,wait:0});
        }
      }
      setMessage(result.ok ? result.data?.url ? "Published" : JSON.stringify(result.data) : result.error?.code ?? "failed");
      if (result.ok && typeof result.data?.url === "string") setLink(result.data.url);
    } finally { setBusy(false); }
  };
  return <section aria-label="Owner captures" className="space-y-2 rounded-md border border-paper-edge p-3">
    <PublishConfirmation channel={channel} />
    <p className="text-sm text-ink-mute">Captures stay on this machine. Publish sends the selected capture to your configured Pass service.</p>
    <div className="flex flex-wrap gap-2">
      {([ ["Start recording", "record.start", {}], ["Stop recording", "record.stop", {}],
        ["Snapshot → PassPage", "publish", { kind: "snapshot", service: "passpage" }],
        ["Snapshot → PassFile", "publish", { kind: "snapshot", service: "passfile" }],
        ["Recording → PassFile", "publish", { kind: "recording", service: "passfile" }],
      ] as const).map(([label, op, args]) => (
        <button type="button" key={label} disabled={busy}
          className="rounded border border-paper-edge px-3 py-2 text-sm disabled:opacity-40"
          onClick={(event) => void run(op, args, event.currentTarget)}>
          {label}
        </button>
      ))}
    </div>
    <p role="status" className="text-sm">{message} {link && <a href={link} target="_blank" rel="noreferrer">Open published capture</a>}</p>
  </section>;
}
