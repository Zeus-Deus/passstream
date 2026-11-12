/**
 * Benchmark meter: reads the raw and redacted tracks off the main thread
 * (which is busy rendering the scene), so arrival times measure the pipeline,
 * not page jank. Keeps the latest frame of each for snapshots.
 */
let raw: VideoFrame | null = null;
let red: VideoFrame | null = null;
const rawAt = new Map<number, number>();
let deltas: number[] = [];
const abs = () => performance.timeOrigin + performance.now();

async function pump(readable: ReadableStream<VideoFrame>, on: (f: VideoFrame) => void) {
  const reader = readable.getReader();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    on(value);
  }
}

self.onmessage = async (e: MessageEvent) => {
  const m = e.data;
  if (m.type === "start") {
    void pump(m.raw, (f) => {
      rawAt.set(f.timestamp, abs());
      if (rawAt.size > 600) rawAt.delete(rawAt.keys().next().value!);
      raw?.close();
      raw = f;
    });
    const seen = new Set<number>();
    void pump(m.red, (f) => {
      // Only a capture frame's first arrival counts (re-sent frames get new timestamps).
      const t = seen.has(f.timestamp) ? undefined : rawAt.get(f.timestamp);
      seen.add(f.timestamp);
      const now = abs();
      if (t !== undefined) deltas.push(now - t);
      postMessage({ type: "arrive", ts: f.timestamp, at: now, rawAt: t });
      red?.close();
      red = f;
    });
  } else if (m.type === "mark") deltas = [];
  else if (m.type === "deltas") postMessage({ type: "deltas", deltas });
  else if (m.type === "snapshot") {
    // copyTo, not createImageBitmap: the latter returned blank GPU frames in headed Vulkan runs.
    const grab = async (f: VideoFrame | null) => {
      if (!f) return null;
      const w = f.visibleRect!.width, h = f.visibleRect!.height;
      const data = new Uint8ClampedArray(w * h * 4);
      await f.copyTo(data, { format: "RGBX" } as VideoFrameCopyToOptions);
      for (let i = 3; i < data.length; i += 4) data[i] = 255;
      return { w, h, data };
    };
    const before = await grab(raw), after = await grab(red);
    postMessage({ type: "snapshot", before, after }, [before?.data.buffer, after?.data.buffer].filter(Boolean) as ArrayBuffer[]);
  }
};
