// Row codes (CONTRACT §4.3, Appendix A #12). The colour is derived from the
// code and is display-only: it's always shown with its name, never compared.
export const ROW_ALPHABET = "ACDEFHJKMNPRTUVWXY3479";
export const ROW_COLOURS = [
  "red",
  "orange",
  "yellow",
  "green",
  "teal",
  "blue",
  "purple",
  "pink",
] as const;
export type RowColour = (typeof ROW_COLOURS)[number];
export const ROW_COLOUR_HEX: Record<RowColour, string> = {
  red: "#e5484d",
  orange: "#f76b15",
  yellow: "#ffc53d",
  green: "#30a46c",
  teal: "#12a594",
  blue: "#0090ff",
  purple: "#8e4ec6",
  pink: "#d6409f",
};

export function isRowCode(code: unknown): code is string {
  return (
    typeof code === "string" &&
    code.length === 3 &&
    [...code].every((c) => ROW_ALPHABET.includes(c))
  );
}

export function rowCodeIndex(code: string): number {
  const i = [...code].map((c) => ROW_ALPHABET.indexOf(c));
  return 484 * i[0] + 22 * i[1] + i[2];
}

export function rowCodeColour(code: string): RowColour | null {
  return isRowCode(code) ? ROW_COLOURS[rowCodeIndex(code) % 8] : null;
}
