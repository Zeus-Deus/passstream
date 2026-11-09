#!/usr/bin/env bash
# Runs the Omarchy plugin's own test suite against the real host daemon.
#
# The plugin's tests start `python3 -I tests/mock-daemon.py …`. This copies
# the plugin to a temp dir and replaces that file with a shim that execs
# fakehost: a fixture that starts the shipped `passstream serve` and
# `passstream host` commands on loopback, in temporary XDG directories.
# A simulated sender speaks the real HMAC/local IPC and server signaling.
# A bounded IPC proxy records requests and supplies shell-environment fixtures
# (socket-activation metadata, status delay, foreign port). The API is real.
# Nothing in the plugin repo or the user's XDG state is modified.
#
#   test/plugin/run.sh [PLUGIN_DIR] [-- node test args]
#   PASSSTREAM_NATIVE_TESTS=1 test/plugin/run.sh   # also Service.qml and the panel
set -euo pipefail
here=$(cd "$(dirname "$0")/../.." && pwd)
plugin=${1:-$here/../passstream-omarchy-plugin}
[ -d "$plugin" ] || { echo "Plugin checkout not found; pass PLUGIN_DIR." >&2; exit 1; }
shift || true
[ "${1:-}" = "--" ] && shift
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(cd "$here" && mise exec -- go build -o "$work/fakehost" ./internal/host/hosttest/fakehost)
(cd "$here" && CGO_ENABLED=0 mise exec -- go build -o "$work/passstream" ./cmd/passstream)
mkdir -p "$work/plugin"
(cd "$plugin" && tar --exclude=.git --exclude=tests/artifacts -cf - .) | tar -C "$work/plugin" -xf -
cat > "$work/plugin/tests/mock-daemon.py" <<PY
import os, sys
os.execv("$work/fakehost", ["$work/fakehost", "--real-server-bin", "$work/passstream"] + sys.argv[1:])
PY
node=$(cd "$here" && mise which node)
cd "$work/plugin"
if [ $# -eq 0 ]; then set -- tests/*.test.js; fi
PASSSTREAM_REAL_DAEMON=1 "$node" --test --test-concurrency=2 "$@"
