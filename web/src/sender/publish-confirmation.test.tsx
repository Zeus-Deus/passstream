// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PublishPreview } from "./publish-confirmation";
import type { PublishPrompt } from "./local-channel";
import { registerPublishReceiver } from "./publish-approval";

afterEach(cleanup);
const request: PublishPrompt={request_id:"test",kind:"snapshot",service:"passfile",destination:"https://files.example:8443",bytes:2048,duration_ms:0,preview:"aGVsbG8=",expires_at:Date.now()+30000};
it("shows the exact destination, frozen preview and shareable-link warning before any action",()=>{
 const approve=vi.fn();const channel={refusePublish:vi.fn()};registerPublishReceiver(channel,()=>request.request_id,approve);
 render(<PublishPreview request={request} channel={channel}/>);
 expect(screen.getByText("files.example:8443")).toBeDefined();expect(screen.getByAltText("Snapshot to publish").getAttribute("src")).toBe("data:image/png;base64,aGVsbG8=");
 expect(screen.getByText(/shareable link/)).toBeDefined();expect(approve).not.toHaveBeenCalled();
 fireEvent.click(screen.getByRole("button",{name:"Confirm upload"}));expect(approve).not.toHaveBeenCalled();
 fireEvent.click(screen.getByRole("button",{name:"Refuse"}));expect(channel.refusePublish).toHaveBeenCalledExactlyOnceWith("test");
});
it("shows recording size and duration before confirmation",()=>{
 render(<PublishPreview request={{...request,kind:"recording",preview:null,duration_ms:12000,bytes:1048576}} channel={{refusePublish:vi.fn()}}/>);
 expect(screen.getByText(/12.0 seconds/).textContent).toContain((1048576).toLocaleString());expect(screen.queryByRole("img")).toBeNull();
});
