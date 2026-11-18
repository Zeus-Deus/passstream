import "./assets/theme.css";
import { Brand } from "./viewer-ui/parts";

export function Home() {
  return (
    <main className="grid min-h-dvh place-items-center px-5 py-6 sm:py-10">
      <div className="w-full max-w-md">
        <Brand />
        <h1 className="display mt-4 mb-5 text-[1.8rem] leading-tight sm:text-4xl">Private screen sharing</h1>
        <div className="sheet divide-y divide-paper-edge">
          <section className="p-4 sm:p-6">
            <h2 className="display text-xl">Watch a stream</h2>
            <p className="mt-2 text-ink-soft">
              Open the private link from your host in your browser. Enter their PIN if prompted. You can watch without an account.
            </p>
          </section>
          <section className="p-4 sm:p-6">
            <h2 className="display text-xl">Share your screen</h2>
            <p className="mt-2 text-ink-soft">
              Use the PassStream bar plugin in Omarchy or run <code className="num whitespace-nowrap">passstream share</code>. Pick what to share, then send the link and any PIN separately.
            </p>
          </section>
        </div>
        <a className="mt-3 inline-flex min-h-11 items-center text-ink-soft underline underline-offset-2" href="/dashboard">
          Owner dashboard
        </a>
      </div>
    </main>
  );
}
