# PassStream

Share your screen, window or browser tab through a private link. Viewers join in their browser, without an account or installation. PassStream is the live-streaming companion to PassPage and PassFile.

A single Go binary embeds the web app and WebRTC SFU. Media always passes through the server, with bounded queues for each viewer. The sender verifies PINs locally using CPace; media uses SFrame end-to-end encryption. Unsupported browsers fail closed.

**Branch: unfinished controller development.** This branch preserves the latest controller and simulator work. It is not approved for release. The last full comparison passed 24/37 properties; the latest correction still fails two selected regressions.

**Status: pre-release.** Encryption is labelled beta, and reliability testing is still in progress. Bad-network quality gates and physical mobile validation are not complete. This is a development snapshot, not a production-ready release.

## Build

Install [mise](https://mise.jdx.dev/), then:

```sh
mise install
mise exec -- npm --prefix e2ee ci
mise exec -- npm --prefix web ci
mise exec -- npm --prefix web run build
mkdir -p build
CGO_ENABLED=0 mise exec -- go build -trimpath -o build/passstream ./cmd/passstream
```

The web bundle is embedded at build time. Rebuild the Go binary after changing it.

## Run locally

```sh
./build/passstream serve --data ./data --listen 127.0.0.1:8080 --base-url http://127.0.0.1:8080
```

Open <http://127.0.0.1:8080> and sign in using the owner key in `data/owner-key`. Keep this file private. The dashboard never displays stream PINs or media keys.

For the local sender, put `build/passstream` on your PATH. Configure the server URL in `~/.config/passstream/config` and place a scoped API key in `~/.config/passstream/key` with mode `0600`:

```ini
server = http://127.0.0.1:8080
```

```sh
passstream share --pin 4
passstream copy link
passstream pin --reveal
passstream pause
passstream resume
passstream stop
```

`--pin 4` chooses a four-digit PIN policy. Pick a screen, window or tab in the browser picker. Send the link and PIN separately. Disabling the PIN requires an explicit `--pin none`.

## What is included

- Browser sharing, reconnect handling and per-viewer media forwarding.
- Sender-verified PINs, session grants, knock approval, lock and kick controls.
- A dashboard, local sender daemon, CLI and agent API.
- Privacy curtain, quality HUD, encrypted annotations, local snapshots and recording.

The current media path targets Chromium and VP9. Browser support depends on insertable-stream encryption support. Native hardware capture, system audio and cursor metadata are planned for v2.

## Security

The PIN and media keys stay with the sender and admitted viewers. This assumes trusted client code: a compromised web server can replace browser JavaScript, and admitted viewers can record what they see. See the [protocol](docs/PROTOCOL.md) and [host security model](docs/HOST-CONTROL.md).

## Documentation

- [Public hosting and TLS](docs/HOSTING.md)
- [CLI, API and agent contract](docs/CONTRACT.md)
- [Host setup and controls](docs/HOST-CONTROL.md)
- [Features](docs/FEATURES.md)

The running server also exposes `/llms.txt` and `/skill.md`.

## Development

```sh
mise exec -- go test -race ./...
mise exec -- go vet ./...
mise exec -- npm --prefix e2ee test
mise exec -- npm --prefix web test
mise exec -- npm --prefix e2ee run typecheck
mise exec -- npm --prefix web run typecheck
python3 scripts/generate_llms.py --check
```

Browser and network tests are under `test/`. Network torture tests require Chromium, Linux network namespaces and `tc netem`. Generated results, captures, runtime state and local development history are excluded from this public source snapshot.
