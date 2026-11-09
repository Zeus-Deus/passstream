import { Fragment, useEffect, useState, type ReactNode } from "react";
import { LOCAL_ONLY, rowActions } from "./actions";
import {
  ago,
  durationShort,
  flag,
  pathText,
  place,
  rowTitle,
  signalBars,
  statusOf,
  viewerName,
  watchTime,
} from "./format";
import type { Viewer } from "./types";
import { DeviceIcon, IconChevron, IconEyeOff, Provenance, RowCodeChip, SignalBars } from "./ui";

export function useWide(query = "(min-width: 640px)") {
  const get = () => (typeof window.matchMedia === "function" ? window.matchMedia(query).matches : true);
  const [wide, setWide] = useState(get);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const m = window.matchMedia(query);
    const on = () => setWide(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, [query]);
  return wide;
}

export interface RowHandlers {
  onKick: (v: Viewer, lock: boolean) => void;
  onBan: (v: Viewer) => void;
  busy?: string | null; // row_id with a command in flight
  ended?: boolean;
}

function prov(v: Viewer, field: string, fallback: string) {
  return v.provenance?.[field] ?? fallback;
}

function Status({ v }: { v: Viewer }) {
  const s = statusOf(v);
  return (
    <span className="inline-flex items-center gap-2" title={s.text}>
      <span aria-hidden="true" className={`size-2 shrink-0 rounded-full ${s.dot} ${v.status === "reconnecting" ? "animate-breathe" : ""}`} />
      <span className="sr-only">{s.text}</span>
    </span>
  );
}

function Place({ v }: { v: Viewer }) {
  const g = v.connections[0]?.geo;
  const text = place(g);
  if (!text) return <span className="text-ink-mute">—</span>;
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      {g?.country && (
        <span role="img" aria-label={g.country}>
          {flag(g.country)}
        </span>
      )}
      <span className="truncate">{g?.city ?? g?.country}</span>
    </span>
  );
}

function Watch({ v, now }: { v: Viewer; now: number }) {
  const hidden = v.connections[0]?.reported.tab_hidden === true;
  const t = watchTime(v, now);
  return (
    <span className="num inline-flex items-center gap-1.5 text-[0.82rem] text-ink-soft">
      {hidden && (
        <span role="img" aria-label="Tab hidden (viewer-reported)" title="Tab hidden (viewer-reported)">
          <IconEyeOff className="size-4 text-warn" />
        </span>
      )}
      {v.status === "left" && v.left_at ? `left ${ago(v.left_at, now)}` : t || "—"}
    </span>
  );
}

function Name({ v }: { v: Viewer }) {
  const n = viewerName(v);
  return (
    <span className="min-w-0">
      <span className="block truncate font-medium text-ink">{rowTitle(v)}</span>
      {n && <span className="block text-[0.68rem] text-warn italic">viewer-reported</span>}
    </span>
  );
}

function Flags({ v }: { v: Viewer }) {
  const text: Record<string, string> = {
    shared_connection: "shared connection",
    no_connection: "no connection",
    sender_stale: "sender stale",
  };
  if (!v.flags.length && !v.sender?.kick_incomplete) return null;
  return (
    <span className="flex flex-wrap gap-1">
      {v.flags.map((f) => (
        <span key={f} className="rounded-sm bg-warn-tint px-1 font-mono text-[0.62rem] text-warn">
          {text[f] ?? f}
        </span>
      ))}
      {v.sender?.kick_incomplete && (
        <span className="rounded-sm bg-stamp-tint px-1 font-mono text-[0.62rem] text-stamp">kick incomplete</span>
      )}
    </span>
  );
}

