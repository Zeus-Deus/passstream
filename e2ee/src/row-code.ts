import { randomBytes } from "./bytes.ts";
import { E2EEError } from "./errors.ts";

export const ROW_CODE_ALPHABET = "ACDEFHJKMNPRTUVWXY3479";
export const ROW_CODE_COUNT = 22 ** 3;
export const COLOURS = ["red", "orange", "yellow", "green", "teal", "blue", "purple", "pink"] as const;

export function rowCodeIndex(code: string): number {
  if (typeof code !== "string" || code.length !== 3) throw new E2EEError("invalid_argument", "row code must be three canonical characters");
  let index = 0;
  for (const c of code) {
    const n = ROW_CODE_ALPHABET.indexOf(c);
    if (n < 0) throw new E2EEError("invalid_argument", "noncanonical row code");
    index = index * 22 + n;
  }
  return index;
}

export function rowCodeFromIndex(index: number): string {
  if (!Number.isInteger(index) || index < 0 || index >= ROW_CODE_COUNT) throw new E2EEError("malformed", "row code index out of range");
  return ROW_CODE_ALPHABET[Math.floor(index / 484)]! + ROW_CODE_ALPHABET[Math.floor(index / 22) % 22]! + ROW_CODE_ALPHABET[index % 22]!;
}

export function rowCodeColour(code: string): (typeof COLOURS)[number] {
  return COLOURS[rowCodeIndex(code) % COLOURS.length]!;
}

/** Rejection sampling is uniform among unused codes, including near exhaustion. */
export function assignRowCode(used: ReadonlySet<string>): string {
  if (used.size >= ROW_CODE_COUNT) throw new E2EEError("state", "row code space exhausted");
  const limit = Math.floor(65536 / ROW_CODE_COUNT) * ROW_CODE_COUNT;
  for (;;) {
    const b = randomBytes(2);
    const n = b[0]! * 256 + b[1]!;
    if (n >= limit) continue;
    const code = rowCodeFromIndex(n % ROW_CODE_COUNT);
    if (!used.has(code)) return code;
  }
}
