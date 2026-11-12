/**
 * Safe mode's text classifier: which characters of one recognised screen
 * line are sensitive. Pure and synchronous; it runs on OCR output, so the
 * rules tolerate common recognition slips (spaces around "@", O/0 and l/1
 * inside digit runs) and treat checksums as supporting evidence, not as the
 * only gate: one misread digit must not uncover a real card number.
 */

export type Category = "email" | "card" | "iban" | "phone" | "money" | "secret" | "id";
export interface Span {
  /** Character range [start, end) in the line's text. */
  start: number;
  end: number;
  category: Category;
}

const EMAIL = /[A-Za-z0-9._%+-]+ ?@ ?[A-Za-z0-9-]+(?:\. ?[A-Za-z0-9-]+)*\.[A-Za-z]{2,}/g;
// 13–19 digits, single space/dash/dot separators, as a whole token.
const CARD = /(?<![\w.])(?:\d[ -]?){12,18}\d(?![\w])/g;
// Digits grouped by separators (4+ groups, 12+ digits): a card or account
// number, possibly with a digit hidden behind another window.
const GROUPED = /(?<![\w.])\d{2,6}(?:[ -]\d{2,6}){3,}(?![\w])/g;
// Three groups of four (a card number being typed, before its last group).
const FOUR_GROUPS = /(?<![\w.])\d{4}([ -])\d{4}\1\d{4}(?![\w])/g;
const SSN = /(?<![\w.-])\d{3}-\d{2}-\d{4}(?![\w-])/g;
const IBAN = /\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b/g;
const MONEY =
  /[-+−–]?(?:[$€£¥₹₩₽¢₺₪฿]|\b(?:USD|EUR|GBP|CHF|JPY|CAD|AUD|NZD|SEK|NOK|DKK|PLN|INR|BRL|MXN)\s?)\s?\d[\d,.' ]*\d?(?:\s?(?:k|K|m|M|bn))?|\b\d[\d,.' ]*\d\s?(?:[€£¥₹₩₽]|(?:USD|EUR|GBP|CHF|JPY|CAD|AUD|NZD|SEK|NOK|DKK|PLN|INR|BRL|MXN)\b)/g;
// International (+), NANP "(415) 555-0132" / "415-555-0132", or a number after a phone keyword.
const PHONE_INTL = /\+\d[\d ().-]{6,18}\d/g;
const PHONE_NANP = /(?<![\w.])\(?\d{3}\)?[ .-]\d{3}[ .-]\d{4}(?![\w])/g;
const PHONE_WORD = /\b(?:phone|tel|mobile|mob|cell|call|whatsapp|fax|sms|text me)\b[^0-9+]{0,24}(\+?\d[\d ().-]{5,18}\d)/gi;
const SECRET_TOKENS = [
  /\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{8,}(?: ?[A-Za-z0-9]{3,})*/gi, // Stripe (OCR may slip a case or a space)
  /\bsk-(?:proj-|ant-)?[A-Za-z0-9_-]{16,}/g, // OpenAI / Anthropic style
  /\bgh[pousr]_[A-Za-z0-9]{20,}/g,
  /\bgithub_pat_[A-Za-z0-9_]{20,}/g,
  /\bxox[abprs]-[A-Za-z0-9-]{10,}/g,
  /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g,
  /\bAIza[0-9A-Za-z_-]{30,}/g,
  /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/g, // JWT
  /-----BEGIN [A-Z ]*PRIVATE KEY-----/g, // its body lines are OPAQUE
  /:\/\/[^\s:/@]+:[^\s@/]{3,}@/g, // URL credentials
];
// NAME=value in env/shell form: the rest of the line (OCR may split the value).
const SECRET_ENV = /\b[A-Za-z0-9_]*(?:SECRET|TOKEN|KEY|PASSWORD|PASSWD|PASS|PWD|AUTH|CREDENTIALS?)[A-Za-z0-9_]*=(\S.*)$/gi;
// KEY=value / "password": value — the value only.
const SECRET_ASSIGN = /\b[\w.-]*(?:secret|token|passw(?:or)?d|pwd|key|auth|credential)[\w.-]*["']?\s*[:=]\s*["']?([^\s"',;]{6,})/gi;

const NOT_PHONE = [
  /^\d{4}[-./]\d{1,2}[-./]\d{1,2}/, // ISO-ish date
  /^\d{1,2}[-./]\d{1,2}[-./]\d{2,4}$/, // dd/mm/yyyy
  /^\d{1,3}(?:\.\d{1,3}){3}$/, // IPv4
];

/** Digit-like OCR confusions, applied only inside runs that are mostly digits. */
function digits(s: string): string {
  return s.replace(/[Oo]/g, "0").replace(/[lI|]/g, "1").replace(/S/g, "5").replace(/B/g, "8").replace(/\D/g, "");
}

export function luhn(num: string): boolean {
  let sum = 0;
  for (let i = 0; i < num.length; i++) {
    let d = num.charCodeAt(num.length - 1 - i) - 48;
    if (i % 2 === 1) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    sum += d;
  }
  return num.length > 0 && sum % 10 === 0;
}

