#!/usr/bin/env bash
# Runs the viewer UX Playwright suite (web/e2e/*.ux.ts) in Linux WebKit.
# Playwright's WebKit build 2359 targets Ubuntu 24.04 and needs libraries an
# Arch host lacks (ICU 74, flite, libxml2.so.2), so it runs in the Playwright
# Ubuntu image: unprivileged, host network (for the Vite dev server it starts
# on 127.0.0.1), this checkout mounted at the same path, the WebKit build
# mounted read-only. Hold the media lock:
#
#   /tmp/passstream-briefs/media-run scripts/viewer-webkit.sh [playwright args]
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
wk=${PW_WEBKIT_DIR:-$HOME/.cache/ms-playwright/webkit-2359}
image=${PW_IMAGE:-mcr.microsoft.com/playwright:v1.63.0-noble}
exec docker run --rm --network host --init \
  --user "$(id -u):$(id -g)" -e HOME=/tmp -e CI=1 \
  -e PW_WEBKIT=/wk/pw_run.sh -e PW_DEV_PORT="${PW_DEV_PORT:-5182}" \
  -v "$root:$root" -v "$wk:/wk:ro" -w "$root/web" \
  "$image" npx playwright test -c playwright.viewer.config.ts "$@"
