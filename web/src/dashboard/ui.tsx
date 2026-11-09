import type { ReactNode, SVGProps } from "react";
import { ROW_COLOUR_HEX, rowCodeColour } from "../viewer-ui/rowcode";
import type { Tone } from "./format";

type P = SVGProps<SVGSVGElement>;
const base = {
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.6,
  strokeLinecap: "round",
  strokeLinejoin: "round",
} as const;

/** The PassStream mark: the family's stamp with a live screen in it. */
export function Mark(p: P) {
  return (
    <svg viewBox="0 0 64 64" aria-hidden="true" {...p}>
      <path fill="#c1422b" d="M10 4h44a6 6 0 0 0 6 6v44a6 6 0 0 0-6 6H10a6 6 0 0 0-6-6V10a6 6 0 0 0 6-6z" />
      <rect x="15" y="18" width="34" height="23" rx="2" fill="#f4eee3" />
      <path fill="#dcd2bb" d="M26 46h12v3H26z" />
      <circle cx="23" cy="25" r="3" fill="#c1422b" />
      <path fill="#c1422b" d="M29 31h14v2.5H29zm-8 5h22v2.5H21z" />
    </svg>
  );
}

export const IconStreams = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <rect x="3" y="5" width="18" height="12" rx="1.5" />
    <path d="M9 21h6M12 17v4" />
    <circle cx="8" cy="9.5" r="1.2" fill="currentColor" />
  </svg>
);
export const IconHistory = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <path d="M4 12a8 8 0 1 0 2.4-5.7L4 8.5" />
    <path d="M4 4v4.5h4.5M12 8v4.5l3 2" />
  </svg>
);
export const IconSettings = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <path d="M4 7h10M18 7h2M4 17h4M12 17h8" />
    <circle cx="16" cy="7" r="2" />
    <circle cx="10" cy="17" r="2" />
  </svg>
);
export const IconLock = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <rect x="5" y="11" width="14" height="9" rx="1.5" />
    <path d="M8 11V8a4 4 0 0 1 8 0v3" />
  </svg>
);
export const IconEyeOff = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <path d="M3 3l18 18M10.6 6.1A9.8 9.8 0 0 1 12 6c5 0 8.5 4.5 9.5 6-.5.8-1.6 2.2-3.1 3.5M6.6 7.6C4.6 9 3.1 11 2.5 12c1 1.5 4.5 6 9.5 6 1.6 0 3-.4 4.3-1.1" />
    <path d="M9.9 10a3 3 0 0 0 4.1 4.1" />
  </svg>
);
export const IconChevron = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <path d="m6 9 6 6 6-6" />
  </svg>
);
export const IconPhone = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <rect x="7" y="3" width="10" height="18" rx="2" />
    <path d="M11 18h2" />
  </svg>
);
export const IconMonitor = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <rect x="3" y="4" width="18" height="12" rx="1.5" />
    <path d="M8 20h8M12 16v4" />
  </svg>
);
export const IconTablet = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <rect x="5" y="3" width="14" height="18" rx="2" />
    <path d="M11 18h2" />
  </svg>
);
export const IconWarn = (p: P) => (
  <svg viewBox="0 0 24 24" aria-hidden="true" {...base} {...p}>
    <path d="M12 3 2 20h20zM12 10v4M12 17h.01" />
  </svg>
);

export function DeviceIcon({ device, className }: { device: string | null | undefined; className?: string }) {
  const d = device ?? "";
  const Icon = /ipad|tablet/i.test(d) ? IconTablet : /iphone|android|mobile|phone/i.test(d) ? IconPhone : IconMonitor;
  return <Icon className={className} />;
}

const tones: Record<Tone, string> = {
  ok: "bg-ok-tint text-ok",
  warn: "bg-warn-tint text-warn",
  stamp: "bg-stamp-tint text-stamp",
  mute: "bg-paper-deep text-ink-mute",
  ink: "bg-ink text-paper",
};

