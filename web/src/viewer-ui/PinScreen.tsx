import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Brand } from "./parts";

export const NAME_KEY = "passstream.viewer-name";

export type PinPolicy = { kind: "digits"; length: number } | { kind: "passphrase" };

export interface PinScreenProps {
  policy: PinPolicy;
  onSubmit: (secret: string, name: string) => void;
  /** The check is running on the host's machine. */
  busy?: boolean;
  /** Shown after a failed try, e.g. "That PIN didn't match." */
  error?: string | null;
  triesLeft?: number | null;
  /** Epoch ms until which the sender refuses tries. */
  lockedUntil?: number | null;
  streamLabel?: string | null;
  privacyHref?: string;
  now?: () => number;
  storage?: NameStore | null;
}

function safeStorage(): NameStore | null {
  try {
    return window.localStorage;
  } catch {
    return null; // private mode or blocked storage: the name just isn't remembered
  }
}

type NameStore = Pick<Storage, "getItem" | "setItem" | "removeItem">;

// Remembering the name is a convenience: a throwing store (quota, blocked
// storage, Safari private mode) never blocks joining.
function readName(storage: NameStore | null): string {
  try {
    return storage?.getItem(NAME_KEY) ?? "";
  } catch {
    return "";
  }
}
function rememberName(storage: NameStore | null, name: string) {
  try {
    if (name) storage?.setItem(NAME_KEY, name);
    else storage?.removeItem(NAME_KEY);
  } catch {
    /* not remembered; joining continues */
  }
}

/** Passphrases are NFC-normalized and 8–256 UTF-8 bytes (CONTRACT §4.1). */
export const PASSPHRASE_MIN_BYTES = 8;
export const PASSPHRASE_MAX_BYTES = 256;
export function passphraseBytes(v: string): number {
  return new TextEncoder().encode(v.normalize("NFC")).length;
}
export function validPassphrase(v: string): boolean {
  const n = passphraseBytes(v);
  return n >= PASSPHRASE_MIN_BYTES && n <= PASSPHRASE_MAX_BYTES;
}

