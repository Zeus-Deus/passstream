/**
 * Annotation app bodies (FEATURES.md "Track D2" §3). They travel inside the
 * E2EE annotation messages (PROTOCOL §20); the library adds the origin.
 *
 *   1 laser   n:u8 (1–64), n × (x:u16, y:u16)
 *   2 stroke  id:u16, end:u8 (0|1), n:u8 (0–64), points
 *   3 clear   scope:u8 (0 everything: host only; 1 the sender's own strokes), gen:u32
 *             (the host's clear generation; downstream only)
 *   4 policy  viewers:u8 (0|1): host only, downstream
 *   5 state   flags:u8 (bit 0: partial list), gen:u32 (clear-all generation),
 *             n:u8 (0–80), n × (origin:u16, gen:u32): host only, downstream.
 *             Authoritative clear generations, including origins without ink. Partial
 *             pages never infer a clear from absence; the host rotates retained
 *             generations and leads new ink with its origin's current generation.
 *   6 input   session:u64, op:u32, clearThrough:u32, gen:u32, barrier:u32,
 *             namespace:u64, one laser/stroke/own-clear primitive. Upstream only. Own clear's
 *             primitive gen is 0; clearThrough=op. No nested envelopes.
 *   7 receipt session:u64, through:u32, barrier:u32. Host-only, per recipient.
 *             session=0 requires through=0 (no input observed). Host barriers
 *             invalidate optimistic ink even if its input never reached the host.
 *   8 bootstrap namespace:u64, source:u64, gen:u32, barrier:u32. First in each batch.
 *   9 ink     source:u64, op:u32, laser/stroke primitive. Host stamped, downstream.
 *  10 sources n:u8 (0–32), n × (source:u64, origin:u16, through:u32). Host only.
 *             source = incarnation:u32 << 16 | slot:u16; slot < 500.
 *
 * Coordinates are normalised to the video frame: 0–65535 maps to 0–1.
 */
export const MAX_POINTS = 64;
/** Supported concurrent admissions; retired slots are reused with a higher incarnation. */
export const MAX_SOURCES = 500;
export const MAX_SOURCE_PAGE = 32;
export function sourceSlot(source: bigint): number { return Number(source & 0xffffn); }
export function validSource(source: bigint): boolean { return source > 0xffffn && source <= 0xffff_ffff_ffffn && sourceSlot(source) < MAX_SOURCES; }
export const MAX_BODY = 512;

export type Point = { x: number; y: number };
export type InkMsg =
  | { kind: "laser"; points: Point[] }
  | { kind: "stroke"; id: number; end: boolean; points: Point[] };
export type InputMsg = {
  kind: "input";
  namespace: bigint;
  session: bigint;
  op: number;
  clearThrough: number;
  gen: number;
  barrier: number;
  msg: InkMsg | { kind: "clear"; scope: "own" };
};
export type AnnotationMsg =
  | InkMsg
  | { kind: "clear"; scope: "all" | "own"; gen?: number }
  | { kind: "policy"; viewers: boolean }
  | { kind: "state"; gen: number; partial: boolean; origins: { origin: string; gen: number }[] }
  | InputMsg
  | { kind: "bootstrap"; namespace: bigint; source: bigint; gen: number; barrier: number }
  | { kind: "ink"; source: bigint; op: number; msg: InkMsg }
  | { kind: "sources"; entries: { source: bigint; origin: string; through: number }[] }
  | { kind: "receipt"; session: bigint; through: number; barrier: number };

const KIND = { laser: 1, stroke: 2, clear: 3, policy: 4, state: 5, input: 6, receipt: 7, bootstrap: 8, ink: 9, sources: 10 } as const;
/** Origins listed in one state record at most. */
export const MAX_STATE_ORIGINS = 80;
const ROW_ALPHABET = "ACDEFHJKMNPRTUVWXY3479";
const HOST_INDEX = 0xffff;
/** The finite row-code namespace plus the host; generation storage never grows with admissions. */
export const ORIGIN_COUNT = 22 ** 3 + 1;
export function originIndex(o: string): number {
  if (o === "host") return ORIGIN_COUNT - 1;
  const i = [...o].map((c) => ROW_ALPHABET.indexOf(c));
  if (o.length !== 3 || i.some((x) => x < 0)) throw new RangeError("origin");
  return 484 * i[0]! + 22 * i[1]! + i[2]!;
}
export function originFromIndex(i: number): string | null {
  if (i === ORIGIN_COUNT - 1) return "host";
  if (i >= 22 ** 3) return null;
  return ROW_ALPHABET[Math.floor(i / 484)]! + ROW_ALPHABET[Math.floor(i / 22) % 22]! + ROW_ALPHABET[i % 22]!;
}
const u32ok = (v: number) => Number.isInteger(v) && v >= 0 && v <= 0xffff_ffff;

function q(v: number): number {
  return Math.round(Math.min(1, Math.max(0, Number.isFinite(v) ? v : 0)) * 65535);
}