export function Badge({ tone = "mute", children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded-sm px-1.5 py-0.5 font-mono text-[0.68rem] font-medium tracking-wide whitespace-nowrap ${tones[tone]}`}
    >
      {children}
    </span>
  );
}

/** "K7P · purple" with its colour dot. The colour is never the only cue. */
export function RowCodeChip({ code, size = "sm" }: { code: string | null; size?: "sm" | "lg" }) {
  if (!code) return <span className="label">no code</span>;
  const colour = rowCodeColour(code);
  return (
    <span
      data-testid="row-code"
      className={`inline-flex items-center gap-1.5 rounded-full border border-paper-dark bg-paper font-mono font-medium whitespace-nowrap text-ink ${size === "lg" ? "px-3 py-1 text-base" : "px-2 py-0.5 text-[0.72rem]"}`}
    >
      <span
        aria-hidden="true"
        className={`${size === "lg" ? "size-3" : "size-2"} rounded-full`}
        style={{ background: colour ? ROW_COLOUR_HEX[colour] : "transparent" }}
      />
      {code}
      {colour && <span className="text-ink-mute">· {colour}</span>}
    </span>
  );
}

export function SignalBars({ bars }: { bars: number }) {
  const label = bars < 0 ? "No signal data" : `Signal ${bars} of 4`;
  return (
    <span role="img" aria-label={label} title={label} className="inline-flex h-3.5 items-end gap-[2px]">
      {[1, 2, 3, 4].map((i) => (
        <span
          key={i}
          className={`w-[3px] rounded-[1px] ${bars < 0 ? "bg-paper-dark" : i <= bars ? (bars <= 1 ? "bg-stamp" : bars === 2 ? "bg-warn" : "bg-ink-soft") : "bg-paper-dark"}`}
          style={{ height: `${i * 25}%` }}
        />
      ))}
    </span>
  );
}

export function PageHead({ kicker, title, children, aside }: { kicker: string; title: string; children?: ReactNode; aside?: ReactNode }) {
  return (
    <header className="mb-7 flex flex-wrap items-end justify-between gap-4 md:mb-9">
      <div className="min-w-0">
        <p className="label !text-stamp">{kicker}</p>
        <h1 className="display mt-2 text-[2.1rem] leading-[1.04] break-words md:text-[2.9rem]">{title}</h1>
        {children && <div className="mt-3 max-w-2xl text-[0.98rem] text-ink-soft">{children}</div>}
      </div>
      {aside}
    </header>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="border-y border-dashed border-paper-dark py-10 text-center text-ink-mute">{children}</p>;
}

export function Loading() {
  return (
    <div className="space-y-3 py-4" aria-label="Loading" role="status">
      {[0, 1, 2].map((i) => (
        <div key={i} className="h-14 animate-pulse rounded-sm bg-paper-deep" />
      ))}
    </div>
  );
}

export function Notice({ tone = "mute", children, role }: { tone?: Tone; children: ReactNode; role?: string }) {
  const t = {
    ok: "border-ok bg-ok-tint/60 text-ok",
    warn: "border-warn bg-warn-tint/70 text-[#6f4512]",
    stamp: "border-stamp bg-stamp-tint/70 text-stamp-deep",
    mute: "border-paper-dark bg-paper-deep/70 text-ink-soft",
    ink: "border-ink bg-ink text-paper",
  }[tone];
  return (
    <div role={role} className={`rounded-sm border-l-[3px] px-3.5 py-2.5 text-[0.86rem] leading-snug ${t}`}>
      {children}
    </div>
  );
}

/** The three provenance classes (HOST-CONTROL §3.3). */
export function Provenance({ source }: { source: string }) {
  if (source === "sender")
    return (
      <span className="inline-flex items-center gap-1 text-[0.72rem] whitespace-nowrap text-ink-soft" title="Reported by the sender, via the server">
        <IconLock className="size-3" />
        sender, via server
      </span>
    );
  if (source === "viewer-reported")
    return <span className="text-[0.72rem] whitespace-nowrap text-warn italic">viewer-reported</span>;
  if (source === "derived")
    return <span className="text-[0.72rem] whitespace-nowrap text-ink-mute">derived</span>;
  return <span className="text-[0.72rem] whitespace-nowrap text-ink-mute">server-observed</span>;
}
