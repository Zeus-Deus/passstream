// Runs only against the isolated source copy prepared under build/. Runtime
// configuration and the test key are supplied by TestLocalPassPageServer.
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { createRequire } from "node:module";
const root=process.env.PASSPAGE_LOCAL_SOURCE;
if(!root || !process.env.PASSPAGE_TEST_KEY_PATH)throw Error("isolated source and temporary key are required");
const source=(p)=>pathToFileURL(`${root}/src/${p}.ts`).href;
const {createApp}=await import(source("index"));
const {rawDb}=await import(source("db/index"));
const {hashToken}=await import(source("services/security"));
const db=rawDb();
db.prepare("INSERT INTO users(id,email,display_name) VALUES(?,?,?)").run("local-test-owner","local-test@example.invalid","Local test");
db.prepare("INSERT INTO api_keys(id,user_id,key_hash,label) VALUES(?,?,?,?)").run("local-test-key","local-test-owner",hashToken(readFileSync(process.env.PASSPAGE_TEST_KEY_PATH,"utf8").trim()),"Local interoperability test");
const require=createRequire(`${root}/package.json`);
const {serve}=require("@hono/node-server");
const server=serve({fetch:createApp().fetch,hostname:"127.0.0.1",port:Number(process.env.PORT)});
process.on("SIGTERM",()=>server.close(()=>process.exit(0)));
