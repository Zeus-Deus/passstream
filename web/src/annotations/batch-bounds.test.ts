// Independent Fix3 mixed bounds probe, with typed upstream encoding and an actual recipient receipt.
import { viewerBody } from "./test-input";
import { expect, it } from 'vitest';
import { HostAnnotations } from './test-input';
import { ClearGenerations } from './generations';
import { encodeAnnotation, decodeAnnotation, ORIGIN_COUNT, originFromIndex } from './codec';
import { encodeAnnotationRecords } from '@passstream/e2ee';

it('full repair pages plus pending clears and varied new ink fit the actual crypto encoder', async () => {
  let batches = 0, maxBytes = 0, maxRecords = 0, maxBody = 0, secondStates = 0;
  for (const points of [0, 1, 64]) for (const origins of [1, 20, 100]) {
    let t = 0;
    const h = new HostAnnotations({
      onAnnotation: () => () => {}, onAdmitted: () => () => {}, onRevoked: () => () => {}, allowed: () => true,
      recipients: () => [{ admissionId: 'observer', rowCode: 'YYY' }],
      sendAnnotations: async (_, rs) => {
        const encoded = encodeAnnotationRecords(rs);
        batches++; maxBytes = Math.max(maxBytes, encoded.length); maxRecords = Math.max(maxRecords, rs.length);
        const covered = new Set<string>(); const sources = new Set<bigint>(); let states = 0;
        for (const r of rs) {
          maxBody = Math.max(maxBody, r.body.length);
          const m = decodeAnnotation(r.body)!; expect(m).not.toBeNull();
          if (m.kind === 'state') { states++; for (const o of m.origins) covered.add(o.origin); }
          if (m.kind === 'sources') for (const e of m.entries) sources.add(e.source);
          if (m.kind === 'stroke' || m.kind === 'ink') expect(covered.has(r.origin)).toBe(true);
          if (m.kind === 'ink') expect(sources.has(m.source)).toBe(true);
        }
        if (states === 2) secondStates++;
        return true;
      },
    }, { now: () => t });
    h.setViewers(true);
    // Also exercise the reserved receipt on the actual encoded recipient batch.
    h.receive({ admissionId: 'observer', rowCode: 'YYY', body: viewerBody(encodeAnnotation({ kind: 'stroke', id: 999, end: true, points: [{ x: 0.1, y: 0.1 }] })) });
    for (let i = 0; i < 200; i++) h.revoke({ admissionIds: [], rowCode: originFromIndex(i)! });
    for (let i = 0; i < origins; i++) h.receive({ admissionId: `ink${i}`, rowCode: originFromIndex(1000 + i)!, body: viewerBody(encodeAnnotation({ kind: 'stroke', id: i, end: true, points: Array.from({ length: points }, () => ({ x: 0.2, y: 0.4 })) })) });
    for (t = 0; t < 10000; t += 125) await h.flush();
    expect(h.pending.ink).toBe(0);
  }
  console.log(JSON.stringify({ mixed_batch_bounds: { batches, maxBytes, maxRecords, maxBody, secondStates } }));
  expect(secondStates).toBeGreaterThan(0);
  expect(maxBytes).toBeLessThanOrEqual(2048); expect(maxRecords).toBeLessThanOrEqual(64); expect(maxBody).toBeLessThanOrEqual(512);
});

it('dense and sparse pages visit each retained generation within 134 pages despite ongoing changes', () => {
  for (const stride of [1, 2, 133, 10648]) {
    const g = new ClearGenerations(); const retained = new Set<string>(); const seen = new Set<string>();
    for (let i = 0; i < ORIGIN_COUNT; i += stride) { const code = originFromIndex(i)!; retained.add(code); g.advance(code); }
    for (let n = 0; n < 134; n++) {
      for (const o of g.page().origins) seen.add(o.origin);
      g.advance(originFromIndex(0)!);
    }
    expect([...retained].every(o => seen.has(o))).toBe(true);
  }
});
