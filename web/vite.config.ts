/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const e2ee = (f: string) => fileURLToPath(new URL(`../e2ee/src/${f}`, import.meta.url));
export default defineConfig({
  // The E2EE library is consumed from source; its worker is bundled as a module worker.
  resolve: {
    alias: [
      // (?=\?|$): also matches "…/transform-worker?worker", which the dev server sees with its query.
      { find: /^@passstream\/e2ee\/transform-worker(?=\?|$)/, replacement: e2ee("transform-worker.ts") },
      { find: /^@passstream\/e2ee$/, replacement: e2ee("index.ts") },
    ],
  },
  worker: { format: "es" },
  plugins: [
    react(),
    tailwindcss(),
    {
      name: "keep",
      closeBundle() {
        writeFileSync("dist/.gitkeep", "");
      },
    },
  ],
  test: {
    // contrast.test.ts reads the theme's colour tokens as text.
    css: { include: [/theme\.css/] },
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/signal": { target: "ws://127.0.0.1:8080", ws: true },
    },
  },
});
