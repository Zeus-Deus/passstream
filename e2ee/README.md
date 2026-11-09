# @passstream/e2ee

PassStream's end-to-end encryption core, in pure TypeScript. It runs on the browser main thread, inside an `RTCRtpScriptTransform` worker, and in Node for tests.

- **Admission:** CPace (draft-irtf-cfrg-cpace-21, CPACE-RISTR255-SHA512) with the mandatory 256-bit link secret from the URL fragment plus the PIN as the password. The sender signs its challenge with the Ed25519 key pinned in the link. Then the viewer gets an AEAD-wrapped epoch key and a session grant. Owner actions (kick, kick + rotate PIN, unlock) address sender-assigned admission ids.
- **Guess budget on the sender:** persistent and atomic. 5 failures per viewer session trigger a 60 s backoff that doubles each time. 20 failures per stream freeze new admissions.
- **Media:** SFrame (RFC 9605, AES_128_GCM_SHA256_128) with per-track keys per epoch, replay windows and codec-aware clear prefixes. Video codecs under E2EE: VP9, then VP8; H.264 and AV1 are refused.
- **Epochs:** rekey on kick, on PIN rotation and every 10 min, with acks and a grace period.
- **Worker glue** (`src/transform-worker.ts`) and feature detection that fail closed.

Runtime dependencies are only `@noble/curves` and `@noble/hashes`. Everything else uses WebCrypto.

The spec, threat model, test inventory and measured throughput are in [docs/PROTOCOL.md](../docs/PROTOCOL.md).

```sh
mise exec -- npm ci
mise exec -- npm test          # all tests, including RFC 9605 and CPace vectors
mise exec -- npm run typecheck
mise exec -- npm run bench     # throughput table
```

`test/vectors/` holds the published test vectors, extracted mechanically from RFC 9605 Appendix C and from the base64 JSON blocks in draft-irtf-cfrg-cpace-21.
