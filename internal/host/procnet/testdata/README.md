# Repeated procfs socket observations

`repeated-peer.tcp` contains the matching rows captured inside one failed
`PeerUID` lookup on 2026-11-09 at approximately 01:13 CET. The source was
`/proc/net/tcp`, under test-owned loopback TCP churn and real headless Chromium.
The original lookup counted three matching rows and returned UID -1 with
`3 matching sockets`.

All three rows describe client `127.0.0.1:46010` to server
`127.0.0.1:42279`, state `01`, UID `1000`, inode `187591862`. Their slot numbers
and timer values differ. These rows came from the failing lookup's own read,
not a later snapshot. The fixture proves repeated observations; it does not
identify the kernel traversal mechanism or prove the identity of the original
controller failure, which did not capture inodes.

`TestPeerUIDRepeatedObservation` fails against the original implementation and
passes when fully agreeing, nonzero socket identities are counted once. The
other tests retain failure for distinct sockets, contradictory owners and
zero-inode duplicates. Browser stress belongs in a separate coordinated run
under `/tmp/passstream-media.lock`, because host TCP churn can interfere with
parallel tests' fixed ports. The normal Chromium test still runs and checks
both windows' page loads and WebSocket requests without retrying a lookup.
