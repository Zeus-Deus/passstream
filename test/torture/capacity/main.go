// Synthetic transport-capacity probe. Payloads exercise VP9 packet routing, not
// decoding; server resources are sampled from a separate process.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type signal struct {
	Protocol   int                        `json:"protocol,omitempty"`
	SenderKind string                     `json:"sender_kind,omitempty"`
	Instance   string                     `json:"instance,omitempty"`
	Type       string                     `json:"type"`
	Role       string                     `json:"role,omitempty"`
	ID         string                     `json:"id,omitempty"`
	Credential string                     `json:"credential,omitempty"`
	SDP        *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate  *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}
type stream struct {
	ID      string `json:"id"`
	URL     string `json:"view_link_base"`
	Publish string `json:"publish_token"`
}
type receiver struct{ frames, packets, last, maxGap atomic.Int64 }

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func connect(ctx context.Context, s stream, publish bool, r *receiver) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP) {
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	pc, e := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	must(e)
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, e := t.ReadRTP()
			if e != nil {
				return
			}
			r.packets.Add(1)
			if p.Marker {
				now := time.Now().UnixNano()
				last := r.last.Swap(now)
				if last != 0 {
					gap := now - last
					for old := r.maxGap.Load(); gap > old; old = r.maxGap.Load() {
						if r.maxGap.CompareAndSwap(old, gap) {
							break
						}
					}
				}
				r.frames.Add(1)
			}
		}
	})
	var track *webrtc.TrackLocalStaticRTP
	key := &atomic.Bool{}
	key.Store(true)
	if publish {
		track, e = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"}, "screen", "synthetic")
		must(e)
		sender, e := pc.AddTrack(track)
		must(e)
		go func() {
			for {
				pkts, _, e := sender.ReadRTCP()
				if e != nil {
					return
				}
				for _, p := range pkts {
					if _, ok := p.(*rtcp.PictureLossIndication); ok {
						key.Store(true)
					}
				}
			}
		}()
	} else {
		tr, e := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		must(e)
		must(tr.SetCodecPreferences([]webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0", RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"}}}, PayloadType: 98}}))
	}
	dc, e := pc.CreateDataChannel("heartbeat", nil)
	must(e)
	dc.OnOpen(func() {
		go func() {
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_ = dc.SendText(`{"type":"heartbeat"}`)
				}
			}
		}()
	})
	c, _, e := websocket.Dial(ctx, "ws://127.0.0.1:18080/signal", nil)
	must(e)
	role, cred := "view", strings.Split(s.URL, "#c=")[1]
	if publish {
		role = "publish"
		cred = s.Publish
	}
	must(wsjson.Write(ctx, c, signal{Type: "join", Protocol: 1, SenderKind: "browser", Instance: "AAAAAAAAAAAAAAAAAAAAAA", ID: s.ID, Role: role, Credential: cred}))
	var joined signal
	must(wsjson.Read(ctx, c, &joined))
	gather := webrtc.GatheringCompletePromise(pc)
	offer, e := pc.CreateOffer(nil)
	must(e)
	must(pc.SetLocalDescription(offer))
	<-gather
	must(wsjson.Write(ctx, c, signal{Type: "offer", SDP: pc.LocalDescription()}))
	var candidates []webrtc.ICECandidateInit
	for {
		var msg signal
		must(wsjson.Read(ctx, c, &msg))
		if msg.Candidate != nil {
			candidates = append(candidates, *msg.Candidate)
		}
		if msg.SDP != nil {
			must(pc.SetRemoteDescription(*msg.SDP))
			break
		}
	}
	for _, v := range candidates {
		must(pc.AddICECandidate(v))
	}
	go func() {
		defer c.CloseNow()
		for {
			var msg signal
			if wsjson.Read(ctx, c, &msg) != nil {
				return
			}
			if msg.Candidate != nil {
				_ = pc.AddICECandidate(*msg.Candidate)
			}
		}
	}()
	if publish {
		go func() {
			tick := time.NewTicker(time.Second / 15)
			defer tick.Stop()
			var seq uint16
			var ts uint32
			var frame byte
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					ts += 6000
					frame++
					k := key.Swap(false)
					for i := 0; i < 3; i++ {
						payload := make([]byte, 720)
						payload[0] = 0x60
						if k {
							payload[0] &^= 0x40
						}
						if i == 0 {
							payload[0] |= 8
						}
						if i == 2 {
							payload[0] |= 4
						}
						payload[1] = 0x10
						payload[2] = frame
						seq++
						_ = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: i == 2}, Payload: payload})
					}
				}
			}
		}()
	}
	return pc, track
}
func usage(pid int) (float64, int64) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	must(e)
	fields := strings.Fields(string(b))
	a, _ := strconv.ParseInt(fields[13], 10, 64)
	c, _ := strconv.ParseInt(fields[14], 10, 64)
	b, e = os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	must(e)
	var rss int64
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rss, _ = strconv.ParseInt(strings.Fields(line)[1], 10, 64)
		}
	}
	return float64(a+c) / 100, rss
}
func run() bool {
	viewers := flag.Int("viewers", 128, "total receivers")
	streams := flag.Int("streams", 1, "independent publishers")
	seconds := flag.Int("seconds", 12, "observation seconds")
	warmup := flag.Duration("warmup", 3*time.Second, "settling time after all peers connect")
	bin := flag.String("server", "./build/passstream", "server binary")
	out := flag.String("out", "/tmp/capacity.json", "JSON output")
	flag.Parse()
	dir, e := os.MkdirTemp("", "ps-capacity-")
	must(e)
	server := exec.Command(*bin, "serve", "--data", dir, "--listen", "127.0.0.1:18080", "--base-url", "http://127.0.0.1:18080", "--ice-udp", "127.0.0.1:18443", "--ice-tcp", "127.0.0.1:18443", "--turn", "127.0.0.1:13478")
	server.Stderr = os.Stderr
	must(server.Start())
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	for i := 0; i < 200; i++ {
		res, e := http.Get("http://127.0.0.1:18080/readyz")
		if e == nil {
			res.Body.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	owner, e := os.ReadFile(filepath.Join(dir, "owner-key"))
	must(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ss []stream
	var pcs []*webrtc.PeerConnection
	for i := 0; i < *streams; i++ {
		req, e := http.NewRequest("POST", "http://127.0.0.1:18080/api/v1/streams", nil)
		must(e)
		req.Header.Set("Authorization", "Bearer "+string(owner))
		res, e := http.DefaultClient.Do(req)
		must(e)
		var s stream
		must(json.NewDecoder(res.Body).Decode(&s))
		res.Body.Close()
		ss = append(ss, s)
		pc, _ := connect(ctx, s, true, &receiver{})
		pcs = append(pcs, pc)
	}
	receivers := make([]*receiver, *viewers)
	for i := range receivers {
		receivers[i] = &receiver{}
		pc, _ := connect(ctx, ss[i%len(ss)], false, receivers[i])
		pcs = append(pcs, pc)
	}
	defer func() {
		cancel()
		for _, p := range pcs {
			_ = p.Close()
		}
	}()
	time.Sleep(*warmup)

	for _, r := range receivers {
		r.frames.Store(0)
		r.packets.Store(0)
		r.maxGap.Store(0)
	}
	load0, _ := os.ReadFile("/proc/loadavg")
	var profileDone chan error
	if os.Getenv("PASSSTREAM_PROFILE") == "1" {
		profileDone = make(chan error, 1)
		go func() {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:18082/debug/pprof/profile?seconds=%d", *seconds))
			if err != nil {
				profileDone <- err
				return
			}
			defer resp.Body.Close()
			f, err := os.Create(*out + ".pprof")
			if err != nil {
				profileDone <- err
				return
			}
			defer f.Close()
			_, err = io.Copy(f, resp.Body)
			profileDone <- err
		}()
	}
	cpu0, _ := usage(server.Process.Pid)
	from := time.Now()
	peak := int64(0)
	loadSamples := []string{}
	for time.Since(from) < time.Duration(*seconds)*time.Second {
		_, rss := usage(server.Process.Pid)
		peak = max(peak, rss)
		load, _ := os.ReadFile("/proc/loadavg")
		loadSamples = append(loadSamples, strings.TrimSpace(string(load)))
		time.Sleep(250 * time.Millisecond)
	}
	cpu1, _ := usage(server.Process.Pid)
	elapsed := time.Since(from).Seconds()
	cpu := (cpu1 - cpu0) / elapsed * 100
	if profileDone != nil {
		must(<-profileDone)
	}
	load1, _ := os.ReadFile("/proc/loadavg")
	rows := []map[string]any{}
	pass := true
	for _, r := range receivers {
		age := float64(time.Now().UnixNano()-r.last.Load()) / 1e6
		gap := float64(r.maxGap.Load()) / 1e6
		frames := r.frames.Load()
		pass = pass && frames > int64(float64(*seconds)*10) && age < 500 && gap < 500
		rows = append(rows, map[string]any{"frames": frames, "packets": r.packets.Load(), "max_gap_ms": gap, "last_frame_age_ms": age})
	}
	binaryBytes, e := os.ReadFile(*bin)
	must(e)
	revision, e := exec.Command("git", "rev-parse", "HEAD").Output()
	must(e)
	result := map[string]any{"load_start": strings.TrimSpace(string(load0)), "load_end": strings.TrimSpace(string(load1)), "binary_sha256": fmt.Sprintf("%x", sha256.Sum256(binaryBytes)), "commit": strings.TrimSpace(string(revision)), "created": time.Now().Format(time.RFC3339), "viewers": *viewers, "streams": *streams, "observation_s": elapsed, "server_cpu_pct": cpu, "server_rss_peak_mib": float64(peak) / 1024, "cpu_per_viewer": cpu / float64(*viewers), "rss_mib_per_viewer": float64(peak) / 1024 / float64(*viewers), "metrics": rows, "pass": pass, "workload": "synthetic VP9 RTP 259.2 kbit/s, 15 fps, no decoding, one publisher per stream; CPU excludes clients"}
	result["warmup_s"] = warmup.Seconds()
	result["load_samples"] = loadSamples
	b, e := json.MarshalIndent(result, "", "  ")
	must(e)
	must(os.WriteFile(*out, b, 0600))
	fmt.Println(string(b))
	return pass
}

func main() {
	if !run() {
		os.Exit(1)
	}
}