export function encodeAnnotation(m: AnnotationMsg): Uint8Array {
  if (m.kind === "bootstrap") {
    if (m.namespace <= 0n || m.namespace > 0xffff_ffff_ffff_ffffn || !validSource(m.source) || !u32ok(m.gen) || !u32ok(m.barrier)) throw new RangeError("bootstrap");
    const out = new Uint8Array(25), dv = new DataView(out.buffer);
    out[0] = KIND.bootstrap; dv.setBigUint64(1, m.namespace); dv.setBigUint64(9, m.source); dv.setUint32(17, m.gen); dv.setUint32(21, m.barrier);
    return out;
  }
  if (m.kind === "ink") {
    if (!validSource(m.source) || !u32ok(m.op) || m.op === 0) throw new RangeError("ink source");
    const body = encodeAnnotation(m.msg), out = new Uint8Array(13 + body.length), dv = new DataView(out.buffer);
    out[0] = KIND.ink; dv.setBigUint64(1, m.source); dv.setUint32(9, m.op); out.set(body, 13); return out;
  }
  if (m.kind === "sources") {
    if (m.entries.length > MAX_SOURCE_PAGE) throw new RangeError("source page");
    const out = new Uint8Array(2 + 14 * m.entries.length), dv = new DataView(out.buffer);
    out[0] = KIND.sources; out[1] = m.entries.length;
    m.entries.forEach((e, i) => {
      if (!validSource(e.source) || !u32ok(e.through)) throw new RangeError("source state");
      dv.setBigUint64(2 + 14 * i, e.source); dv.setUint16(10 + 14 * i, originIndex(e.origin)); dv.setUint32(12 + 14 * i, e.through);
    }); return out;
  }
  if (m.kind === "input" || m.kind === "receipt") {
    if (m.session < 0n || m.session > 0xffff_ffff_ffff_ffffn) throw new RangeError("input session");
    if (m.kind === "receipt") {
      if (!u32ok(m.through) || !u32ok(m.barrier) || (m.session === 0n && m.through !== 0)) throw new RangeError("input cutoff");
      const out = new Uint8Array(17);
      const dv = new DataView(out.buffer);
      out[0] = KIND.receipt;
      dv.setBigUint64(1, m.session);
      dv.setUint32(9, m.through);
      dv.setUint32(13, m.barrier);
      return out;
    }
    if (m.namespace <= 0n || m.namespace > 0xffff_ffff_ffff_ffffn || m.session === 0n || !u32ok(m.gen) || !u32ok(m.barrier) || !u32ok(m.op) || m.op === 0 || !u32ok(m.clearThrough) || m.clearThrough > m.op || (m.msg.kind === "clear" && m.clearThrough !== m.op)) throw new RangeError("input operation");
    const body = encodeAnnotation(m.msg);
    const out = new Uint8Array(33 + body.length);
    const dv = new DataView(out.buffer);
    out[0] = KIND.input;
    dv.setBigUint64(1, m.session);
    dv.setUint32(9, m.op);
    dv.setUint32(13, m.clearThrough);
    dv.setUint32(17, m.gen);
    dv.setUint32(21, m.barrier);
    dv.setBigUint64(25, m.namespace);
    out.set(body, 33);
    return out;
  }
  if (m.kind === "state") {
    if (m.origins.length > MAX_STATE_ORIGINS || !u32ok(m.gen)) throw new RangeError("state");
    const out = new Uint8Array(7 + 6 * m.origins.length);
    const dv = new DataView(out.buffer);
    out[0] = KIND.state;
    out[1] = m.partial ? 1 : 0;
    dv.setUint32(2, m.gen);
    out[6] = m.origins.length;
    m.origins.forEach((o, i) => {
      if (!u32ok(o.gen)) throw new RangeError("state gen");
      dv.setUint16(7 + 6 * i, o.origin === "host" ? HOST_INDEX : originIndex(o.origin));
      dv.setUint32(9 + 6 * i, o.gen);
    });
    return out;
  }
  if (m.kind === "clear") {
    const gen = m.gen ?? 0;
    if (!u32ok(gen)) throw new RangeError("clear gen");
    const out = new Uint8Array(6);
    out[0] = KIND.clear;
    out[1] = m.scope === "all" ? 0 : 1;
    new DataView(out.buffer).setUint32(2, gen);
    return out;
  }
  const pts = "points" in m ? m.points : [];
  if (pts.length > MAX_POINTS) throw new RangeError("too many points");
  if (m.kind === "laser" && pts.length === 0) throw new RangeError("a laser needs a point");
  const head = m.kind === "laser" ? 2 : m.kind === "stroke" ? 5 : 2;
  const out = new Uint8Array(head + 4 * pts.length);
  const dv = new DataView(out.buffer);
  out[0] = KIND[m.kind];
  let off = 1;
  if (m.kind === "laser") out[off++] = pts.length;
  else if (m.kind === "stroke") {
    if (!Number.isInteger(m.id) || m.id < 0 || m.id > 0xffff) throw new RangeError("stroke id");
    dv.setUint16(off, m.id);
    out[off + 2] = m.end ? 1 : 0;
    out[off + 3] = pts.length;
    off += 4;
  } else out[off++] = m.viewers ? 1 : 0;
  for (const p of pts) {
    dv.setUint16(off, q(p.x));
    dv.setUint16(off + 2, q(p.y));
    off += 4;
  }
  return out;
}

