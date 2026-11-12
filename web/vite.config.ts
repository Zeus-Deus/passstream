/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const e2ee = (f: string) => fileURLToPath(new URL(`../e2ee/src/${f}`, import.meta.url));
export default defineConfig({
  // The E2EE library is consumed from source; its worker is bundled as a module worker.
  resolve: {
    alias: [
      // (?=\?|$): also matches "…/transform-worker?worker", which the dev server sees with its query.
      { find: /^@passstream\/e2ee\/transform-worker(?=\?|$)/, replacement: e2ee("transform-worker.ts") },
      { find: /^@passstream\/e2ee$/, replacement: e2ee("index.ts") },
      // Safe mode: the ONNX Runtime build that loads its WASM runtime from /redact/ort/.
      { find: /^onnxruntime-web\/webgpu$/, replacement: fileURLToPath(new URL("node_modules/onnxruntime-web/dist/ort.webgpu.min.mjs", import.meta.url)) },
    ],
  },
  worker: { format: "es" },
  // Safe mode's ONNX Runtime loads its own WASM and threads; pre-bundling breaks that.
  optimizeDeps: { exclude: ["onnxruntime-web"] },
  plugins: [
    {
      // Safe mode's WASM runtime is served as static files (dev and dist alike).
      name: "ort-runtime",
      buildStart() {
        mkdirSync("public/redact/ort", { recursive: true });
        for (const f of ["ort-wasm-simd-threaded.asyncify.mjs", "ort-wasm-simd-threaded.asyncify.wasm"])
          copyFileSync(`node_modules/onnxruntime-web/dist/${f}`, `public/redact/ort/${f}`);
      },
    },
    {
      // Threaded WASM (safe mode recognition) needs cross-origin isolation, as served in production.
      name: "cross-origin-isolation",
      configureServer(server) {
        server.middlewares.use((_req, res, next) => {
          res.setHeader("Cross-Origin-Opener-Policy", "same-origin");
          res.setHeader("Cross-Origin-Embedder-Policy", "require-corp");
          next();
        });
      },
    },
    react(),
    tailwindcss(),
    {
      name: "keep",
      closeBundle() {
        if (!process.env.BENCH) writeFileSync("dist/.gitkeep", "");
      },
    },
  ],
  // `BENCH=1 vite build`: the safe mode benchmark page only, outside dist (never embedded).
  ...(process.env.BENCH
    ? { build: { outDir: "/tmp/passstream-bench", emptyOutDir: true, rollupOptions: { input: "bench/redact.html" } } }
    : {}),
  preview: { headers: { "Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Embedder-Policy": "require-corp" } },
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
