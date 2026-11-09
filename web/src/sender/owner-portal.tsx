import { createRoot } from "react-dom/client";
import { useState } from "react";
import "../assets/theme.css";

export function OwnerPortal() {
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [link, setLink] = useState("");
  const stream = new URLSearchParams(location.search).get("stream_id") ?? "";
  const run = async (op: string, args: Record<string, unknown>) => {
    setBusy(true); setMessage(""); setLink("");
    try {
      const response = await fetch("/local/v1/features", { method: "POST", headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json", "X-PassStream-Stream": stream }, body: JSON.stringify({ op, args }), credentials: "omit" });
      let result = await response.json();
      if(response.ok && op==="publish") {
        setMessage("Confirm this upload in the local sender window");
        while(result.status==="pending" || result.uploading===true) {
          await new Promise((resolve)=>setTimeout(resolve,500));
          const status=await fetch("/local/v1/features",{method:"POST",headers:{Authorization:`Bearer ${token}`,"Content-Type":"application/json","X-PassStream-Stream":stream},body:JSON.stringify({op:"publish.status",args:{request_id:result.request_id}}),credentials:"omit"});
          result=await status.json();if(!status.ok)break;
        }
      }
      if (!response.ok) setMessage(result.error?.code ?? "failed");
      else if (typeof result.url === "string") { setLink(result.url); setMessage("Published"); }
      else setMessage(JSON.stringify(result));
    } catch { setMessage("Local host unavailable"); }
    finally { setBusy(false); }
  };
  return <main className="min-h-dvh bg-paper p-6 text-ink"><div className="mx-auto max-w-xl space-y-5">
    <h1 className="text-2xl font-semibold">Owner captures</h1>
    <p>This page is served by your local PassStream host. Publishing sends a snapshot or recording to your configured Pass service.</p>
    <label className="block space-y-2"><span>Run <code>passstream capture-auth</code> on the host. Paste its local token here (valid for 5 minutes).</span>
      <input aria-label="Local owner token" type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" className="w-full rounded border border-paper-edge p-2" />
    </label>
    <div className="flex flex-wrap gap-2">{([
      ["Start recording", "record.start", {}], ["Stop recording", "record.stop", {}],
      ["Snapshot → PassPage", "publish", { kind: "snapshot", service: "passpage" }],
      ["Snapshot → PassFile", "publish", { kind: "snapshot", service: "passfile" }],
      ["Recording → PassFile", "publish", { kind: "recording", service: "passfile" }],
    ] as const).map(([label, op, args]) => <button type="button" key={label} disabled={busy || !token} onClick={() => void run(op, args)} className="rounded border border-paper-edge px-3 py-2 disabled:opacity-40">{label}</button>)}</div>
    <p role="status">{message} {link && <a href={link} target="_blank" rel="noreferrer">Open published capture</a>}</p>
  </div></main>;
}

export function mount(el: HTMLElement) { createRoot(el).render(<OwnerPortal />); }
