// Builds the real server binary from this checkout (embedding web/dist, so
// run `npm run build` first), starts it on free loopback ports with a
// temporary data dir, and seeds a showcase stream through a scripted sender.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { createSocket } from "node:dgram";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer as createHTTPServer } from "node:http";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { api, FakeSender, UA } from "./fake-sender";

const repo = fileURLToPath(new URL("../..", import.meta.url));

async function freePort(): Promise<number> {
  for (;;) {
    const port = 20000 + Math.floor(Math.random() * 30000);
    const tcp = await new Promise<boolean>((ok) => {
      const s = createServer().once("error", () => ok(false));
      s.listen(port, "127.0.0.1", () => s.close(() => ok(true)));
    });
    const udp = await new Promise<boolean>((ok) => {
      const s = createSocket("udp4").once("error", () => ok(false));
      s.bind(port, "127.0.0.1", () => s.close(() => ok(true)));
    });
    if (tcp && udp) return port;
  }
}

export default async function setup() {
  const dir = mkdtempSync(join(tmpdir(), "passstream-e2e-"));
  const bin = join(dir, "passstream");
  execFileSync("nice", ["-n", "10", "mise", "exec", "--", "go", "build", "-o", bin, "./cmd/passstream"], {
    cwd: repo,
    env: { ...process.env, CGO_ENABLED: "0" },
    stdio: "inherit",
  });
  const [http, ice, turn] = [await freePort(), await freePort(), await freePort()];
  const base = `http://127.0.0.1:${http}`;
  const data = join(dir, "data");
  const proc: ChildProcess = spawn(
    bin,
    ["serve", "--data", data, "--listen", `127.0.0.1:${http}`, "--base-url", base, "--ice-udp", `127.0.0.1:${ice}`, "--ice-tcp", `127.0.0.1:${ice}`, "--turn", `127.0.0.1:${turn}`, "--trust-proxy", "127.0.0.1"],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  let log = "";
  proc.stderr!.on("data", (b) => (log = (log + b).slice(-8000)));
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(`${base}/readyz`)).ok) break;
    } catch {
      /* not up yet */
    }
    if (i > 100) throw new Error(`server didn't start:\n${log}`);
    await new Promise((r) => setTimeout(r, 100));
  }
  const key = readFileSync(join(data, "owner-key"), "utf8").trim();

  // An ended stream, for the "ended" section.
  const old = await FakeSender.create(base, key, { label: "Standup notes" });
  await api(base, key, `/api/v1/streams/${old.stream.id}`, { method: "DELETE" });

  // The showcase: watching, reconnecting-free, left and knocking rows.
  const show = await FakeSender.create(base, key, { label: "Design review" });
  await show.connect();
  const ana = await show.viewer("Ana", UA.iphone, "203.0.113.20");
  const bo = await show.viewer("Bo", UA.mac, "198.51.100.7");
  const anon = await show.viewer(null, UA.android, "192.0.2.44");
  const left = await show.viewer("Chris", UA.linux, "203.0.113.99");
  const hana = await show.viewer("Hana", UA.android, "198.51.100.150");
  show.admit(ana, "PFT", false, 11 * 60_000);
  show.admit(bo, "K7P", true, 4 * 60_000);
  show.admit(anon, "M3X", false, 30_000);
  const gone = show.admit(left, "RAW", false, 20 * 60_000);
  show.knock(hana, "UPV");
  const dee = await show.viewer("Dee", UA.mac, "192.0.2.80");
  const denied = show.knock(dee, "TEX");
  await show.settle();
  show.kick(gone.admission_id);
  show.endKnock(denied.knock_id);
  show.failedPins(9, 2);
  await show.settle();

  // Workers can't reach this process's sockets, so the showcase sender takes
  // pokes over loopback: alerts aren't replayed to late SSE subscribers.
  const control = createHTTPServer((req, res) => {
    if (req.url === "/failed-pin") show.failedPins(2, 2);
    res.end();
  });
  await new Promise<void>((r) => control.listen(0, "127.0.0.1", r));
  process.env.PS_CONTROL = `http://127.0.0.1:${(control.address() as { port: number }).port}`;
  process.env.PS_BASE = base;
  process.env.PS_KEY = key;
  process.env.PS_SHOWCASE = show.stream.id;
  process.env.PS_ENDED = old.stream.id;

  return async () => {
    show.close();
    control.close();
    proc.kill("SIGTERM");
    await new Promise((r) => proc.once("exit", r));
    rmSync(dir, { recursive: true, force: true });
  };
}
