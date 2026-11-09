import "./assets/theme.css";
import { Brand } from "./viewer-ui/parts";

/** The server's front page: nothing to do here without a link. */
export function Home() {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-5 bg-paper px-6 text-ink">
      <Brand />
      <p className="max-w-md text-center">
        Private live screen sharing. Start a share from the Omarchy bar or with <code className="font-mono">passstream share</code>, then send the link.
      </p>
      <a className="underline underline-offset-2" href="/dashboard">
        Owner dashboard
      </a>
    </main>
  );
}
