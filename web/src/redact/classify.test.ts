import { describe, expect, it } from "vitest";
import { findSensitive, ibanValid, luhn } from "./classify";

const hidden = (text: string) => findSensitive(text).map((s) => [text.slice(s.start, s.end), s.category]);

describe("safe mode classifier", () => {
  it("finds emails, tolerating OCR spaces", () => {
    expect(hidden("From: Ana <ana.ruiz@northwind-labs.com> · To: me")).toEqual([["ana.ruiz@northwind-labs.com", "email"]]);
    expect(hidden("billing @acme-payments.io")).toEqual([["billing @acme-payments.io", "email"]]);
  });

  it("finds Luhn-valid and formatted card numbers", () => {
    expect(luhn("4539148803436467")).toBe(true);
    expect(hidden("4539 1488 0343 6467")).toEqual([["4539 1488 0343 6467", "card"]]);
    expect(hidden("card 4539148803436467 ok")).toEqual([["4539148803436467", "card"]]);
    // One misread digit (Luhn fails) is still hidden when formatted like a card.
    expect(hidden("4539 1488 0343 6468")).toEqual([["4539 1488 0343 6468", "card"]]);
    // Partly hidden behind another window: still grouped like a card.
    expect(hidden("539 1488 0343 6467")).toEqual([["539 1488 0343 6467", "card"]]);
    expect(hidden("SSN 078-05-1120")).toEqual([["078-05-1120", "id"]]);
    // Being typed: hidden before the last group.
    expect(hidden("pay 4111 1111 1111")).toEqual([["4111 1111 1111", "card"]]);
    // Letters OCR reads for digits.
    expect(hidden("4539 1488 O343 6467")).toEqual([["4539 1488 O343 6467", "card"]]);
    expect(hidden("4539 I488 0343 6467")).toEqual([["4539 I488 0343 6467", "card"]]);
    // A long unformatted non-Luhn number is not a card.
    expect(hidden("build 4539148803436468")).toEqual([]);
  });

  it("finds IBANs", () => {
    expect(ibanValid("DE89370400440532013000")).toBe(true);
    expect(hidden("IBAN DE89 3704 0044 0532 0130 00 or")).toEqual([["DE89 3704 0044 0532 0130 00", "iban"]]);
  });

  it("finds phones without matching dates, versions or IPs", () => {
    expect(hidden("call me at +1 (415) 555-0132 or")).toEqual([["+1 (415) 555-0132", "phone"]]);
    expect(hidden("office 415-555-0132")).toEqual([["415-555-0132", "phone"]]);
    expect(hidden("my mobile 07700 900461.")).toEqual([["07700 900461", "phone"]]);
    expect(hidden("received on 2026-10-03, version 3.4.1, host 10.0.12.4, PO-77812")).toEqual([]);
    expect(hidden("14:02:11 up 12 days, ports 8080, 8443, 9090")).toEqual([]);
    expect(hidden("Build 2026.11.12 passed at 14:30")).toEqual([]);
  });

  it("finds money only next to a currency", () => {
    expect(hidden("comes to $12,480.50, and")).toEqual([["$12,480.50", "money"]]);
    expect(hidden("deposit of €3.200,00 was")).toEqual([["€3.200,00", "money"]]);
    expect(hidden("-£4.20")).toEqual([["-£4.20", "money"]]);
    expect(hidden("+$6,250.00")).toEqual([["+$6,250.00", "money"]]);
    expect(hidden("1,250 EUR fee")).toEqual([["1,250 EUR", "money"]]);
    expect(hidden("load average: 0.42, 0.38, 0.31")).toEqual([]);
  });

  it("finds secrets and assigned credentials", () => {
    expect(hidden("STRIPE_KEY=sk_" + "live_51Hx9kQ2eZvKYlo2C8nT4rWm")).toEqual([["sk_" + "live_51Hx9kQ2eZvKYlo2C8nT4rWm", "secret"]]);
    expect(hidden("export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123")).toEqual([["ghp_abcdefghijklmnopqrstuvwxyz0123", "secret"]]);
    expect(hidden('"password": "hunter2!!"')).toEqual([["hunter2!!", "secret"]]);
    expect(hidden("postgres://admin:s3cr3tpw@db.internal:5432/prod")).toEqual([["://admin:s3cr3tpw@db.internal", "secret"]]);
    expect(hidden("STRIPE_KEY=sk_liVe_51Hx9kQ2eZvRYlo2l C8nT4rWm")).toEqual([["sk_liVe_51Hx9kQ2eZvRYlo2l C8nT4rWm", "secret"]]);
    expect(hidden("export DB_PASSWORD=correct horse")).toEqual([["correct horse", "secret"]]);
    expect(hidden("a3f9c21 fix: retry on 503 (#1842)")).toEqual([]);
    // PEM body lines and bare tokens; hex hashes stay visible.
    expect(hidden("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7")[0][1]).toBe("secret");
    expect(hidden("commit 9be0d44e8b1f0c2a7d3e5f6a1b2c3d4e5f6a7b8c")).toEqual([]);
  });
});

import { classifyLine, segments, type Glyph } from "./ocr";

describe("long lines", () => {
  it("stitches a hard-cut token without inserting a space", () => {
    // A 14 px high, 900 px wide line with ink everywhere: no word gap to cut at.
    const w = 900, h = 14;
    const data = new Uint8Array(w * h).fill(255);
    for (let x = 0; x < w; x += 2) for (let y = 3; y < 11; y++) data[y * w + x] = 0;
    const segs = segments(data, w, h);
    expect(segs.length).toBeGreaterThan(1);
    expect(segs[1].join).toBeDefined();
    expect(segs[1].x0).toBeLessThan(segs[0].x1); // overlap
    // Fake readings: the overlap region is read by both segments.
    const key = "sk_" + "live_51Hx9kQ2eZvKYlo2C8nT4rWmAbCdEfGhIjKlMnOpQrStUvWxYz0123456789";
    const pitch = w / key.length;
    const glyphs: Glyph[][] = segs.map((s) =>
      [...key].map((c, i) => ({ c, x: (i + 0.5) * pitch, p: 0.99 })).filter((g) => g.x >= s.x0 && g.x < s.x1),
    );
    const r = classifyLine(glyphs, segs, w, h, true);
    expect(r.text).toBe(key);
    expect(r.covers).toHaveLength(1);
    expect(r.covers[0].x0).toBeLessThan(pitch);
    expect(r.covers[0].x1).toBeGreaterThan(w - pitch);
  });
});
