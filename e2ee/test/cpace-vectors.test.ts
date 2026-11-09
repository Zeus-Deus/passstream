import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { fromHex, lvCat, prependLen, toHex, utf8 } from "../src/bytes.ts";
import {
  calculateGenerator,
  deriveIsk,
  DSI,
  generatorString,
  IDENTITY,
  isIdentity,
  oCat,
  scalarMult,
  scalarMultVfy,
  SHA512_BLOCK,
  sidOutput,
  transcriptIr,
  transcriptOc,
} from "../src/cpace.ts";
import { ristretto255 } from "@noble/curves/ed25519.js";

// Decoded from the base64 JSON blocks of draft-irtf-cfrg-cpace-21 Appendix A and B.3.
const V = JSON.parse(readFileSync(new URL("./vectors/cpace-ristretto255-sha512.json", import.meta.url), "utf8"));
const h = (s: string) => fromHex(s);
const hex = (b: Uint8Array) => toHex(b).toUpperCase();

describe("CPace draft-21 Appendix A string utilities", () => {
  it("prepend_len", () => {
    const p = V.prepend_len;
    expect(hex(prependLen(new Uint8Array(0)))).toBe(p["prepend_len(b'')"]);
    expect(hex(prependLen(utf8("1234")))).toBe(p["prepend_len(b'1234')"]);
    const r127 = Uint8Array.from({ length: 127 }, (_, i) => i);
    const r128 = Uint8Array.from({ length: 128 }, (_, i) => i);
    expect(hex(prependLen(r127))).toBe(p["prepend_len(bytes(range(127)))"]);
    expect(hex(prependLen(r128))).toBe(p["prepend_len(bytes(range(128)))"]);
  });

  it("lv_cat", () => {
    expect(hex(lvCat(utf8("1234"), utf8("5"), new Uint8Array(0), utf8("678")))).toBe(V.lv_cat["lv_cat(b'1234',b'5',b'',b'678')"]);
  });

  it("o_cat and transcripts", () => {
    expect(hex(oCat(utf8("ABCD"), utf8("BCD")))).toBe(V.o_cat["o_cat(b'ABCD',b'BCD')"]);
    expect(hex(oCat(utf8("BCD"), utf8("ABCDE")))).toBe(V.o_cat["o_cat(b'BCD',b'ABCDE')"]);
    expect(hex(transcriptIr(utf8("123"), utf8("PartyA"), utf8("234"), utf8("PartyB")))).toBe(
      V.transcript_ir["transcript_ir(b'123',b'PartyA',b'234',b'PartyB')"],
    );
    expect(hex(transcriptIr(utf8("3456"), utf8("PartyA"), utf8("2345"), utf8("PartyB")))).toBe(
      V.transcript_ir["transcript_ir(b'3456',b'PartyA',b'2345',b'PartyB')"],
    );
    expect(hex(transcriptOc(utf8("123"), utf8("PartyA"), utf8("234"), utf8("PartyB")))).toBe(
      V.transcript_oc["transcript_oc(b'123',b'PartyA',b'234',b'PartyB')"],
    );
    expect(hex(transcriptOc(utf8("3456"), utf8("PartyA"), utf8("2345"), utf8("PartyB")))).toBe(
      V.transcript_oc["transcript_oc(b'3456',b'PartyA',b'2345',b'PartyB')"],
    );
  });
});

describe("CPACE-RISTR255-SHA512 Appendix B.3", () => {
  const G = V.ristretto_generator;
  const P = V.ristretto_protocol;

  it("B.3.1 calculate_generator", () => {
    expect(hex(DSI)).toBe(G.DSI);
    const gs = generatorString(DSI, h(G.PRS), h(G.CI), h(G.sid), SHA512_BLOCK);
    expect(hex(gs)).toBe(G["generator_string(G.DSI,PRS,CI,sid,H.s_in_bytes)"]);
    const g = calculateGenerator(h(G.PRS), h(G.CI), h(G.sid));
    expect(hex(g.toBytes())).toBe(G["encoded generator g"]);
  });

  it("B.3.2–B.3.7 shares, K, ISK (IR and symmetric), sid_output", () => {
    const g = calculateGenerator(h(P.PRS), h(P.CI), h(P.sid));
    expect(hex(g.toBytes())).toBe(P.g);
    const Ya = scalarMult(h(P.ya), g);
    const Yb = scalarMult(h(P.yb), g);
    expect(hex(Ya)).toBe(P.Ya);
    expect(hex(Yb)).toBe(P.Yb);
    const Ka = scalarMultVfy(h(P.ya), Yb);
    const Kb = scalarMultVfy(h(P.yb), Ya);
    expect(hex(Ka)).toBe(P.K);
    expect(hex(Kb)).toBe(P.K);
    const tir = transcriptIr(Ya, h(P.ADa), Yb, h(P.ADb));
    expect(hex(deriveIsk(h(P.sid), Ka, tir))).toBe(P.ISK_IR);
    const toc = transcriptOc(Ya, h(P.ADa), Yb, h(P.ADb));
    expect(hex(deriveIsk(h(P.sid), Ka, toc))).toBe(P.ISK_SY);
    expect(hex(sidOutput(tir))).toBe(P.sid_output_ir);
    expect(hex(sidOutput(toc))).toBe(P.sid_output_oc);
  });

  it("B.3.10 scalar_mult with valid inputs", () => {
    const t = V.ristretto_invalid.Valid;
    const X = ristretto255.Point.fromBytes(h(t.X));
    expect(hex(scalarMult(h(t.s), X))).toBe(t["G.scalar_mult(s,decode(X))"]);
    expect(hex(scalarMultVfy(h(t.s), h(t.X)))).toBe(t["G.scalar_mult_vfy(s,X)"]);
  });

  it("B.3.11 invalid inputs map to G.I", () => {
    const s = h(V.ristretto_invalid.Valid.s);
    const i1 = scalarMultVfy(s, h(V.ristretto_invalid["Invalid Y1"]));
    const i2 = scalarMultVfy(s, h(V.ristretto_invalid["Invalid Y2"]));
    expect(i1).toEqual(IDENTITY);
    expect(i2).toEqual(IDENTITY);
    expect(isIdentity(i1) && isIdentity(i2)).toBe(true);
    // Wrong lengths are invalid encodings too.
    expect(isIdentity(scalarMultVfy(s, new Uint8Array(31)))).toBe(true);
  });
});
