import { describe, it, expect } from "vitest";
import { RecoveryPolicy, selectedPair, videoFreshnessLimit } from "./recovery";
const stats = (values: object[]) => new Map(values.map((s, i) => [String(i), {id: String(i), ...s}])) as unknown as RTCStatsReport;
const pongs = (p: RecoveryPolicy, from: number, to: number) => { for (let t = from; t <= to; t += 500) p.pong(t); };
describe("recovery policy", () => {
 it("uses media and consent progress even when heartbeats are lost", () => {
  const p=new RecoveryPolicy(); p.reset(0); pongs(p,0,10000);
  for(let n=1;n<20;n++) { p.observe(stats([{type:"transport",bytesReceived:n}]),n*500); expect(p.action(n*500,80,true)).toBe("none"); }
 });
 it("replaces at once when signaling answered throughout the media stall", () => {
  const p=new RecoveryPolicy(); p.reset(0); pongs(p,0,1000);
  expect(p.action(900,80,true)).toBe("none");
  expect(p.action(1100,80,true)).toBe("replace");
 });
 it("scales the stall threshold with RTT", () => {
  const p=new RecoveryPolicy(); p.reset(0); pongs(p,0,1500);
  expect(p.action(1100,400,true)).toBe("none");
  expect(p.action(1700,400,true)).toBe("replace");
 });
 it("waits through a total outage and gives the old path a grace period", () => {
  const p=new RecoveryPolicy(); p.reset(0); p.pong(0);
  expect(p.action(1100,80,true)).toBe("none");
  expect(p.action(2500,80,true)).toBe("none");
  p.pong(2600);
  expect(p.action(3000,80,true)).toBe("none");
  p.observe(stats([{type:"transport",bytesReceived:5}]),3100);
  p.pong(3100);
  expect(p.action(3200,80,true)).toBe("none");
 });
 it("replaces when media stays stalled after signaling returns", () => {
  const p=new RecoveryPolicy(); p.reset(0); p.pong(0);
  pongs(p,2600,3600);
  expect(p.action(3500,80,true)).toBe("none");
  expect(p.action(3600,80,true)).toBe("replace");
 });
 it("treats a reconnected socket as newly responsive", () => {
  const p=new RecoveryPolicy(); p.reset(0); p.pong(0);
  p.signalingLost(); p.pong(800);
  expect(p.action(1100,80,true)).toBe("none");
  expect(p.action(1100,80,false)).toBe("none");
 });
 it("replaces after six seconds without progress regardless of signaling", () => {
  const p=new RecoveryPolicy(); p.reset(0);
  expect(p.action(1100,80,false)).toBe("none");
  expect(p.action(7000,80,false)).toBe("none");
  expect(p.action(7100,80,false)).toBe("replace");
 });
 it("only reports the transport selected pair", () => {
  const s=stats([{type:"candidate-pair",nominated:true},{type:"candidate-pair",nominated:true},{type:"transport",selectedCandidatePairId:"1"}]);
  expect(selectedPair(s)?.id).toBe("1");
 });
 it("ignores consent on an unselected alternative pair", () => {
  const p=new RecoveryPolicy();p.reset(0); pongs(p,0,1600);
  const s=stats([{type:"candidate-pair",responsesReceived:20},{type:"candidate-pair",responsesReceived:0},{type:"transport",selectedCandidatePairId:"1"}]);
  p.observe(s,1600);expect(p.action(1600,80,true)).toBe("replace");
 });
});

