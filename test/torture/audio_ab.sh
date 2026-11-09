#!/bin/sh
# Bad-Wi-Fi A/B on one binary: audio m-lines present (TORTURE_AUDIO=mline,
# the silent transceivers of fix round 1) against absent (none). Each pair
# shares a random netem seed and runs both arms at the same time, each in its
# own network namespace, so host load and the loss/jitter PRNG are matched;
# which arm launches first is random. Results:
# test/torture/results/<PREFIX>-<pair>-<arm>.json.
#
#   BIN=build/passstream-x PREFIX=ab PAIRS=4 [FIRST=1] test/torture/audio_ab.sh
set -eu
: "${BIN:?}" "${PREFIX:?}"
PAIRS=${PAIRS:-4}
mkdir -p test/torture/out
i=${FIRST:-1}
PAIRS=$((i + PAIRS - 1))
while [ "$i" -le "$PAIRS" ]; do
	seed=$(od -An -N4 -tu4 /dev/urandom | tr -d ' ')
	coin=$(od -An -N1 -tu1 /dev/urandom | tr -d ' ')
	if [ $((coin % 2)) -eq 0 ]; then order="mline none"; else order="none mline"; fi
	echo "pair $i seed $seed order $order"
	for arm in $order; do
		TORTURE_RUN="$PREFIX-$i-$arm" TORTURE_AUDIO="$arm" TORTURE_NETEM_SEED="$seed" \
			TORTURE_FILTER='^bad_wifi$' TORTURE_WIFI_SECONDS="${SECONDS_PER_RUN:-300}" TORTURE_SERVER_BIN="$BIN" \
			nice -n 10 unshare -rn sh -ec 'ip link set lo up mtu 1500; mise exec -- node web/torture.mjs' \
			>"test/torture/out/$PREFIX-$i-$arm.log" 2>&1 &
		sleep 2
	done
	wait
	i=$((i + 1))
done
