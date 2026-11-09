import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { fromHex, toHex } from "../src/bytes.ts";
import { type CipherSuiteId, decodeHeader, deriveKeySalt, encodeHeader, makeAead, makeNonce, SFrameKey } from "../src/sframe.ts";

// Parsed from RFC 9605 Appendix C (all 289 header cases, C.2 and C.3).
const V = JSON.parse(readFileSync(new URL("./vectors/rfc9605.json", import.meta.url), "utf8"));

describe("RFC 9605 Appendix C.1 header encoding", () => {
  it(`round-trips all ${V.header.length} cases`, () => {
    expect(V.header.length).toBe(289);
    for (const t of V.header) {
      const kid = BigInt(t.kid);
      const ctr = BigInt(t.ctr);
      expect(toHex(encodeHeader(kid, ctr))).toBe(t.header);
      const d = decodeHeader(fromHex(t.header));
      expect(d.kid).toBe(kid);
      expect(d.ctr).toBe(ctr);
      expect(toHex(d.bytes)).toBe(t.header);
    }
  });
});

describe("RFC 9605 Appendix C.2 AES-CTR + HMAC AEAD", () => {
  for (const t of V.aead_ctr_hmac) {
    it(`suite ${t.cipher_suite}`, async () => {
      const aead = await makeAead(Number(t.cipher_suite) as CipherSuiteId, fromHex(t.key));
      const ct = await aead.encrypt(fromHex(t.nonce), fromHex(t.aad), fromHex(t.pt));
      expect(toHex(ct)).toBe(t.ct);
      expect(toHex(await aead.decrypt(fromHex(t.nonce), fromHex(t.aad), fromHex(t.ct)))).toBe(t.pt);
    });
  }
});

describe("RFC 9605 Appendix C.3 SFrame encryption", () => {
  for (const t of V.sframe) {
    it(`suite ${t.cipher_suite}`, async () => {
      const suite = Number(t.cipher_suite) as CipherSuiteId;
      const kid = BigInt(t.kid);
      const ctr = BigInt(t.ctr);
      const d = deriveKeySalt(suite, kid, fromHex(t.base_key));
      expect(toHex(d.keyLabel)).toBe(t.sframe_key_label);
      expect(toHex(d.saltLabel)).toBe(t.sframe_salt_label);
      expect(toHex(d.secret)).toBe(t.sframe_secret);
      expect(toHex(d.key)).toBe(t.sframe_key);
      expect(toHex(d.salt)).toBe(t.sframe_salt);
      expect(toHex(makeNonce(d.salt, ctr))).toBe(t.nonce);

      const enc = await SFrameKey.create({ kid, baseKey: fromHex(t.base_key), direction: "encrypt", suite });
      const ct = await enc.encryptWithCtr(ctr, fromHex(t.pt), fromHex(t.metadata));
      expect(toHex(ct)).toBe(t.ct);
      const dec = await SFrameKey.create({ kid, baseKey: fromHex(t.base_key), direction: "decrypt", suite });
      expect(toHex(await dec.decrypt(fromHex(t.ct), fromHex(t.metadata)))).toBe(t.pt);
    });
  }
});
