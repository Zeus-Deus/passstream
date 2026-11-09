#!/bin/sh
set -eu
# Run inside an unprivileged network namespace with lo up and MTU 1500.
# Usage: run.sh SERVER_BINARY RUN_PREFIX [MAX_VIEWERS]
server=$1
prefix=$2
limit=${3:-512}
for mode in viewers streams; do
  for n in 16 64 128 256 512 1024; do
    [ "$n" -le "$limit" ] || break
    streams=1
    [ "$mode" = viewers ] || streams=$n
    ./build/capacity -server "$server" -viewers "$n" -streams "$streams" -seconds 12 -out "test/torture/results/capacity/$prefix-$mode-$n.json" || break
  done
done
