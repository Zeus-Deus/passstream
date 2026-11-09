import { useSyncExternalStore } from "react";
import type { LocalChannel, PublishPrompt } from "./local-channel";
import { deliverPublishApproval, mintPublishApproval } from "./publish-approval";

export function PublishConfirmation({channel}:{channel:LocalChannel}) {
 const request=useSyncExternalStore(channel.subscribePublish,channel.getPublishPrompt);
 if(!request)return null;
 return <PublishPreview request={request} channel={channel} />;
}
export function PublishPreview({request,channel}:{request:PublishPrompt;channel:Pick<LocalChannel,"refusePublish">}) {
 return <section role="dialog" aria-modal="true" aria-label="Confirm publishing" className="space-y-3 rounded border-2 border-stamp bg-paper-sheet p-4">
  <h2 className="font-semibold">Publish {request.kind}?</h2>
  <p>Destination: <strong>{new URL(request.destination).host}</strong></p>
  <p>This uploads the selected capture and creates a shareable link.</p>
  {request.preview && <img src={`data:image/png;base64,${request.preview}`} alt="Snapshot to publish" className="max-w-full" />}
  <p>{request.bytes.toLocaleString()} bytes{request.kind==="recording" ? ` · ${(request.duration_ms/1000).toFixed(1)} seconds` : ""}. This request expires in 30 seconds.</p>
  <div className="flex gap-3">
   <button type="button" className="rounded bg-stamp px-3 py-2 text-paper" onClick={(event)=>{
     if (!event.isTrusted) return;
     const approval = mintPublishApproval(channel, request.request_id, event.nativeEvent, event.currentTarget);
     if (approval) deliverPublishApproval(channel, approval);
   }}>Confirm upload</button>
   <button type="button" className="rounded border px-3 py-2" onClick={()=>channel.refusePublish(request.request_id)}>Refuse</button>
  </div>
 </section>;
}
