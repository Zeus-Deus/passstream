import { defineConfig, type Project } from "@playwright/test";

// The viewer UX device matrix (phase 4). No Go server: the dev gallery
// (/dev/viewer-ui) and the real /v route's network-free states (unsupported,
// invalid link) on the Vite dev server. Real-server transitions are in
// test/e2e/viewer-ux.mjs.
//
//   flock -x /tmp/passstream-media.lock nice -n 10 npx playwright test -c playwright.viewer.config.ts
//
// WebKit runs in the Playwright Ubuntu image (this host lacks its libraries):
// see scripts/viewer-webkit.sh. PW_WEBKIT=/path/to/pw_run.sh selects it.
const iPhoneUA =
  "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const pixelUA =
  "Mozilla/5.0 (Linux; Android 15; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36";

export const DEV = `http://127.0.0.1:${process.env.PW_DEV_PORT ?? 5181}`;

// Phone widths from the brief: 320 (SE 1st gen), 375 (SE 2/3, mini), 390 (iPhone 12–16), 430 (Pro Max).
const phones: [string, number, number, string][] = [
  ["320", 320, 568, iPhoneUA],
  ["375", 375, 667, iPhoneUA],
  ["390", 390, 844, iPhoneUA],
  ["430", 430, 932, pixelUA],
];

function chromium(): Project[] {
  const out: Project[] = [];
  for (const [name, w, h, ua] of phones) {
    const use = { deviceScaleFactor: 3, isMobile: true, hasTouch: true, userAgent: ua };
    out.push({ name: `chromium-${name}`, use: { ...use, viewport: { width: w, height: h } } });
    out.push({ name: `chromium-${name}-landscape`, use: { ...use, viewport: { width: h, height: w } } });
  }
  return out;
}

function webkit(exe: string): Project[] {
  // WebKit has no isMobile emulation; touch, DPR and the viewport still apply.
  return phones.flatMap(([name, w, h, ua]) => [
    { name: `webkit-${name}`, use: { browserName: "webkit" as const, launchOptions: { executablePath: exe }, viewport: { width: w, height: h }, deviceScaleFactor: 3, hasTouch: true, userAgent: ua } },
  ]);
}

export default defineConfig({
  testDir: "e2e",
  testMatch: /.*\.ux\.ts$/,
  outputDir: "test-results-viewer",
  workers: 1,
  fullyParallel: false,
  reporter: [["list"]],
  timeout: 45_000,
  use: {
    browserName: "chromium",
    launchOptions: {
      executablePath: "/usr/bin/chromium",
      args: ["--no-sandbox", "--disable-dev-shm-usage", "--autoplay-policy=no-user-gesture-required"],
    },
    colorScheme: "light",
  },
  webServer: {
    command: `nice -n 10 npx vite --host 127.0.0.1 --port ${process.env.PW_DEV_PORT ?? 5181} --strictPort`,
    url: `${DEV}/dev/viewer-ui`,
    reuseExistingServer: false,
    stdout: "ignore",
  },
  projects: process.env.PW_WEBKIT ? webkit(process.env.PW_WEBKIT) : chromium(),
});