/** Every field with where it came from (HOST-CONTROL §3.1). */
export function Details({ v, now }: { v: Viewer; now: number }) {
  const c = v.connections[0];
  const st = c?.stats;
  const rows: [string, ReactNode, string][] = [
    ["Row code", v.row_code ?? "—", prov(v, "row_code", "sender")],
    ["Status", statusOf(v).text, prov(v, "status", "derived")],
    [
      "Admitted",
      v.sender ? `${ago(v.sender.admitted_at, now)} · ${v.sender.via_grant ? "session grant" : "PIN"}` : "—",
      prov(v, "sender", "sender"),
    ],
    ["Receives rekeys", v.status === "left" ? "no (left)" : v.sender ? (v.sender.connected ? "yes" : "no") : "—", prov(v, "sender", "sender")],
    ["Name", c?.reported.label ?? "—", "viewer-reported"],
    ["Tab", c?.reported.tab_hidden == null ? "—" : c.reported.tab_hidden ? "hidden" : "visible", "viewer-reported"],
    ["IP", c?.ip ?? (c ? "not retained" : "—"), "server-observed"],
    ["ASN", c?.geo?.asn ? `AS${c.geo.asn}${c.geo.as_org ? ` ${c.geo.as_org}` : ""}` : "—", "server-observed"],
    ["Location", place(c?.geo) || "—", "server-observed"],
    ["Device", c?.device ?? "—", "server-observed"],
    ["Path", c ? pathText(c.path) : "—", "server-observed"],
    ["RTT", st?.rtt_ms != null ? `${Math.round(st.rtt_ms)} ms` : "—", "server-observed"],
    ["Loss", st?.loss_pct != null ? `${st.loss_pct.toFixed(1)} %` : "—", "server-observed"],
    ["Bitrate", st?.bitrate_kbps != null ? `${st.bitrate_kbps} kb/s` : "—", "server-observed"],
    ["Freezes", st ? String(st.freezes) : "—", "server-observed"],
    ["Layer", st?.temporal_layer != null ? `L1T${st.temporal_layer + 1}` : "—", "server-observed"],
    ["Connected for", c ? durationShort((c.left_at ?? now) - c.joined_at) : "—", "server-observed"],
  ];
  if (v.connections.length > 1)
    rows.push(["Connections", `${v.connections.length} (newest shown)`, "server-observed"]);
  return (
    <dl className="max-w-2xl divide-y divide-paper-edge/70 text-[0.84rem]" data-testid="viewer-details">
      {rows.map(([k, val, p]) => (
        // Phones: label | value, provenance under the value. Wider: three columns.
        <div key={k} className="grid grid-cols-[6.75rem_minmax(0,1fr)] items-baseline gap-x-3 py-1 sm:grid-cols-[8.5rem_minmax(0,1fr)_9.5rem]">
          <dt className="label leading-snug">{k}</dt>
          <dd className="num min-w-0 text-[0.8rem] break-words text-ink">{val}</dd>
          <dd className="col-start-2 sm:col-start-auto sm:text-right">
            <Provenance source={p} />
          </dd>
        </div>
      ))}
    </dl>
  );
}

function RowButtons({ v, h }: { v: Viewer; h: RowHandlers }) {
  const a = rowActions(v);
  const busy = h.busy === v.row_id;
  if (h.ended || (!a.kick && !a.ban))
    return v.status === "left" ? <p className="text-[0.8rem] text-ink-mute">Left. Moves to history after 5 minutes.</p> : null;
  return (
    <div className="flex flex-wrap items-center gap-2">
      {a.kick && (
        <>
          <button type="button" className="btn btn-danger min-h-9 border-stamp/50 py-1" disabled={busy} onClick={() => h.onKick(v, false)}>
            Kick
          </button>
          <button type="button" className="btn btn-danger min-h-9 border-stamp/50 py-1" disabled={busy} onClick={() => h.onKick(v, true)}>
            Kick + lock
          </button>
        </>
      )}
      {a.ban && (
        <button type="button" className="btn btn-danger min-h-9 border-stamp/50 py-1" disabled={busy} onClick={() => h.onBan(v)}>
          Ban IP
        </button>
      )}
      {a.kick && (
        <button type="button" className="btn min-h-9 py-1" disabled title={LOCAL_ONLY} aria-describedby={`local-${v.row_id}`}>
          Kick + new PIN
        </button>
      )}
      {a.kick && (
        <span id={`local-${v.row_id}`} className="text-[0.74rem] text-ink-mute">
          New PIN: {LOCAL_ONLY}
        </span>
      )}
    </div>
  );
}

