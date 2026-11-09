export function freshTimeline(v,from,to,maxDelay=1000,capturedAfter=-Infinity) {
 let previous=null;
 return v.timeline.filter(f=>{
  const advancing=f.frame!==null && (previous===null || f.frame>previous);
  if (f.frame!==null && advancing) previous=f.frame;
  return advancing && f.at>=from && f.at<=to && f.delay!==null && f.delay>=0 && f.delay<=maxDelay && f.at-f.delay>=capturedAfter;
 });
}
export function bandwidthMetrics(v,from,to) {
 const fresh=freshTimeline(v,from,to);
 const m=summarize({...v,timeline:fresh},from,to);
 const latency=summarize(v,from,to);
 return {...m,g2g_p95_ms:latency.g2g_p95_ms};
}
export function bandwidthPass(m) { return m.pixel_samples>0 && m.g2g_p95_ms<=1000 && m.max_gap_ms<=1000 && m.last_frame_age_ms<=1000 }
// A finite observation has whole frames, while its wall clock can end a few
// milliseconds late. Score the 90% floor at the nearest measurable frame.
export function frameFloorPass(frames, offeredFPS, seconds) {
 return offeredFPS>0 && seconds>0 && frames>=Math.round(offeredFPS*seconds*.9);
}
// Each sampled bitrate describes the byte delta ending at that sample. Weight
// its overlap with the observation so low-rate intervals cannot hide in a median.
export function meanBitrate(v,from,to) {
 let bits=0,elapsed=0;
 for(let i=1;i<v.samples.length;i++) {
  const end=v.samples[i],start=v.samples[i-1];
  const ms=Math.max(0,Math.min(to,end.timestamp)-Math.max(from,start.timestamp));
  if(ms>0) {
   elapsed+=ms;
   if(Number.isFinite(end.bitrate) && end.bitrate>=0) bits+=end.bitrate*ms;
  }
 }
 return elapsed>0?bits/elapsed:0;
}
// Full-stream fairness includes the selected native temporal layer, so repair
// bytes from a reduced rendition cannot substitute for the offered stream.
export function mediaFloorPass(f) {
 return !f.applicable || f.received_bps>=f.offered_bps*.9 && frameFloorPass(f.decoded_frames,f.offered_fps,f.observation_s) && f.full_layer_share>=.9;
}
export function pct(a, p) {
  if (!a.length) return null;
  const sorted = [...a].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.ceil(sorted.length * p) - 1)];
}
export function summarize(v, from, to) {
  const frames = v.timeline.filter((f) => f.at >= from && f.at <= to);
  const g2g = frames.filter((f) => f.delay !== null).map((f) => f.delay);
  const gaps = [];
  if ((frames[0]?.at ?? to) - from > 500)
    gaps.push((frames[0]?.at ?? to) - from);
  for (let i = 1; i < frames.length; i++)
    if (frames[i].at - frames[i - 1].at > 500)
      gaps.push(frames[i].at - frames[i - 1].at);
  if (frames.length && to - frames.at(-1).at > 500)
    gaps.push(to - frames.at(-1).at);
  const samples = v.samples.filter(
    (s) => s.timestamp >= from && s.timestamp <= to,
  );
  const last = frames.at(-1);
  return {
    ttff_ms: v.ttff || null,
    g2g_p50_ms: pct(g2g, 0.5),
    g2g_p95_ms: pct(g2g, 0.95),
    pixel_samples: g2g.length,
    frames: frames.length,
    freezes_over_500ms: gaps.length,
    max_gap_ms: Math.max(0, ...gaps),
    freezeCount: v.quality.freezes,
    totalFreezesDuration: v.quality.freezeSeconds,
    bitrate_p50_bps: pct(
      samples.map((s) => s.bitrate),
      0.5,
    ),
    rtt_p50_ms: pct(
      samples.map((s) => s.rtt),
      0.5,
    ),
    loss_p50_pct: pct(
      samples.map((s) => s.loss),
      0.5,
    ),
    path: v.quality.path,
    layer: v.quality.layer,
    last_frame_age_ms: last ? to - last.at : null,
    replacements: v.replacements,
  };
}

// Native means both readable source pixels and the full temporal layer.
export function nativeMetrics(v, from, to, width = 2560, height = 1440) {
 const samples = v.samples.filter(s=>s.timestamp>=from && s.timestamp<=to);
 const native = samples.filter(s=>s.layer === "L1T3" && s.width>=width && s.height>=height);
 const first = native[0];
 const a = samples[0]?.repair, b = samples.at(-1)?.repair;
 const counters = samples.map(s=>s.repair).filter(Boolean);
 const reset = counters.some((s,i)=>i>0&&(s.wireBytes<counters[i-1].wireBytes||s.paddingBytes<counters[i-1].paddingBytes));
 const valid = !reset && [a?.wireBytes,b?.wireBytes,a?.paddingBytes,b?.paddingBytes].every(Number.isFinite);
 const wire = valid ? b.wireBytes-a.wireBytes : null;
 const padding = valid ? b.paddingBytes-a.paddingBytes : null;
 return {samples:samples.length,counter_span_ms:samples.length>1?samples.at(-1).timestamp-samples[0].timestamp:0,native_share:native.length/Math.max(1,samples.length),first_native_ms:first?first.timestamp-from:null,padding_share:wire>0?padding/wire:null,wire_bytes:wire,padding_bytes:padding,counter_reset:reset};
}
export function nativePass(m) { return m.samples>=10 && m.native_share>=.9; }
// Admission rows have a quiet 30s hold after the deadline: no rendition or
// temporal regression is allowed, with at most two missing 500ms samples.
export function nativeHoldPass(m) { return m.samples>=60 && m.counter_span_ms>=30000 && m.native_share===1; }
export function paddingPass(m) { return m.counter_span_ms>=30000 && !m.counter_reset && m.wire_bytes>0 && Number.isFinite(m.padding_share) && m.padding_share<=.5; }

// Readable native video must deliver the offered cadence as fresh pictures;
// selected-layer labels alone cannot establish that the encoder's work arrived.
export function nativeCadencePass(viewer, publisher, from, to) {
 const offered=pct(publisher.samples.filter(s=>s.timestamp>=from&&s.timestamp<=to).map(s=>s.fps),.5);
 return frameFloorPass(freshTimeline(viewer,from,to).length,offered,(to-from)/1000);
}
