// Vite replaces MODE at build time. Normal production builds erase these hooks.
export const SENDER_TEST_HOOKS = import.meta.env.MODE === "e2e" &&
  typeof navigator !== "undefined" && navigator.webdriver &&
  typeof location !== "undefined" && new URLSearchParams(location.search).has("test");
