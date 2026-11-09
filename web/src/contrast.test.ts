// WCAG 2.x contrast for every text colour the dashboard and viewer UI use,
// against every background it's drawn on. Normal-size text needs 4.5:1.
import { describe, expect, it } from "vitest";
import css from "./assets/theme.css?raw";

const SOURCES = import.meta.glob<string>(["./dashboard/*.tsx", "./viewer-ui/*.tsx", "!**/*.test.tsx"], { query: "?raw", import: "default", eager: true });
const TOKENS = Object.fromEntries([...css.matchAll(/--color-([a-z-]+):\s*(#[0-9a-f]{6});/g)].map((m) => [m[1], m[2]]));

type RGB = [number, number, number];
const rgb = (hex: string): RGB => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as RGB;
/** "ink", "paper/60" (alpha over the background) or "#rrggbb". */
function colour(spec: string, under?: RGB): RGB {
  const [name, alpha] = spec.split("/");
  const hex = name.startsWith("#") ? name : TOKENS[name];
  if (!hex) throw new Error(`unknown colour ${spec}`);
  const c = rgb(hex);
  if (!alpha) return c;
  if (!under) throw new Error(`${spec} needs a background`);
  const a = Number(alpha) / 100;
  return c.map((v, i) => Math.round(a * v + (1 - a) * under[i])) as RGB;
}
function luminance([r, g, b]: RGB) {
  const f = (v: number) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
function contrast(fg: string, bg: string, base = "paper") {
  const b = colour(bg, colour(base));
  const [x, y] = [luminance(colour(fg, b)), luminance(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
}

const LIGHT = ["paper", "paper-sheet", "paper-deep"];
// [text, background]; translucent backgrounds sit on paper, translucent text
// on its background.
const PAIRS: [string, string][] = [
  ...["ink", "ink-soft", "ink-mute", "stamp", "stamp-deep", "ok", "warn"].flatMap((fg) => LIGHT.map((bg) => [fg, bg] as [string, string])),
  // Badges and notices (dashboard/ui.tsx).
  ["ok", "ok-tint"],
  ["warn", "warn-tint"],
  ["stamp", "stamp-tint"],
  ["ok", "ok-tint/60"],
  ["#6f4512", "warn-tint/70"],
  ["stamp-deep", "stamp-tint/70"],
  ["ink-soft", "paper-deep/70"],
  // Filled buttons and badges.
  ["paper", "ink"],
  ["paper", "ink-soft"],
  ["paper", "stamp"],
  ["paper", "stamp-deep"],
  // The viewer's night stage (state cards, framed header, glass over the letterbox).
  ...["paper", "paper/55", "paper/60", "paper/70", "paper/75", "warn-tint", "#e8735c"].map((fg) => [fg, "night"] as [string, string]),
  ["paper", "night-soft"],
];

describe("theme text contrast", () => {
  it.each(PAIRS)("%s on %s is at least 4.5:1", (fg, bg) => {
    expect(contrast(fg, bg)).toBeGreaterThanOrEqual(4.5);
  });

  it("warning text on paper clears 4.5:1 (it was 3.95 on paper-sheet)", () => {
    expect(contrast("warn", "paper-sheet")).toBeGreaterThanOrEqual(4.5);
  });

  // Every text colour class in the UI must be one of the pairs above, so a new
  // colour can't skip the check. ink-faint is for disabled controls only.
  it("covers every text colour used by the dashboard and viewer UI", () => {
    const files = [...Object.values(SOURCES), css];
    expect(files.length).toBeGreaterThan(15);
    const known = new Set(PAIRS.map(([fg]) => fg));
    const used = new Set<string>();
    for (const src of files)
      for (const m of src.matchAll(/(^|[\s"'`:!])((?:[a-z-]+:)*)text-(\[#[0-9a-f]{6}\]|[a-z]+(?:-[a-z]+)*(?:\/\d+)?)(?=[\s"'`;]|$)/gm)) {
        const variants = m[2];
        const spec = m[3].startsWith("[") ? m[3].slice(1, -1) : m[3];
        if (!spec.startsWith("#") && !(spec.split("/")[0] in TOKENS)) continue; // sizes, alignment
        if (spec === "ink-faint") {
          expect(variants, "ink-faint outside a disabled state").toBe("disabled:");
          continue;
        }
        used.add(spec);
      }
    expect([...used].filter((u) => !known.has(u))).toEqual([]);
  });
});