describe("decoded media recovery", () => {
 it("detects a decoder stall through video deltas when SCTP counters are delayed", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);pongs(p,0,2000);
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:10,bytesReceived:100}]),500);
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:20,bytesReceived:200}]),2000);
  expect(p.action(2000,80,true)).toBe("replace");
  p.video(2100);expect(p.action(2100,80,true)).toBe("none");
 });
 it("does not infer video activity from a first or unchanged idle snapshot", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);pongs(p,0,5000);
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:10,bytesReceived:100}]),2000);
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:10,bytesReceived:100}]),5000);
  p.alive(5000);expect(p.action(5000,80,true)).toBe("none");
 });
 it("keeps audio activity and old-PC video deltas out of decoded recovery", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);pongs(p,0,5000);
  p.observe(stats([{type:"inbound-rtp",kind:"audio",packetsReceived:10,bytesReceived:100}]),500);
  p.observe(stats([{type:"inbound-rtp",kind:"audio",packetsReceived:20,bytesReceived:200}]),2000);
  expect(p.action(2000,80,true)).toBe("none");
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:10}]),2500);
  p.observe(stats([{type:"inbound-rtp",kind:"video",packetsReceived:20}]),4000);
  p.reset(4100);p.video(4100);p.alive(5000);
  expect(p.action(5000,80,true)).toBe("none");
 });
 it("does not let incoming packets mask a decoder stall", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);pongs(p,0,2000);
  p.source(1,500);p.alive(500);p.source(20,2000);p.alive(2000);
  expect(p.action(2000,80,true)).toBe("replace");
 });
 it("does not replace an idle screen whose last counter arrived later", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(1,500);pongs(p,0,5000);p.alive(5000);
  expect(p.action(5000,80,true)).toBe("none");
 });
 it("ignores delayed source counters on an unordered heartbeat", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(10,500);p.source(5,2000);p.source(10,3000);pongs(p,0,5000);p.alive(5000);
  expect(p.action(5000,80,true)).toBe("none");
 });
 it("rejects stale moving video and resumes on a fresh picture", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);pongs(p,0,2000);
  p.video(1500,false);p.source(20,2000);p.alive(2000);
  expect(p.action(2000,80,true)).toBe("replace");
  p.video(2100);expect(p.action(2100,80,true)).toBe("none");
 });
 it("keeps the signaling-return grace during a decoded stall", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);p.pong(0);p.signalingLost();p.pong(2600);
  p.source(30,2700);p.alive(2700);expect(p.action(3000,80,true)).toBe("none");
  p.video(3100);p.pong(3100);expect(p.action(3200,80,true)).toBe("none");
 });
 it("does not reset a decoded-stall deadline on each heartbeat", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(20,2000);p.alive(2000);
  expect(p.action(2000,80,false)).toBe("none");
  p.source(90,8100);p.alive(8100);expect(p.action(8100,80,false)).toBe("replace");
 });
 it("clears the decoded clock for a replacement", () => {
  const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(20,2000);p.reset(2000);pongs(p,2000,3000);p.alive(3000);
  expect(p.action(3000,80,true)).toBe("none");
 });
 it("allows physical path latency without accepting congestion as a baseline", () => {
  expect(videoFreshnessLimit({initialRTTMs:800})).toBe(1300);
  expect(videoFreshnessLimit({initialRTTMs:1})).toBe(1000);
  expect(videoFreshnessLimit({initialRTTMs:Infinity})).toBe(1000);
  expect(videoFreshnessLimit(null)).toBe(1000);
 });
});

it("backs off repeated replacements until decoded video stays healthy",()=>{
 const p=new RecoveryPolicy();p.reset(0);p.replaced(2000);p.reset(2100);p.video(2500,false);p.source(10,5000);p.alive(5000);pongs(p,2100,8200);
 expect(p.action(6000,80,true)).toBe("none");
 expect(p.action(8200,80,true)).toBe("replace");
 p.replaced(8200);p.reset(8300);p.video(9000,false);p.source(30,11000);p.alive(11000);pongs(p,8300,16000);
 expect(p.action(16000,80,true)).toBe("none");
 for(let t=16000;t<=20000;t+=500)p.video(t);
 p.source(100,22000);p.alive(22000);pongs(p,20000,22000);
 expect(p.action(22000,80,true)).toBe("replace");
});

it("defers the decoded deadline while hidden and detects a stuck resume",()=>{
 const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(20,5000);p.alive(5000);pongs(p,0,5000);p.deferVideo(5000);
 expect(p.action(5000,80,true)).toBe("none");
 p.source(30,6500);p.alive(6500);pongs(p,5500,6500);expect(p.action(6500,80,true)).toBe("replace");
});

it("does not turn congestion-inflated RTT into a decoder stall allowance",()=>{
 const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(20,2000);p.alive(2000);pongs(p,0,2000);
 expect(p.action(2000,2000,true,1)).toBe("replace");
});
it("retains the stall allowance of an intrinsically high-RTT decoder path",()=>{
 const p=new RecoveryPolicy();p.reset(0);p.video(0);p.source(20,2000);p.alive(2000);pongs(p,0,4000);
 expect(p.action(2000,800,true,800)).toBe("none");
 p.source(40,4000);p.alive(4000);expect(p.action(4000,800,true,800)).toBe("replace");
});