/** Strict: wrong sizes, unknown kinds or flags → null. */
export function decodeAnnotation(b: Uint8Array): AnnotationMsg | null {
  if (!(b instanceof Uint8Array) || b.length < 2 || b.length > MAX_BODY) return null;
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const points = (off: number, n: number): Point[] | null => {
    if (n > MAX_POINTS || b.length !== off + 4 * n) return null;
    const pts: Point[] = [];
    for (let i = 0; i < n; i++) pts.push({ x: dv.getUint16(off + 4 * i) / 65535, y: dv.getUint16(off + 4 * i + 2) / 65535 });
    return pts;
  };
  switch (b[0]) {
    case 6: {
      if (b.length < 35) return null;
      const session = dv.getBigUint64(1), op = dv.getUint32(9), clearThrough = dv.getUint32(13), gen = dv.getUint32(17), barrier = dv.getUint32(21), namespace = dv.getBigUint64(25);
      // Only primitives can be nested; reject envelopes before recursing.
      if (namespace === 0n || session === 0n || op === 0 || clearThrough > op || b[33]! < 1 || b[33]! > 3) return null;
      const msg = decodeAnnotation(b.subarray(33));
      if (msg?.kind === "laser" || msg?.kind === "stroke") return { kind: "input", namespace, session, op, clearThrough, gen, barrier, msg };
      if (msg?.kind === "clear" && msg.scope === "own" && msg.gen === 0 && clearThrough === op) return { kind: "input", namespace, session, op, clearThrough, gen, barrier, msg: { kind: "clear", scope: "own" } };
      return null;
    }
    case 8:
      return b.length === 25 && dv.getBigUint64(1) !== 0n && validSource(dv.getBigUint64(9)) ? { kind: "bootstrap", namespace: dv.getBigUint64(1), source: dv.getBigUint64(9), gen: dv.getUint32(17), barrier: dv.getUint32(21) } : null;
    case 9: {
      if (b.length < 15 || !validSource(dv.getBigUint64(1)) || dv.getUint32(9) === 0 || (b[13] !== 1 && b[13] !== 2)) return null;
      const msg = decodeAnnotation(b.subarray(13));
      return msg?.kind === "laser" || msg?.kind === "stroke" ? { kind: "ink", source: dv.getBigUint64(1), op: dv.getUint32(9), msg } : null;
    }
    case 10: {
      if (b[1]! > MAX_SOURCE_PAGE || b.length !== 2 + 14 * b[1]!) return null;
      const entries: { source: bigint; origin: string; through: number }[] = [];
      for (let i = 0; i < b[1]!; i++) {
        const source = dv.getBigUint64(2 + 14 * i), origin = originFromIndex(dv.getUint16(10 + 14 * i));
        if (!validSource(source) || !origin) return null;
        entries.push({ source, origin, through: dv.getUint32(12 + 14 * i) });
      } return { kind: "sources", entries };
    }
    case 7:
      return b.length === 17 && (dv.getBigUint64(1) !== 0n || dv.getUint32(9) === 0) ? { kind: "receipt", session: dv.getBigUint64(1), through: dv.getUint32(9), barrier: dv.getUint32(13) } : null;
    case 1: {
      const n = b[1]!;
      const pts = n >= 1 ? points(2, n) : null;
      return pts ? { kind: "laser", points: pts } : null;
    }
    case 2: {
      if (b.length < 5) return null;
      const end = b[3]!;
      if (end > 1) return null;
      const pts = points(5, b[4]!);
      return pts ? { kind: "stroke", id: dv.getUint16(1), end: end === 1, points: pts } : null;
    }
    case 3:
      return b.length === 6 && b[1]! <= 1 ? { kind: "clear", scope: b[1] === 0 ? "all" : "own", gen: dv.getUint32(2) } : null;
    case 5: {
      if (b.length < 7 || b[1]! > 1) return null;
      const n = b[6]!;
      if (n > MAX_STATE_ORIGINS || b.length !== 7 + 6 * n) return null;
      const origins: { origin: string; gen: number }[] = [];
      for (let i = 0; i < n; i++) {
        const index = dv.getUint16(7 + 6 * i);
        const origin = index === HOST_INDEX ? "host" : index < ORIGIN_COUNT - 1 ? originFromIndex(index) : null;
        if (origin === null) return null;
        origins.push({ origin, gen: dv.getUint32(9 + 6 * i) });
      }
      return { kind: "state", partial: b[1] === 1, gen: dv.getUint32(2), origins };
    }
    case 4:
      return b.length === 2 && b[1]! <= 1 ? { kind: "policy", viewers: b[1] === 1 } : null;
  }
  return null;
}

/** What only the host may send: the policy, and clearing everyone's strokes. */
export function hostOnly(m: AnnotationMsg): boolean {
  return m.kind === "bootstrap" || m.kind === "sources" || m.kind === "policy" || m.kind === "state" || m.kind === "receipt" || (m.kind === "clear" && m.scope === "all");
}
