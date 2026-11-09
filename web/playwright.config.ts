import { defineConfig } from "@playwright/test";

// System Chromium, real server binary (e2e/global-setup.ts) for the
// dashboard, and the Vite dev server for the dev-only viewer UI gallery.
const iPhoneUA =
  "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const pixelUA =
  "Mozilla/5.0 (Linux; Android 15; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36";

export const DEV = "http://127.0.0.1:5179";

export default defineConfig({
  testDir: "e2e",
  testMatch: /.*\.e2e\.ts$/, // kept apart from vitest's *.test.ts
  outputDir: "test-results",
  globalSetup: "./e2e/global-setup.ts",
  workers: 1,
  fullyParallel: false,
  reporter: [["list"]],
  timeout: 60_000,
  use: {
    browserName: "chromium",
    launchOptions: {
      executablePath: "/usr/bin/chromium",
      args: ["--no-sandbox", "--disable-dev-shm-usage", "--autoplay-policy=no-user-gesture-required"],
    },
    colorScheme: "light",
  },
  webServer: {
    command: "nice -n 10 npx vite --host 127.0.0.1 --port 5179 --strictPort",
    url: `${DEV}/dev/viewer-ui`,
    reuseExistingServer: false,
    stdout: "ignore",
  },
  projects: [
    { name: "iphone-320", use: { viewport: { width: 320, height: 568 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, userAgent: iPhoneUA } },
    { name: "iphone-390", use: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, userAgent: iPhoneUA } },
    { name: "pixel-412", use: { viewport: { width: 412, height: 915 }, deviceScaleFactor: 2.625, isMobile: true, hasTouch: true, userAgent: pixelUA } },
    { name: "desktop", use: { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 } },
  ],
});