export function ViewerTable({ viewers, now, ...h }: { viewers: Viewer[]; now: number } & RowHandlers) {
  const wide = useWide();
  const [open, setOpen] = useState<Set<string>>(new Set());
  const toggle = (id: string) =>
    setOpen((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  const rows = viewers.filter((v) => v.status !== "knocking");
  if (!wide)
    return (
      <ul className="space-y-2.5" data-testid="viewer-cards">
        {rows.map((v) => {
          const c = v.connections[0];
          const isOpen = open.has(v.row_id);
          return (
            <li key={v.row_id} data-status={v.status} className={`sheet px-3.5 py-3 ${v.status === "left" ? "opacity-55" : ""}`}>
              <button
                type="button"
                className="flex w-full items-start gap-3 text-left"
                aria-expanded={isOpen}
                onClick={() => toggle(v.row_id)}
              >
                <span className="pt-1.5">
                  <Status v={v} />
                </span>
                <span className="min-w-0 flex-1 space-y-1.5">
                  <span className="flex items-center gap-2">
                    <Name v={v} />
                  </span>
                  <span className="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[0.8rem] text-ink-soft">
                    <RowCodeChip code={v.row_code} />
                    <span className="inline-flex items-center gap-1">
                      <DeviceIcon device={c?.device} className="size-4" />
                      {c?.device ?? "—"}
                    </span>
                    <Place v={v} />
                  </span>
                  <Flags v={v} />
                </span>
                <span className="flex shrink-0 flex-col items-end gap-1.5">
                  <SignalBars bars={signalBars(c?.stats)} />
                  <Watch v={v} now={now} />
                </span>
                <IconChevron className={`mt-1 size-4 shrink-0 text-ink-mute transition-transform ${isOpen ? "rotate-180" : ""}`} />
              </button>
              {isOpen && (
                <div className="mt-3 space-y-3 border-t border-paper-edge pt-3">
                  <Details v={v} now={now} />
                  <RowButtons v={v} h={h} />
                </div>
              )}
            </li>
          );
        })}
      </ul>
    );
  return (
    <div className="sheet overflow-hidden">
      <table className="w-full text-left text-[0.88rem]" data-testid="viewer-table">
        <thead className="border-b border-paper-edge bg-paper-deep/50">
          <tr>
            <th className="label w-8 py-2.5 pl-4"><span className="sr-only">Status</span></th>
            <th className="label py-2.5">Code</th>
            <th className="label py-2.5">Name</th>
            <th className="label hidden py-2.5 md:table-cell">Device</th>
            <th className="label py-2.5">Place</th>
            <th className="label py-2.5">Signal</th>
            <th className="label py-2.5">Watching</th>
            <th className="w-10 py-2.5 pr-3"><span className="sr-only">Details</span></th>
          </tr>
        </thead>
        <tbody>
          {rows.map((v) => {
            const c = v.connections[0];
            const isOpen = open.has(v.row_id);
            return (
              <Fragment key={v.row_id}>
                <tr
                  data-status={v.status}
                  className={`cursor-pointer border-b border-paper-edge last:border-0 hover:bg-paper-deep/40 ${v.status === "left" ? "opacity-55" : ""}`}
                  onClick={() => toggle(v.row_id)}
                >
                  <td className="py-3 pl-4"><Status v={v} /></td>
                  <td className="py-3"><RowCodeChip code={v.row_code} /></td>
                  <td className="max-w-[14rem] py-3">
                    <Name v={v} />
                    <Flags v={v} />
                  </td>
                  <td className="hidden py-3 md:table-cell">
                    <span className="inline-flex items-center gap-1.5 text-ink-soft">
                      <DeviceIcon device={c?.device} className="size-4 shrink-0" />
                      <span className="truncate">{c?.device ?? "—"}</span>
                    </span>
                  </td>
                  <td className="max-w-[10rem] py-3 text-ink-soft"><Place v={v} /></td>
                  <td className="py-3"><SignalBars bars={signalBars(c?.stats)} /></td>
                  <td className="py-3"><Watch v={v} now={now} /></td>
                  <td className="py-3 pr-3 text-right">
                    <button
                      type="button"
                      className="btn-quiet min-h-8 px-1.5"
                      aria-expanded={isOpen}
                      aria-label={`${isOpen ? "Hide" : "Show"} details for ${rowTitle(v)}`}
                      onClick={(e) => {
                        e.stopPropagation();
                        toggle(v.row_id);
                      }}
                    >
                      <IconChevron className={`size-4 transition-transform ${isOpen ? "rotate-180" : ""}`} />
                    </button>
                  </td>
                </tr>
                {isOpen && (
                  <tr className="border-b border-paper-edge bg-paper/60 last:border-0">
                    <td />
                    <td colSpan={7} className="py-4 pr-4">
                      <div className="grid gap-4 lg:grid-cols-[1fr_auto]">
                        <Details v={v} now={now} />
                        <div className="lg:max-w-[16rem]">
                          <RowButtons v={v} h={h} />
                        </div>
                      </div>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
