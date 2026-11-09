import { test } from "node:test";
import assert from "node:assert/strict";
import { artifactNeedles } from "./artifact-needles.mjs";

test("changed-picture needles cover new content and match raw/base64 transport", () => {
 const before=Buffer.alloc(4096,31),after=Buffer.from(before);after[1300]=92;
 const needles=artifactNeedles(after,before);
 assert(needles.some((n)=>n.includes(92)));
 for(const n of needles){assert(after.includes(n));assert(after.toString("base64").includes(n.toString("base64")));}
});
test("each recording chunk contributes transport-aligned content needles",()=>{
 const chunks=[Buffer.alloc(512<<10,41),Buffer.alloc(2317,79)];
 for(const chunk of chunks)for(const n of artifactNeedles(chunk)){assert.equal(n.length,126);assert(chunk.toString("base64").includes(n.toString("base64")));}
});
