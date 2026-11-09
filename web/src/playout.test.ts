import { expect, it } from "vitest";
import { PlayoutPolicy, lossEvidence } from "./playout";
it("ignores NACKs from reordering until repair causes a freeze", () => {
  const p = new PlayoutPolicy();
  expect(p.observe(0, 0, 80, 0)).toBe(50);
  for (let t = 500, n = 1; t < 60_000; t += 500, n++) expect(p.observe(n, 0, 80, t)).toBe(50);
});
it("ignores a single freeze on a clean link", () => {
  const p = new PlayoutPolicy();
  p.observe(1, 0, 80, 0);
  expect(p.observe(2, 1, 80, 500)).toBe(50);
  expect(p.observe(3, 2, 80, 40_000)).toBe(50);
});
it("raises on repeated repair freezes, scales with RTT, and relaxes when quiet", () => {
  const p = new PlayoutPolicy();
  p.observe(3, 0, 80, 0);
  expect(p.observe(4, 1, 80, 200)).toBe(50);
  expect(p.observe(4, 2, 80, 500)).toBe(150);
  expect(p.observe(5, 1, 10, 1000)).toBeCloseTo(174.5);
  expect(p.observe(6, 1, 400, 1500)).toBeCloseTo(223.55);
  expect(p.observe(6, 1, 80, 31_400)).toBe(185);
  expect(p.observe(6, 1, 80, 31_600)).toBe(50);
});
it("keeps repair headroom across a brief low RTT sample",()=>{
 const p=new PlayoutPolicy();
 p.observe(0,0,80,0);
 for(let t=500;t<=5000;t+=500)p.observe(t/500,0,80,t,true);
 expect(p.observe(11,0,103,5500,true)).toBeCloseTo(188.45);
 expect(p.observe(12,0,57,6000,true)).toBeGreaterThan(180);
 expect(p.observe(13,0,57,6500,true)).toBeGreaterThan(175);
 expect(p.observe(14,0,80,12000,true)).toBe(185);
});
it("does not raise for freezes without repair", () => {
  const p = new PlayoutPolicy();
  expect(p.observe(0, 1, 80, 10_000)).toBe(50);
});
it("relaxes after two minutes without a freeze despite NACKs", () => {
  const p = new PlayoutPolicy();
  p.observe(1, 0, 80, 0);
  p.observe(2, 2, 80, 100);
  let n = 3;
  for (let t = 1000; t < 120_000; t += 1000) expect(p.observe(n++, 2, 80, t)).toBe(185);
  expect(p.observe(n++, 2, 80, 120_200)).toBe(50);
});
it("buffers measured loss repair before its first long freeze",()=>{
 const p=new PlayoutPolicy();p.observe(0,0,80,0);
 expect(p.observe(3,0,80,500,true)).toBe(150);
 expect(p.observe(3,0,80,1000)).toBe(185);
 expect(p.observe(3,0,80,1500)).toBe(185);
 expect(p.observe(3,0,80,2000)).toBe(185);
 expect(p.observe(3,0,80,31000)).toBe(50);
});
it("retains a brief heartbeat loss signal until the next stats observation",()=>{
 const p=new PlayoutPolicy();p.observe(0,0,80,0);
 if(lossEvidence({estimate:{averageLoss:.0255}}))p.noteLoss(1200);
 expect(p.observe(3,0,80,1600,false)).toBe(150);
 expect(p.observe(3,0,80,2100,false)).toBe(185);
 expect(p.observe(3,0,80,2600,false)).toBe(185);
 expect(p.observe(3,0,80,3100,false)).toBe(185);
 expect(p.observe(3,0,80,31200,false)).toBe(50);
});
it("requires measured loss rather than clean or malformed feedback",()=>{
 expect(lossEvidence({estimate:{averageLoss:.005}})).toBe(false);
 expect(lossEvidence({estimate:{averageLoss:"0.2"}})).toBe(false);
 expect(lossEvidence(null)).toBe(false);
});
