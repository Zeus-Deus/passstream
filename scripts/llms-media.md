## Media transport and reliability

The join and its reply are in §8.1 below. Transport fields on top of it: `session` (a transport
resumption token, never an E2EE admission) and `replace: true` (discard the session's existing PC, inheriting its applied path and native-key budget);
the reply adds `session`, `resumed` and `iceServers`. Transport session grants expire in 24 h and
survive a server restart. When `resumed` is false, create a new PC. A media peer may wait up to two
minutes for its client's first offer (E2EE clients offer once admission or capture starts); after that
an unanswered peer is reaped.
Client sends {type:"offer",sdp:{type:"offer",sdp}};
server replies {type:"answer",sdp}. Both trickle {type:"candidate",candidate}.
Repeat offer/answer for a serialized renegotiation (browsers replace the PC instead of restarting ICE). Signaling reconnect does not interrupt healthy media.
{type:"ping",freshFrames?,at?} -> {type:"pong",refreshMs} measures signaling responsiveness (clients send it every 500ms).
Viewer freshFrames/at feed ReportDecoded with the client sample timestamp without renewing transport liveness; publisher samples are ignored. refreshMs is RefreshTransitMS(), a queue advisory bounded to 6000ms, zero when idle or publishing.
{type:"leave"} closes the PC. One publisher is fenced per stream; a new publisher replaces it.
Data channels: reliable control text echo; unordered heartbeat (maxRetransmits=0) every 500ms. Messages are bounded to 4096 bytes, with a five-message/second token rate and two-message burst per peer. Every non-join native-key cause shares one request per ten seconds per viewer; recovery prefers the independent low rendition.
Heartbeat replies include selected spatial and temporal layers, forwarding counters, ingress diagnostics, and repair
diagnostics under repair for viewers {fecPackets,pacerDrops,targetBitrate,estimate,queueDelayMs,layerDowns,
congestion,replay,repairBytes,wireBytes,wireBudget,baseWireDemand,initialRTTMs,suppressedRepair,keyframeBytes,sourceKeyframeBytes,twccRTTMs,decoderReported,freshFrames,freshAgeMs,mediaRates,fullLayerTimestamps,catchupMs,receivedWireRate,provenWireRate}. These are diagnostic, not a stable schema. Publisher heartbeat repair diagnostics also include
sourceKeyframeBytes and delay/capacity-control measurements. A {type:"heartbeat",diagnostics:true}
request includes sourceFrameTimes (the latest 256 primary-frame completion times at ingress, Unix milliseconds),
so source encoding gaps can be separated from SFU forwarding stalls. Normal unreliable replies keep
bounded diagnostics within one datagram; full history is opt-in. A {type:"keyframe"}
heartbeat request is coalesced with PLI/FIR. Publisher replacement fences the previous PC.
Viewer heartbeats may include freshFrames: a monotonically advancing count of decoded frames with
advancing requestVideoFrameCallback presentedFrames/RTP timestamps and recent receiveTime; at is the
client sample timestamp used for decoded-fps deltas, independently of delayed heartbeat arrival. A remapped
captureTime is not treated as a decoder-stall signal. No pixels or test-source clock are needed. Zero does
not establish decoder reporting. This untrusted diagnostic helps distinguish repaired loss from stalled video;
it does not authorize a viewer or bypass replacement re-admission.
Audio: Opus only (no RED), one track per PC. The SFU forwards it opaquely (never parses the payload),
through a bounded per-viewer queue (50 packets, oldest dropped; >200ms old dropped at the pacer), sent ahead
of video from the same wire budget; video layer admission subtracts the audio rate. Offers may add
a=rtcp-fb:<opus PT> nack; audio NACKs are served from a 128-packet cache. Viewer heartbeats add
repair.audio {forwarded,dropped,replayed,paced,late,ingress}. Audio wire bytes are counted separately and excluded from video wire costs, received proof and trial accounting.
VP9 profile 0 only, non-flexible L1T3 requested; active video offers without it get error code unsupported_codec and no negotiation. VP8 is refused. A publisher may negotiate a second send-only video transceiver
with an independent 320x180, 80-100kbit/s L1T3 rendition for captures at least 960 pixels wide.
It shares capture lifetime. Native video is SFrame track 0, audio track 1 and the independent 320x180 thumbnail track 2 (KID=epoch*16+2), with separate counters on one worker. Both video transforms attach before their tracks; the switched receiver accepts video tracks {0,2} with replay windows per KID. Rekey requests keyframes on both video encoders.
Publishers negotiate REMB and absolute-send-time; viewer transport feedback remains TWCC.
The receiver bounds publisher capacity using sustained growth in completed 100ms transit-delay
buckets and measured received wire rate, up to 15Mbit/s. Production text capture uses the default
4Mbit/s cap and L1T3. Encoding caps remain fixed, including during keyframe recovery; no sender bitrate-boost path exists. Repaired random loss and fixed propagation-delay steps
do not themselves cut capacity. Chromium VP9's transport floor is 128kbit/s, includes repair
demand, and is independent of downstream viewer budgets. Missing send clocks use that floor.
The first video receiver is native, the second is low; further video receivers are ignored.
Viewers start low when native key cost is unknown or exceeds one second at the conservative 256kbit/s seed. A constrained
viewer switches low when its budget cannot serialize a native keyframe/catch-up within one second.
Native admission proves steady media demand and a keyframe that serializes within one second at the received video rate. The same envelope checks the actual arriving key. Dependent pictures remain bounded and held while an admitted key drains; RTP descriptor fields alone identify picture dependencies.
One rendition hysteresis preserves a productive native decoder through one second of transient pressure. A proven upgrade may await its publisher key for at most two seconds; losing key capacity revokes it. Recovery prefers the independent low key. All non-join native-key causes, including explicit viewer requests, RTCP PLI/FIR, decoded-stall reports and upgrades, share a ten-second per-viewer budget. Joins coalesce separately; a low join requests native only after proving the upgrade.
One wire bucket charges original media, repair, replay and padding. Its idle credit is max(4 MTU, 100ms at the actual rate), with one packet of debt. Outside bounded trials the actual wire rate is at most 110% of proven video delivery. Video feedback flight uses physical RTT plus 100ms. Padding accounts for already outstanding round-trip work plus at most 200ms at proven rate within remaining forward-delay headroom. Audio has a separate accounting path and cannot prove video capacity.
Padding trials last 750ms, have at least two seconds between starts, and double failed-trial waits up to 15s. Failure uses actual trial loss or a completed 500ms trial-only delivery window; an aborted unsampled experiment neither erases nor compounds previous backoff. Local frame assembly cannot cancel a trial. Discovery allows one full 750ms trial (at least 256KiB), then at most one padding byte per four video wire bytes. Padding never represents useful decoded media, and video pacing cannot borrow the exploratory padding rate.
Layer selection uses measured delta rates, a 10% margin and measured repair reservation. Unknown low paths use the 256kbit/s seed between bounded trials; encoded low video cannot borrow exploratory padding rate. Replacements inherit their viewer's actual applied wire budget and native-key request budget. A native-to-low switch revokes capacity proof and fences old native receipts without rebasing physical or forward delay. One rate decision combines a time-normalized forward-delay trend (25ms/s), a 300ms standing-excess cap on the same forward clock, loss and one delivered/decoded reservation. At standing excess only actual current full-source delivery and decoding can preserve that reservation; decoder claims alone cannot. Congestion confirmation is 300ms; cuts are at most twice per second and cannot be undone by a floor or probe in the same tick. Numerical increases are at most 25% per 100ms, within measured demand, with two seconds after a cut; only actual wire trials have exponential backoff. Receiver/transport delay baselines reset on selected ICE-pair changes; transport minima use a 30s window.
Delta expiry remains 100ms plus at most six seconds of key serialization/catch-up grace. Unstarted obsolete pictures yield to a new independent key. Media, parity and NACK repair remain in the same wire budget, with dependency-aware temporal switches and bounded replay deadlines. Proactive parity requires reserved wire headroom; inexpensive nearby repair uses NACK. Transport feedback may request bounded cached repair, never recursive repair of replay/parity/padding.
Viewers count as dead (deltas paused) after max(1s, 4xRTT) without RTCP since connecting, 2s before RTT is known.
Upstream NACK: 11 attempts in 20ms slots within 300ms. A known disposable TID2 gap can be
superseded by a newer picture. Non-flexible TID1 damage suppresses dependent enhancements
until an intact base and TID1 switching point; base/keyframe damage still requires an independent
keyframe. Downstream replay: 6 attempts within 1s for ordinary packets or 6s for current-keyframe packets, retries spaced by max(30ms, RTT), or 100ms before RTT is known, with deadline ordering against media expiry in the same wire budget, never
for parity or packets before the latest keyframe; suppress replay that cannot arrive by its deadline.
Viewer recovery also watches decoded video while the publisher keeps producing frames.
The torture source independently checks pixel timestamps for glass-to-glass latency. The decoded-stall deadline uses max(1s, 4x initial RTT), so congestion-inflated RTT cannot postpone it. Idle/hidden screens do not trigger this clock. Repeated replacements back off 6-30s until 3s of fresh decoded video.
New viewers share a pending keyframe request at 500ms room coalescing after transport connection.
Room source-repair PLIs can repeat after 300ms; viewer-caused native requests also require their per-viewer budget. Unanswered room requests normally wait one second.
A measured fast publisher whose key serializes within 62.5ms may retry after max(300ms, RTT +100ms
+ serialization), capped at one second. Joins still coalesce for 500ms; in-flight keyframes
remain protected. Local holdoff is serialization plus 2xRTT.
Queued keyframes and dependent catch-up suppress redundant requests. Temporal upgrades use VP9 switching points and do not request another key.
Incoming media after a one-second gap requests a fresh capture. An independent keyframe can skip
obsolete upstream repair gaps while preserving its packets that arrived out of order. A transport-feedback
gap bypasses that holdoff briefly, including after feedback resumes. A paced decoder stall does not.
Complete lower spatial pictures survive a missing highest-layer marker. TL0PICIDX detects a wholly lost
upstream base picture, so it requests repair without waiting for the viewer watchdog.
Viewers can negotiate video/red + video/ulpfec; tight budgets disable proactive parity.
Viewers use 50ms playout on clean/reordering-only links, 150-300ms after repeated repair freezes,
and an RTT-scaled 150-300ms proactively when measured loss coincides with NACK repair. Ample viewer budgets
use one parity per media packet on measured Wi-Fi loss, still inside the shared wire budget. Brief heartbeat loss evidence is latched for 5s between stats observations. Target increases are capped at 100ms per 500ms stats observation to avoid a buffer-fill freeze. Quiet paths return to 50ms.
A deliberate PC replacement has one immediate signaling retry; all failed rejoins use jittered backoff.
A browser may defer decoded-stall replacement during actual queued keyframe catch-up for at most 6s; latency/freshness measurements still include that entire cost.
The server admits forwarding only after the replacement join has been accepted.
The browser watchdog uses inbound bytes and selected-pair consent in addition to heartbeats; a stall is
1s (or 4xRTT). It recovers by replacing the PC (join with replace:true), not by ICE restart: at once if
signaling answered pings throughout the stall, 1s after signaling returns from a total outage if media
is still stalled, and after 6s without progress regardless.
TURN permissions and relay sockets permit only the configured SFU media endpoint.
ICE UDP mux and passive ICE TCP, authenticated embedded TURN UDP/TCP/TLS. Public mode separates bind and advertised addresses; relay sockets on 127.0.0.2 can reach only this SFU (configuration and certificates: §3.1).

