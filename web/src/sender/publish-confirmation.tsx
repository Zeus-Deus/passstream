import { useLayoutEffect, useRef, useSyncExternalStore } from "react";
import type { LocalChannel, PublishPrompt } from "./local-channel";
import { deliverPublishApproval, mintPublishApproval } from "./publish-approval";

export function PublishConfirmation({ channel }: { channel: LocalChannel }) {
  const request = useSyncExternalStore(channel.subscribePublish, channel.getPublishPrompt);
  if (!request) return null;
  return <PublishPreview key={request.request_id} request={request} channel={channel} />;
}

export function PublishPreview({ request, channel }: { request: PublishPrompt; channel: Pick<LocalChannel, "refusePublish"> }) {
  const ref = useRef<HTMLDialogElement>(null);
  useLayoutEffect(() => {
    const dialog = ref.current!;
    dialog.showModal();
    return () => dialog.close();
  }, []);

  const refuse = () => channel.refusePublish(request.request_id);
  return (
    <dialog ref={ref} aria-label="Confirm publishing"
      className="fixed inset-0 m-auto max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-md space-y-3 overflow-y-auto rounded-md border-2 border-stamp bg-paper-sheet p-5 text-ink backdrop:bg-ink/60"
      onCancel={(event) => { event.preventDefault(); refuse(); }}>
      <h2 className="display text-[1.5rem]">Publish {request.kind}?</h2>
      <p>Upload to <strong className="break-all">{new URL(request.destination).host}</strong></p>
      <p>This uploads the selected capture and creates a shareable link.</p>
      {request.preview && <img src={`data:image/png;base64,${request.preview}`} alt="Snapshot to publish" className="mx-auto max-h-[35dvh] max-w-full object-contain" />}
      <p>{request.bytes.toLocaleString()} bytes{request.kind === "recording" ? ` · ${(request.duration_ms / 1000).toFixed(1)} seconds` : ""}. This request expires in 30 seconds.</p>
      <div className="flex flex-wrap justify-end gap-3">
        <button type="button" className="btn" onClick={refuse}>Refuse</button>
        <button type="button" className="btn btn-primary" onClick={(event) => {
          if (!event.isTrusted) return;
          const approval = mintPublishApproval(channel, request.request_id, event.nativeEvent, event.currentTarget);
          if (approval) deliverPublishApproval(channel, approval);
        }}>Confirm upload</button>
      </div>
    </dialog>
  );
}