function countdown(ms: number) {
  const s = Math.ceil(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/**
 * The join screen. Digits use one real input (so iOS/Android one-time-code
 * autofill and paste work) drawn as N boxes; it submits itself on the last
 * digit. The PIN is checked by the host's machine, never by the server.
 */
export function PinScreen({
  policy,
  onSubmit,
  busy = false,
  error = null,
  triesLeft = null,
  lockedUntil = null,
  streamLabel = null,
  privacyHref = "/privacy",
  now = Date.now,
  storage = safeStorage(),
}: PinScreenProps) {
  const digits = policy.kind === "digits";
  const length = digits ? Math.min(12, Math.max(4, policy.length)) : 0;
  const [value, setValue] = useState("");
  const [name, setName] = useState(() => readName(storage));
  const [focused, setFocused] = useState(false);
  const [reveal, setReveal] = useState(false);
  const [clock, setClock] = useState(now);
  const submitted = useRef<string | null>(null);
  const input = useRef<HTMLInputElement>(null);
  const ids = useId();
  const remaining = lockedUntil ? Math.max(0, lockedUntil - clock) : 0;
  const locked = remaining > 0;
  // The lockout's length as first seen, for a stable screen-reader sentence.
  const [lockMs, setLockMs] = useState(0);
  useEffect(() => {
    if (lockedUntil) setLockMs(Math.max(0, lockedUntil - now()));
  }, [lockedUntil, now]);

  useEffect(() => {
    if (!lockedUntil) return;
    setClock(now());
    const t = setInterval(() => setClock(now()), 250);
    return () => clearInterval(t);
  }, [lockedUntil, now]);

  // A finished, failed try clears the boxes for the next one.
  useEffect(() => {
    if (!busy && error) {
      setValue("");
      submitted.current = null;
      input.current?.focus();
    }
  }, [busy, error]);
  useEffect(() => {
    if (!locked && lockedUntil) input.current?.focus();
  }, [locked, lockedUntil]);

  const ready = (v: string) => (digits ? v.length === length : validPassphrase(v));
  const submit = (raw: string) => {
    const v = digits ? raw : raw.normalize("NFC");
    if (busy || locked || submitted.current === v || !ready(v)) return;
    submitted.current = v;
    const n = name.trim().slice(0, 64);
    rememberName(storage, n);
    onSubmit(v, n);
  };

  const change = (raw: string) => {
    if (!digits) {
      setValue(raw);
      return;
    }
    // Paste and autofill can bring spaces, dashes or a longer string.
    const v = raw.replace(/\D/g, "").slice(0, length);
    setValue(v);
    if (v !== submitted.current) submitted.current = null;
    if (v.length === length) submit(v);
  };

  const onForm = (e: FormEvent) => {
    e.preventDefault();
    submit(value);
  };

  const tooLong = !digits && passphraseBytes(value) > PASSPHRASE_MAX_BYTES;
  const hint = locked
    ? null
    : busy
      ? "Checking with the host…"
      : tooLong
        ? `Passphrases are at most ${PASSPHRASE_MAX_BYTES} bytes.`
      : triesLeft !== null && triesLeft <= 3
        ? `${triesLeft} ${triesLeft === 1 ? "try" : "tries"} left before a pause.`
        : null;

  return (
    <main className="flex min-h-dvh flex-col items-center px-4 pt-[max(1.25rem,env(safe-area-inset-top))] pb-[max(2rem,env(safe-area-inset-bottom))] sm:px-5">
      <div className="flex w-full max-w-md items-center justify-between pb-6">
        <Brand />
        <span className="rounded-sm border border-paper-edge px-2 py-1 font-mono text-[0.64rem] tracking-[0.14em] text-ink-mute uppercase">Private stream</span>
      </div>
      <form onSubmit={onForm} className="sheet w-full max-w-md animate-rise p-5 sm:p-8" aria-describedby={`${ids}-hint`}>
        <p className="label !text-stamp">{streamLabel ? `Joining · ${streamLabel}` : "Joining a live screen"}</p>
        <h1 className="display mt-2 text-[1.9rem] leading-[1.08] sm:text-[2.3rem]">
          {digits ? "Enter the PIN." : "Enter the passphrase."}
        </h1>
        <p className="mt-2 text-[0.92rem] text-ink-soft">
          The host sent it to you separately from the link. Their computer checks it; the server never sees it.
        </p>

        <label htmlFor={`${ids}-pin`} className="sr-only">
          {digits ? `${length}-digit PIN` : "Passphrase"}
        </label>
        {digits ? (
          <div className="relative mt-6" data-testid="pin-boxes">
            <div
              aria-hidden="true"
              // Long PINs wrap into rows of six, so each box stays tappable-sized at 320 px.
              className={length > 8 ? "grid grid-cols-6 gap-2" : "flex justify-between gap-[clamp(0.25rem,2vw,0.6rem)]"}
            >
              {Array.from({ length }, (_, i) => {
                const active = focused && !locked && (i === value.length || (i === length - 1 && value.length === length));
                return (
                  <span
                    key={i}
                    className={`num grid aspect-[4/5] min-w-0 flex-1 place-items-center rounded-md border-[1.5px] bg-paper text-[clamp(1.2rem,6vw,1.9rem)] text-ink transition-colors ${
                      error && !value ? "border-stamp" : active ? "border-ink shadow-[0_0_0_3px_var(--color-stamp-tint)]" : value[i] ? "border-ink-soft" : "border-paper-dark"
                    } ${locked ? "opacity-50" : ""} ${length > 8 ? "!text-[clamp(1.1rem,5vw,1.5rem)]" : ""}`}
                  >
                    {value[i] ?? ""}
                  </span>
                );
              })}
            </div>
            <input
              ref={input}
              id={`${ids}-pin`}
              data-testid="pin-input"
              className="absolute inset-0 h-full w-full cursor-text border-0 bg-transparent text-transparent caret-transparent opacity-[0.01] outline-none selection:bg-transparent"
              type="text"
              inputMode="numeric"
              pattern="[0-9]*"
              autoComplete="one-time-code"
              enterKeyHint="go"
              maxLength={length + 8 /* room for pasted separators; filtered */}
              value={value}
              disabled={locked}
              autoFocus
              aria-invalid={!!error}
              aria-describedby={`${ids}-hint ${ids}-err`}
              onFocus={() => setFocused(true)}
              onBlur={() => setFocused(false)}
              onChange={(e) => change(e.target.value)}
            />
          </div>
        ) : (
          <div className="relative mt-6">
          <input
            ref={input}
            id={`${ids}-pin`}
            data-testid="pin-input"
            className="field num min-h-12 pr-16 text-[1.05rem]"
            type={reveal ? "text" : "password"}
            autoComplete="current-password"
            enterKeyHint="go"
            spellCheck={false}
            value={value}
            disabled={locked}
            autoFocus
            aria-invalid={!!error}
            aria-describedby={`${ids}-hint ${ids}-err`}
            onChange={(e) => change(e.target.value)}
          />
          <button
            type="button"
            className="btn-quiet absolute inset-y-0 right-1 my-auto h-11"
            onClick={() => setReveal((r) => !r)}
            aria-pressed={reveal}
            aria-controls={`${ids}-pin`}
            data-testid="pin-reveal"
          >
            {reveal ? "Hide" : "Show"}
          </button>
          </div>
        )}

        <div className="mt-3 min-h-[1.4rem] text-[0.86rem]" aria-live="polite">
          {error && !locked && (
            <p id={`${ids}-err`} role="alert" className="font-medium text-stamp">{error}</p>
          )}
          {locked && (
            // The countdown ticks visually; screen readers hear the lockout once
            // (the live region's text only changes when it starts or ends).
            <p id={`${ids}-hint`} className="font-medium text-stamp" data-testid="pin-hint">
              <span className="sr-only">Too many wrong tries. The host's computer is pausing PIN checks for {Math.max(1, Math.round(lockMs / 60_000))} {Math.round(lockMs / 60_000) > 1 ? "minutes" : "minute"}.</span>
              <span aria-hidden="true">
                Too many tries. Try again in <span className="num" data-testid="pin-countdown">{countdown(remaining)}</span>.
              </span>
            </p>
          )}
          {hint && (
            <p id={`${ids}-hint`} className="text-ink-mute" data-testid="pin-hint">{hint}</p>
          )}
        </div>

        <label className="mt-4 block">
          <span className="label mb-1.5 block">Your name <span className="tracking-normal normal-case">(optional)</span></span>
          <input
            className="field min-h-11"
            value={name}
            maxLength={64}
            autoComplete="nickname"
            enterKeyHint="go"
            placeholder="e.g. Ana"
            onChange={(e) => setName(e.target.value)}
            data-testid="name-input"
          />
          <span className="mt-1 block text-[0.76rem] text-ink-mute">Remembered on this device. The host sees it labelled as reported by you.</span>
        </label>

        <button className="btn btn-primary mt-5 min-h-12 w-full" disabled={busy || locked || !ready(value)}>
          {busy ? "Checking…" : "Join"}
        </button>
        <p className="mt-5 border-t border-paper-edge pt-4 text-[0.8rem] text-ink-mute">
          The host sees your name, device, approximate place and connection quality while you watch.{" "}
          <a className="font-medium text-ink underline underline-offset-2" href={privacyHref}>Who sees what, and for how long</a>.
        </p>
      </form>
    </main>
  );
}