export function ibanValid(raw: string): boolean {
  const s = raw.replace(/ /g, "").toUpperCase();
  if (s.length < 15 || s.length > 34) return false;
  const r = s.slice(4) + s.slice(0, 4);
  let mod = 0;
  for (const ch of r) {
    const v = ch >= "A" && ch <= "Z" ? String(ch.charCodeAt(0) - 55) : ch;
    for (const c of v) mod = (mod * 10 + (c.charCodeAt(0) - 48)) % 97;
  }
  return mod === 1;
}

function cardLike(match: string): boolean {
  const d = digits(match);
  if (d.length < 13 || d.length > 19) return false;
  if (luhn(d)) return true;
  // A formatted card (4-4-4-4, 4-6-5) is hidden even if one digit was misread.
  return /^\d{4}([ -])\d{4}\1\d{4}\1\d{1,7}$/.test(match.trim()) || /^\d{4}([ -])\d{6}\1\d{5}$/.test(match.trim());
}

function push(out: Span[], start: number, end: number, category: Category) {
  if (end > start) out.push({ start, end, category });
}

/** Sensitive spans in one line, merged and sorted. */
/**
 * The text with OCR's letter-for-digit slips (O→0, I/l/|→1, S→5, B→8) undone
 * inside runs that are mostly digits; same length, so offsets carry over.
 */
export function numeric(text: string): string {
  // Per chunk: "O343" (mostly digits) is fixed, the word "or" next to a number isn't.
  return text.replace(/(?<![A-Za-z])[0-9OoIl|SB]+(?![A-Za-z])/g, (m) => {
    const d = (m.match(/\d/g) ?? []).length;
    return d >= 1 && d * 2 >= m.length ? m.replace(/[Oo]/g, "0").replace(/[Il|]/g, "1").replace(/S/g, "5").replace(/B/g, "8") : m;
  });
}

// A long token mixing upper and lower case and digits: a key, a token, a PEM
// body line (hex hashes have no upper case and stay visible).
const OPAQUE = /(?<![A-Za-z0-9+/_-])(?=[A-Za-z0-9+/_-]*[a-z])(?=[A-Za-z0-9+/_-]*[A-Z])(?=[A-Za-z0-9+/_-]*\d)[A-Za-z0-9+/_-]{32,}={0,2}(?![A-Za-z0-9+/=_-])/g;

export function findSensitive(text: string): Span[] {
  const out: Span[] = [];
  const num = numeric(text);
  for (const m of text.matchAll(OPAQUE)) push(out, m.index, m.index + m[0].length, "secret");
  for (const m of text.matchAll(EMAIL)) push(out, m.index, m.index + m[0].length, "email");
  for (const re of SECRET_TOKENS) for (const m of text.matchAll(re)) push(out, m.index, m.index + m[0].length, "secret");
  for (const m of text.matchAll(SECRET_ASSIGN)) {
    const v = m[1];
    const at = m.index + m[0].length - v.length;
    push(out, at, at + v.length, "secret");
  }
  for (const m of text.matchAll(SECRET_ENV)) {
    const at = m.index + m[0].length - m[1].length;
    push(out, at, text.length, "secret");
  }
  for (const m of text.matchAll(IBAN)) {
    const compact = m[0].replace(/ /g, "");
    if (ibanValid(compact) || (/ /.test(m[0]) && compact.length >= 15)) push(out, m.index, m.index + m[0].length, "iban");
  }
  for (const m of num.matchAll(CARD)) if (cardLike(m[0])) push(out, m.index, m.index + m[0].length, "card");
  for (const m of num.matchAll(GROUPED)) if (m[0].replace(/\D/g, "").length >= 12) push(out, m.index, m.index + m[0].length, "card");
  for (const m of num.matchAll(FOUR_GROUPS)) push(out, m.index, m.index + m[0].length, "card");
  for (const m of num.matchAll(SSN)) push(out, m.index, m.index + m[0].length, "id");
  for (const m of text.matchAll(MONEY)) {
    if (!/\d/.test(m[0])) continue;
    const s = m[0].replace(/[\s,.']+$/, "");
    push(out, m.index, m.index + s.length, "money");
  }
  const phone = (start: number, s: string) => {
    const t = s.trim();
    const n = t.replace(/\D/g, "").length;
    if (n < 7 || n > 15 || NOT_PHONE.some((re) => re.test(t))) return;
    push(out, start, start + s.length, "phone");
  };
  for (const m of num.matchAll(PHONE_INTL)) phone(m.index, m[0]);
  for (const m of num.matchAll(PHONE_NANP)) phone(m.index, m[0]);
  for (const m of text.matchAll(PHONE_WORD)) phone(m.index + m[0].length - m[1].length, m[1]);
  return merge(out);
}

function merge(spans: Span[]): Span[] {
  spans.sort((a, b) => a.start - b.start || b.end - a.end);
  const out: Span[] = [];
  for (const s of spans) {
    const last = out[out.length - 1];
    if (last && s.start <= last.end) {
      if (s.end > last.end) last.end = s.end;
    } else out.push({ ...s });
  }
  return out;
}
