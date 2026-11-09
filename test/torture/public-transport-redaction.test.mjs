import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { redactText, redactReport, serializeReport } from "./public-transport-redact.mjs";

// Run this browser-only failure probe under /tmp/passstream-media.lock too.
test("failed Playwright navigation, page errors, diagnostics and stderr redact fragments", async () => {
  const { chromium } = await import(pathToFileURL(resolve("web/node_modules/@playwright/test/index.mjs")).href);
  const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true, args: ["--no-sandbox", "--no-proxy-server"] });
  const needle = "FAKE_TEST_CAPABILITY_NEEDLE_DO_NOT_USE";
  const url = `http://127.0.0.1:1/v#c=${needle}&k=FAKE_KEY&s=FAKE_SECRET`;
  let navigation;
  const pageErrors = [];
  try {
    const page = await browser.newPage();
    page.on("pageerror", error => pageErrors.push(redactText(error)));
    try { await page.goto(url, { timeout: 2000 }); } catch (error) { navigation = error; }
    assert.ok(navigation, "probe requires a navigation failure");
    assert.equal(String(navigation).includes(needle), true, "probe requires Playwright's actual fragment-bearing error");
    const diagnosticsPage = await browser.newPage();
    diagnosticsPage.on("pageerror", error => pageErrors.push(redactText(error)));
    const errorEvent = diagnosticsPage.waitForEvent("pageerror");
    await diagnosticsPage.evaluate(url => { setTimeout(() => { throw Error(url); }, 0); }, url);
    await errorEvent;
    let diagnostic;
    try { await diagnosticsPage.evaluate(url => { throw Error(url); }, url); } catch (error) { diagnostic = { error: redactText(error) }; }
    const result = redactReport({ route: "failure-probe", pass: false, error: redactText(navigation), page_errors: pageErrors, diagnostics: [{ events: [{ nested_url: url }], ...diagnostic }] });
    const file = join(mkdtempSync(join(tmpdir(), "passstream-redaction-")), "failure.json");
    writeFileSync(file, serializeReport({ results: [result], error: navigation.stack, success_diagnostic: { url } }));
    const report = readFileSync(file, "utf8");
    assert.equal(report.includes(needle), false, "report must not retain a fragment");
    assert.equal(report.includes("FAKE_SECRET"), false, "report must not retain other fragment fields");
    assert.ok(pageErrors.length > 0 && report.includes("[redacted]"));
    const child = spawnSync(process.execPath, ["--input-type=module", "-e", `import {redactText} from ${JSON.stringify(pathToFileURL(resolve("test/torture/public-transport-redact.mjs")).href)}; console.error(redactText(process.env.FAKE_ERROR));`], { env: { ...process.env, FAKE_ERROR: navigation.stack }, encoding: "utf8" });
    assert.equal(child.status, 0);
    assert.equal(child.stderr.includes(needle), false, "stderr must not print the original Error");
    assert.ok(child.stderr.includes("[redacted]"));
    assert.equal(serializeReport({ nested: [url, "wss://example.test/ws#" + needle] }).includes(needle), false);
    console.log("Actual Playwright failure contained fake fragment; persisted JSON, pageerror, nested diagnostics, cleanup stack and stderr redacted it.");
  } finally { await browser.close(); }
});
