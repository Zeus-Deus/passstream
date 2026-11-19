import { expect, test } from "@playwright/test";
import { env } from "./helpers";

test("the dashboard can cache built assets and always fetches fresh HTML", async ({ page }) => {
  const [script, font, dashboard] = await Promise.all([
    page.waitForResponse((response) => response.request().resourceType() === "script"),
    page.waitForResponse((response) => response.request().resourceType() === "font"),
    page.goto(`${env().base}/dashboard`),
  ]);

  expect(dashboard?.status()).toBe(200);
  expect(dashboard?.headers()["cache-control"]).toBe("no-store");
  for (const asset of [script, font]) {
    expect(asset.status(), asset.url()).toBe(200);
    expect(asset.headers()["cache-control"], asset.url()).toBe("public, max-age=31536000, immutable");
  }
});
