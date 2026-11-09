import type { ReactNode } from "react";
import { Brand } from "./parts";

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="border-t border-paper-edge pt-6">
      <h2 className="display text-[1.45rem] leading-tight">{title}</h2>
      <div className="mt-3 space-y-3 text-[0.95rem] leading-relaxed text-ink-soft">{children}</div>
    </section>
  );
}

function Row({ what, who, why }: { what: string; who: string; why: string }) {
  return (
    <tr className="border-b border-paper-edge align-top last:border-0">
      <th scope="row" className="py-2.5 pr-3 text-left font-medium text-ink">{what}</th>
      <td className="py-2.5 pr-3">{who}</td>
      <td className="py-2.5">{why}</td>
    </tr>
  );
}

/** The join screen's privacy notice: who sees what, why, how long, rights. */
export function Privacy() {
  return (
    <main className="mx-auto max-w-2xl px-4 pt-[max(1.25rem,env(safe-area-inset-top))] pb-16 sm:px-6">
      <div className="flex items-center justify-between pb-8">
        <a href="/" aria-label="PassStream"><Brand /></a>
        <span className="label">Privacy notice</span>
      </div>
      <article className="sheet space-y-6 p-5 sm:p-8">
        <header>
          <p className="label !text-stamp">For viewers</p>
          <h1 className="display mt-2 text-[2.2rem] leading-[1.05] sm:text-[2.8rem]">Who sees what when you watch.</h1>
          <p className="mt-3 text-ink-soft">
            Someone shared their screen with you through PassStream. This page explains what the person who shared it (the host) and the server can see about you, why, how long it's kept, and what you can do about it.
          </p>
        </header>

        <Section title="The short version">
          <ul className="list-disc space-y-1.5 pl-5">
            <li>
              Streams from the host's PassStream app are end-to-end encrypted, so the server relays the screen without being able to read it. This encryption is in <strong className="text-ink">beta</strong> until an outside security review, and it protects you only while the page code your browser runs can be trusted (see “Encryption, and its limits” below).
            </li>
            <li>Some streams are encrypted only on the way to and from the server, not end to end. They're labelled that way, and the server can see their content.</li>
            <li>The PIN you type is checked by the host's own computer. The server never receives it.</li>
            <li>While you watch, the host sees a row for you: the name you typed (if any), your device, an approximate place, your connection quality and how long you've watched.</li>
            <li>Your IP address is kept only for your session unless the host turned on longer retention.</li>
          </ul>
        </Section>

        <Section title="What is seen, by whom, and why">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[30rem] text-[0.86rem]">
              <thead>
                <tr className="border-b border-paper-dark text-left">
                  <th className="label py-2 pr-3">What</th>
                  <th className="label py-2 pr-3">Who sees it</th>
                  <th className="label py-2">Why</th>
                </tr>
              </thead>
              <tbody>
                <Row what="Name you type" who="Host. Shown as “viewer-reported”: it isn't verified." why="So the host knows who is watching. Optional." />
                <Row what="Row code (e.g. K7P · purple)" who="You and the host" why="So the host can check a row is really you by asking for your code." />
                <Row what="IP address" who="Server and host, during your session" why="To show where a connection comes from and to block abuse." />
                <Row what="Approximate place and network" who="Host (city, country, network name from your IP)" why="To help the host recognise viewers. It may show a VPN or mobile network, not where you are." />
                <Row what="Device and browser" who="Host (e.g. “iPhone · Safari”)" why="To tell viewers apart and help with playback problems." />
                <Row what="Connection quality" who="Host (round-trip time, loss, freezes, path)" why="To spot and fix bad playback." />
                <Row what="Whether your tab is hidden" who="Host" why="To show whether you're actually watching. Reported by your browser." />
                <Row what="Join and leave times" who="Host and server" why="To show who is watching now and who watched." />
                <Row what="The screen itself and audio" who="You, the host and other admitted viewers" why="Shared with admitted viewers and encrypted end to end." />
                <Row what="The PIN" who="You and the host" why="The host's computer checks it; it never reaches the server." />
                <Row what="Stream size, timing and layer metadata" who="Server" why="Needed to relay and pace the video: how big each packet is, when it's sent, which quality layer it belongs to and whether it's a keyframe." />
              </tbody>
            </table>
          </div>
        </Section>

        <Section title="How long it's kept">
          <p>
            <strong className="text-ink">Raw IP addresses</strong> are kept only for your session, plus up to five minutes after you leave so the host can still block a connection that just left. Then they're removed. The host can opt in to keeping IPs in the stream's history; if they did, it's kept as long as the history below.
          </p>
          <p>
            <strong className="text-ink">Stream history</strong> (your row code, device, city and country, and when you joined and left) is kept for the stream's lifetime and then for as long as the host keeps the stream listed.
          </p>
          <p>
            <strong className="text-ink">The server's audit log</strong> records the host's actions (for example, removing a viewer) by internal id, not by your name or IP. It's kept for 30 days by default.
          </p>
          <p>Your name is remembered on your own device (in this browser's storage) so you don't retype it. Clear it by emptying the name field before you join.</p>
        </Section>

        <Section title="Encryption, and its limits">
          <p>
            <strong className="text-ink">End-to-end encryption is in beta.</strong> It hasn't had an outside security review yet. Until it has, treat it as a strong extra layer, not a guarantee.
          </p>
          <p>
            <strong className="text-ink">It's only as trustworthy as the page code.</strong> Your browser decrypts the stream with JavaScript that this page loads from the server. If that code were changed, for example by someone who controls the server, it could read the screen or the PIN and send them elsewhere. End-to-end encryption protects you from a server that relays honestly but looks at what passes through; it can't protect you from compromised page code.
          </p>
          <p>
            <strong className="text-ink">No unencrypted fallback.</strong> Browser streams use end-to-end encryption. If your browser lacks the support it needs to decrypt, playback doesn't start; encryption isn't weakened.
          </p>
          <p>
            <strong className="text-ink">What the server can always see</strong>, even on end-to-end encrypted streams: your IP address and the device and browser you use; when you connect and leave; how much data flows and when; the size and timing of each video packet; and the layer metadata it needs to relay the video (quality layer, keyframes, frame boundaries).
          </p>
        </Section>

        <Section title="What isn't private">
          <p>
            The host and anyone else they let in can see the screen and could record it, like any screen share. The name you type is visible to the host and isn't checked.
          </p>
        </Section>

        <Section title="Your rights">
          <p>
            The host who sent you the link decides how this stream is used and is responsible for it. Under data-protection law such as the GDPR you can ask them what they hold about you, to correct or delete it, or object to its use. Ask them directly; they control the server that stores the history.
          </p>
          <p>
            You don't have to give a name, and you can leave at any time by closing the page. If you think a stream is being misused, contact the host or the operator of the server it runs on.
          </p>
        </Section>

        <Section title="Location data">
          <p className="text-[0.86rem]">IP geolocation by DB-IP (db-ip.com), DB-IP Lite City and ASN, licensed CC BY 4.0. It's an estimate and is often wrong.</p>
        </Section>
      </article>
      <p className="mt-6 text-center text-[0.82rem] text-ink-mute">
        <button type="button" className="underline" onClick={() => history.back()}>Back</button>
      </p>
    </main>
  );
}
