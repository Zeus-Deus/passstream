/**
 * Dedicated worker entry for RTCRtpScriptTransform. Load it as a module worker:
 *
 *   const worker = new Worker(new URL("@passstream/e2ee/transform-worker", import.meta.url), { type: "module" });
 *
 * See worker-runtime.ts for the message protocol.
 */
import { installTransformWorker, type WorkerScopeLike } from "./worker-runtime.ts";

// The worker global is typed by lib.webworker, which can't share a program
// with lib.dom; the runtime only needs this structural slice of it.
installTransformWorker(globalThis as unknown as WorkerScopeLike);
