// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";

afterEach(() => { vi.unstubAllEnvs(); vi.unstubAllGlobals(); vi.resetModules(); });

async function enabled(mode: string, webdriver: boolean, query: string) {
  vi.resetModules();
  vi.stubEnv("MODE", mode);
  vi.stubGlobal("navigator", { webdriver });
  vi.stubGlobal("location", new URL(`http://127.0.0.1/sender${query}`));
  return (await import("./test-hooks")).SENDER_TEST_HOOKS;
}
it("cannot enable hooks in a production build using automation and ?test", async () => {
  expect(await enabled("production", true, "?test=1")).toBe(false);
});
it("requires the explicit e2e build, automation and the test page", async () => {
  expect(await enabled("e2e", false, "?test=1")).toBe(false);
  expect(await enabled("e2e", true, "")).toBe(false);
  expect(await enabled("e2e", true, "?test=1")).toBe(true);
});
