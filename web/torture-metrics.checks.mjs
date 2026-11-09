import { test } from "node:test";
import assert from "node:assert/strict";
import { freshTimeline, bandwidthMetrics, bandwidthPass, frameFloorPass, meanBitrate, mediaFloorPass, nativeMetrics, nativePass, nativeHoldPass, paddingPass, nativeCadencePass } from "./torture-metrics.mjs";
const raw=(timeline)=>({timeline,samples:[],ttff:100,quality:{}});
test("recovery ignores duplicate, invalid and stale decoded frames",()=>{
 const v=raw([{at:0,frame:1,delay:30},{at:100,frame:1,delay:40},{at:200,frame:2,delay:null},{at:300,frame:3,delay:2000},{at:400,frame:4,delay:-1},{at:500,frame:5,delay:50}]);
 assert.deepEqual(freshTimeline(v,0,1000).map(f=>f.frame),[1,5]);
});
test("stale moving video fails both freshness and latency",()=>{
 const v=raw(Array.from({length:20},(_,i)=>({at:i*100,frame:i,delay:3000})));
 const m=bandwidthMetrics(v,0,2000);
 assert.equal(m.pixel_samples,0);assert.equal(m.max_gap_ms,2000);assert.equal(bandwidthPass(m),false);
});
test("a valid frame after a long freeze cannot hide the gap",()=>{
 const v=raw([{at:0,frame:1,delay:50},{at:3000,frame:2,delay:50},{at:3100,frame:3,delay:50}]);
 const m=bandwidthMetrics(v,0,3200);
 assert.equal(m.max_gap_ms,3000);assert.equal(bandwidthPass(m),false);
});
test("fresh low-latency continuous video passes",()=>{
 const v=raw(Array.from({length:30},(_,i)=>({at:i*100,frame:i,delay:200})));
 assert.equal(bandwidthPass(bandwidthMetrics(v,0,3000)),true);
});
test("frames captured before an outage ends cannot establish recovery",()=>{
 const v=raw([{at:1010,frame:1,delay:200},{at:1020,frame:2,delay:200},{at:1030,frame:3,delay:200},{at:1300,frame:4,delay:100}]);
 assert.deepEqual(freshTimeline(v,1000,2000,1000,1000).map(f=>f.frame),[4]);
});
test("the fps floor uses whole-frame precision without hiding a missing frame",()=>{
 assert.equal(frameFloorPass(378,14,30.001),true);
 assert.equal(frameFloorPass(377,14,30.001),false);
 assert.equal(frameFloorPass(378,14,30.1),false);
 assert.equal(frameFloorPass(0,0,30),false);
});
test("the byte floor includes low-rate time rather than the median sample",()=>{
 const v={samples:[{timestamp:0},{timestamp:1000,bitrate:0},{timestamp:2000,bitrate:300000},{timestamp:3000,bitrate:300000}]};
 assert.equal(meanBitrate(v,0,3000),200000);
 assert.equal(meanBitrate(v,500,2500),225000);
});
test("the byte floor weights uneven sample intervals",()=>{
 assert.equal(meanBitrate({samples:[{timestamp:0},{timestamp:500,bitrate:100000},{timestamp:2000,bitrate:300000}]},0,2000),250000);
});

test("a counter reset cannot remove low-rate time from the byte floor",()=>{
 assert.equal(meanBitrate({samples:[{timestamp:0},{timestamp:1000,bitrate:-100},{timestamp:2000,bitrate:200000}]},0,2000),100000);
});

test("repair bytes and full fps cannot mask a reduced rendition in the media floor",()=>{
 const f={applicable:true,offered_bps:200000,received_bps:400000,offered_fps:14,decoded_frames:420,observation_s:30,full_layer_share:0};
 assert.equal(mediaFloorPass(f),false);
 assert.equal(mediaFloorPass({...f,full_layer_share:.9}),true);
 assert.equal(mediaFloorPass({...f,full_layer_share:1,received_bps:170000}),false);
 assert.equal(mediaFloorPass({...f,applicable:false}),true);
});

test("B1 freshness, a full-layer label and padding cannot substitute for native pixels",()=>{
 const samples=Array.from({length:61},(_,i)=>({timestamp:i*500,layer:"L1T3",width:320,height:180,repair:{wireBytes:i*50000,paddingBytes:i*48500}}));
 const m=nativeMetrics({samples},0,30000);
 assert.equal(nativePass(m),false);
 assert.equal(paddingPass(m),false);
 assert.equal(m.first_native_ms,null);
 assert.equal(m.padding_share,.97);
 const native=nativeMetrics({samples:samples.map(s=>({...s,width:2560,height:1440,repair:{...s.repair,paddingBytes:0}}))},0,30000);
 assert.equal(nativePass(native),true);
 assert.equal(paddingPass(native),true);
 assert.equal(nativeHoldPass(native),true);
 assert.equal(nativeHoldPass({...native,native_share:60/61}),false);
 assert.equal(nativeHoldPass({...native,samples:57}),false);
 assert.equal(nativeHoldPass({...native,counter_span_ms:29999}),false);
 assert.equal(paddingPass({...native,counter_span_ms:29999}),false);
 const reset=nativeMetrics({samples:samples.map((s,i)=>({...s,repair:{wireBytes:i<30?i*50000:(i-30)*50000,paddingBytes:0}}))},0,30000);
 assert.equal(reset.counter_reset,true);
 assert.equal(reset.wire_bytes,null);
 assert.equal(paddingPass(reset),false);
});

test("native dimensions and a full layer cannot hide half the offered cadence",()=>{
 const samples=Array.from({length:61},(_,i)=>({timestamp:i*500,fps:14,layer:"L1T3",width:2560,height:1440}));
 const viewer={samples,timeline:Array.from({length:210},(_,i)=>({at:i*1000/7,frame:i,delay:100}))};
 assert.equal(nativePass(nativeMetrics(viewer,0,30000)),true);
 assert.equal(nativeCadencePass(viewer,{samples},0,30000),false);
 viewer.timeline=Array.from({length:420},(_,i)=>({at:i*1000/14,frame:i,delay:100}));
 assert.equal(nativeCadencePass(viewer,{samples},0,30000),true);
});
