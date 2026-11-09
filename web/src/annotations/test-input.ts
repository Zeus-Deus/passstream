import { HostAnnotations as Router, type HostLink } from "./host";
export class HostAnnotations extends Router {
  constructor(link: HostLink, options: ConstructorParameters<typeof Router>[1] = {}) { super(link, { namespace: 1n, ...options }); }
}

import { decodeAnnotation, encodeAnnotation, type AnnotationMsg } from "./codec";

let operation = 0;
/** Production upstream format for tests that inject authenticated router events. */
export function viewerBody(body: Uint8Array): Uint8Array {
  const msg = decodeAnnotation(body);
  if (msg?.kind !== "laser" && msg?.kind !== "stroke" && !(msg?.kind === "clear" && msg.scope === "own")) return body;
  const op = ++operation;
  return encodeAnnotation({ kind: "input", namespace: 1n, session: 1n, op, clearThrough: msg.kind === "clear" ? op : 0, gen: 0, barrier: 0, msg: msg.kind === "clear" ? { kind: "clear", scope: "own" } : msg });
}
export function viewerMessage(msg: AnnotationMsg): Uint8Array {
  return viewerBody(encodeAnnotation(msg));
}

import type { AnnotationRecord } from "@passstream/e2ee";
import { originIndex } from "./codec";
/** Wrap old primitive assertions in the current authenticated app framing. */
export function fixtureBatch(records: AnnotationRecord[], own = "AAA"): AnnotationRecord[] {
  const extra: AnnotationRecord[] = [];
  const framed = records.map(r => {
    const m = decodeAnnotation(r.body);
    if (r.origin === "host" || (m?.kind !== "laser" && m?.kind !== "stroke")) return r;
    const source = 65536n + BigInt(r.origin === own ? 0 : 1 + originIndex(r.origin) % 499);
    extra.push({ origin: "host", body: encodeAnnotation({ kind: "sources", entries: [{ source, origin: r.origin, through: 0 }] }) });
    return { ...r, body: encodeAnnotation({ kind: "ink", source, op: 1, msg: m }) };
  });
  return [{ origin: "host", body: encodeAnnotation({ kind: "bootstrap", namespace: 1n, source: 65536n, gen: 0, barrier: 0 }) }, ...extra, ...framed];
}
/** Legacy primitive-order assertions still inspect the actual payloads in the new framing. */
export function payloadRecords(records: AnnotationRecord[]): AnnotationRecord[] {
  return records.flatMap(r => {
    const m = decodeAnnotation(r.body);
    if (m?.kind === "bootstrap" || m?.kind === "sources") return [];
    return [{ ...r, body: m?.kind === "ink" ? encodeAnnotation(m.msg) : r.body }];
  });
}
