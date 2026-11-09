package sfusim

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// TestWifiCalibration compares browser bad_wifi runs on the in-order spec
// (GATES line 108) with the model on the same controller (ctrl.Default,
// not a replay): clock source, audio, one viewer, both directions through
// one netem qdisc, established and prejoin histories, the browser's own
// observation windows. Diagnostic: it reports and never fails.
//
//	SFUSIM_WIFI_CALIB=test/torture/results/<run>.json go test -run TestWifiCalibration -v ./internal/sfusim
func TestWifiCalibration(t *testing.T) {
	path := os.Getenv("SFUSIM_WIFI_CALIB")
	if path == "" {
		t.Skip("set SFUSIM_WIFI_CALIB to a browser torture result JSON with bad_wifi rows")
	}
	runs := loadWifiRuns(t, path)
	seeds := 20
	if v := os.Getenv("SFUSIM_JB_CAP"); v != "" {
		// Sweep of the receiver's jitter-estimate cap (calibration only).
		d, e := time.ParseDuration(v)
		if e != nil {
			t.Fatal(e)
		}
		saved := jitterCap
		jitterCap = d
		defer func() { jitterCap = saved }()
	}
	if v := os.Getenv("SFUSIM_CLOCK_ENDPOINT"); v != "" {
		d, e := time.ParseDuration(v)
		if e != nil {
			t.Fatal(e)
		}
		saved := Clock.Endpoint
		Clock.Endpoint = d
		defer func() { Clock.Endpoint = saved }()
	}
	var b strings.Builder
	header := "| Run | History | Window s | Frames | G2G p50 / p95 / p99 ms | Freezes >500 ms (per 300 s) | Gaps >2 s | Max gap ms | Freeze causes (key / source / replace / delivery) | PC replacements | SFU averageLoss / model packet loss (run, pkts) | Residual video loss | Video NACK pkts per 1000 video pkts | Audio missing % / NACK pkts per 1000 / resent per 1000 | Playout target ms | Jitter buffer ms | Samples native T2 / T1 / T0 / low % |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n"
	b.WriteString(header)
	var pool [2]wifiStat // browser, model
	for i, br := range runs {
		bs := br.stat()
		pool[0].add(bs)
		b.WriteString(bs.row(fmt.Sprintf("browser %d", i+1), br.History))
		var ms wifiStat
		for seed := 1; seed <= seeds; seed++ {
			sc, w := br.scenario()
			r := Run(sc, uint64(1000*i+seed), Default)
			ms.add(modelStat(r, w))
		}
		ms.scale(1 / float64(seeds))
		pool[1].add(ms)
		b.WriteString(ms.row(fmt.Sprintf("model (mean of %d)", seeds), br.History))
	}
	b.WriteString(pool[0].row("browser pooled", "both"))
	b.WriteString(pool[1].row("model pooled", "both"))
	t.Log("\n" + b.String())
	t.Logf("endpoint fit (GATES Source model method): browser G2G p5 %v − model network p5 %v = %v (Clock.Endpoint %v, jitter cap %v)",
		Pct(pool[0].g2g, .05), Pct(pool[1].network, .05), Pct(pool[0].g2g, .05)-Pct(pool[1].network, .05), Clock.Endpoint, jitterCap)
}

type wifiRun struct {
	History  string  `json:"history"`
	From     float64 `json:"from"`
	To       float64 `json:"to"`
	Feedback struct {
		Viewer struct {
			SSRCs []struct {
				Kind      string  `json:"kind"`
				Packets   float64 `json:"packets"`
				Lost      float64 `json:"lost"`
				NACK      float64 `json:"nack"`
				Concealed float64 `json:"concealed"`
				Freezes   float64 `json:"freezes"`
			} `json:"ssrcs"`
		} `json:"viewer"`
		SFU map[string]any `json:"sfu"`
	} `json:"feedback"`
	Viewers []struct {
		StartedAt float64 `json:"startedAt"`
		Timeline  []bFrame
		Events    []struct {
			At     float64 `json:"at"`
			Kind   string  `json:"kind"`
			Detail string  `json:"detail"`
		} `json:"events"`
		Samples []struct {
			Timestamp     float64  `json:"timestamp"`
			Layer         string   `json:"layer"`
			IngressFrames float64  `json:"ingressFrames"`
			PlayoutTarget *float64 `json:"playoutTarget"`
			Repair        struct {
				ForwardedKeys float64 `json:"forwardedKeys"`
				Estimate      struct {
					AverageLoss *float64 `json:"averageLoss"`
				} `json:"estimate"`
			} `json:"repair"`
			Stats map[string]any `json:"stats"`
		} `json:"samples"`
	} `json:"viewers"`
}

func loadWifiRuns(t *testing.T, path string) []*wifiRun {
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		if r, e = gzip.NewReader(f); e != nil {
			t.Fatal(e)
		}
	}
	var doc struct {
		Commit  string `json:"commit"`
		Results []struct {
			Name string `json:"name"`
			wifiRun
		} `json:"results"`
	}
	if e = json.NewDecoder(r).Decode(&doc); e != nil {
		t.Fatal(e)
	}
	t.Logf("browser run %s commit %s", path, doc.Commit)
	var out []*wifiRun
	for i := range doc.Results {
		// bad_wifi, bad_wifi_prejoin and their _repN repetitions (in-order spec);
		// bad_wifi_reorder and bad_wifi_both_legs carry no history field.
		if x := &doc.Results[i]; strings.HasPrefix(x.Name, "bad_wifi") && x.History != "" && len(x.Viewers) > 0 {
			out = append(out, &x.wifiRun)
		}
	}
	return out
}

// wifiStat is one run's (or a mean's) comparison metrics.
type wifiStat struct {
	n                                 float64
	seconds, frames, freezes, long    float64
	maxGap                            float64
	g2g                               []time.Duration
	network                           []time.Duration // model: G2G without the endpoint delay
	key, source, replace, delivery    float64
	replacements                      float64
	rawLoss, residual, nacks          float64
	runLen                            float64    // mean forward loss run, packets (model only)
	avgLoss                           float64    // SFU averageLoss mean
	layers                            [4]float64 // sample shares: native T2, T1, T0, low
	audioMissing, audioNACK, audioRTX float64
	playout, jitter                   float64
}

func (s *wifiStat) add(x wifiStat) {
	s.n += max(1, x.n)
	s.seconds += x.seconds
	s.frames += x.frames
	s.freezes += x.freezes
	s.long += x.long
	s.maxGap = max(s.maxGap, x.maxGap)
	s.g2g = append(s.g2g, x.g2g...)
	s.network = append(s.network, x.network...)
	s.key += x.key
	s.source += x.source
	s.replace += x.replace
	s.delivery += x.delivery
	s.replacements += x.replacements
	// rates are averaged per run
	s.rawLoss += x.rawLoss
	s.runLen += x.runLen
	s.avgLoss += x.avgLoss
	for i := range s.layers {
		s.layers[i] += x.layers[i]
	}
	s.residual += x.residual
	s.nacks += x.nacks
	s.audioMissing += x.audioMissing
	s.audioNACK += x.audioNACK
	s.audioRTX += x.audioRTX
	s.playout += x.playout
	s.jitter += x.jitter
}

// scale turns a sum of k runs into their mean (the G2G pool is kept whole).
func (s *wifiStat) scale(f float64) {
	for _, p := range []*float64{&s.seconds, &s.frames, &s.freezes, &s.long, &s.key, &s.source, &s.replace, &s.delivery, &s.replacements,
		&s.rawLoss, &s.runLen, &s.avgLoss, &s.residual, &s.nacks, &s.audioMissing, &s.audioNACK, &s.audioRTX, &s.playout, &s.jitter} {
		*p *= f
	}
	for i := range s.layers {
		s.layers[i] *= f
	}
	s.n = 1
}

func (s wifiStat) row(name, history string) string {
	k := max(1, s.n)
	per := s.freezes / max(1e-9, s.seconds/300)
	return fmt.Sprintf("| %s | %s | %.0f | %.0f | %d / %d / %d | %.1f (%.2f) | %.1f | %.0f | %.1f / %.1f / %.1f / %.1f | %.1f | %.2f%% / %.2f%% (%.2f) | %.2f%% | %.0f | %.2f / %.0f / %.0f | %.0f | %.0f | %.0f / %.0f / %.0f / %.0f |\n",
		name, history, s.seconds, s.frames, Pct(s.g2g, .5).Milliseconds(), Pct(s.g2g, .95).Milliseconds(), Pct(s.g2g, .99).Milliseconds(),
		s.freezes, per, s.long, s.maxGap, s.key, s.source, s.replace, s.delivery, s.replacements, 100*s.avgLoss/k, 100*s.rawLoss/k, s.runLen/k, 100*s.residual/k, s.nacks/k, s.audioMissing/k, s.audioNACK/k, s.audioRTX/k, s.playout/k, s.jitter/k, 100*s.layers[0]/k, 100*s.layers[1]/k, 100*s.layers[2]/k, 100*s.layers[3]/k)
}

func num(m map[string]any, keys ...string) float64 {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return math.NaN()
		}
		v = mm[k]
	}
	f, ok := v.(float64)
	if !ok {
		return math.NaN()
	}
	return f
}

func (br *wifiRun) stat() wifiStat {
	v := br.Viewers[0]
	st := wifiStat{n: 1, seconds: (br.To - br.From) / 1000}
	prev := br.From
	var freezes [][2]float64
	for _, f := range v.Timeline {
		if f.At < br.From || f.At > br.To {
			continue
		}
		st.frames++
		if f.Delay != nil {
			st.g2g = append(st.g2g, time.Duration(*f.Delay*float64(time.Millisecond)))
		}
		if f.At-prev > 500 {
			freezes = append(freezes, [2]float64{prev, f.At})
		}
		prev = f.At
	}
	if br.To-prev > 500 {
		freezes = append(freezes, [2]float64{prev, br.To})
	}
	for _, fz := range freezes {
		d := fz[1] - fz[0]
		st.freezes++
		st.maxGap = max(st.maxGap, d)
		if d > 2000 {
			st.long++
		}
		replace := false
		for _, e := range v.Events {
			if e.Kind == "replace" && e.At >= fz[0]-1000 && e.At <= fz[1] {
				replace = true
			}
		}
		// Samples bracketing the freeze: SFU forwarded keys and ingress frames.
		var keys0, keys1, in0, in1 float64 = math.NaN(), math.NaN(), math.NaN(), math.NaN()
		for _, s := range v.Samples {
			if s.Timestamp <= fz[0] {
				keys0, in0 = s.Repair.ForwardedKeys, s.IngressFrames
			}
			if s.Timestamp >= fz[1] && math.IsNaN(keys1) {
				keys1, in1 = s.Repair.ForwardedKeys, s.IngressFrames
			}
		}
		switch {
		case replace:
			st.replace++
		case keys1 > keys0:
			st.key++
		case in1-in0 < 0.5*15*d/1000*2: // two renditions at ~15 fps each
			st.source++
		default:
			st.delivery++
		}
	}
	var loss, lossN, play, playN float64
	var jb0, jbN0, jb1, jbN1 float64 = math.NaN(), 0, math.NaN(), 0
	for _, s := range v.Samples {
		if s.Timestamp < br.From || s.Timestamp > br.To {
			continue
		}
		if a := s.Repair.Estimate.AverageLoss; a != nil {
			loss += *a
			lossN++
		}
		if s.PlayoutTarget != nil {
			play += *s.PlayoutTarget
			playN++
		}
		if d, n := num(s.Stats, "jitterBufferDelay"), num(s.Stats, "jitterBufferEmittedCount"); !math.IsNaN(d) && !math.IsNaN(n) {
			if math.IsNaN(jb0) {
				jb0, jbN0 = d, n
			}
			jb1, jbN1 = d, n
		}
	}
	n := 0.0
	for _, s := range v.Samples {
		if s.Timestamp < br.From || s.Timestamp > br.To {
			continue
		}
		n++
		switch {
		case strings.HasPrefix(s.Layer, "180p/"):
			st.layers[3]++
		case s.Layer == "L1T3":
			st.layers[0]++
		case s.Layer == "L1T2":
			st.layers[1]++
		case s.Layer == "L1T1":
			st.layers[2]++
		}
	}
	for i := range st.layers {
		st.layers[i] /= max(1, n)
	}
	st.avgLoss = loss / max(1, lossN)
	st.rawLoss = math.NaN()
	st.playout = play / max(1, playN)
	if jbN1 > jbN0 {
		st.jitter = 1000 * (jb1 - jb0) / (jbN1 - jbN0)
	}
	for _, e := range v.Events {
		// session.ts logs the policy's action (with "alive … ago") and then
		// replace() itself; count the latter.
		if e.Kind == "replace" && e.Detail == "" && e.At >= br.From && e.At <= br.To {
			st.replacements++
		}
	}
	// getStats counters cover the current PeerConnection only: use ratios.
	for _, s := range br.Feedback.Viewer.SSRCs {
		switch s.Kind {
		case "video":
			st.residual = s.Lost / max(1, s.Packets+s.Lost)
			st.nacks = 1000 * s.NACK / max(1, s.Packets)
		case "audio":
			st.audioMissing = 100 * s.Lost / max(1, s.Packets+s.Lost)
			st.audioNACK = 1000 * s.NACK / max(1, s.Packets)
		}
	}
	st.audioRTX = 1000 * num(br.Feedback.SFU, "audio", "replayed") / max(1, num(br.Feedback.SFU, "audio", "forwarded"))
	return st
}

// scenario is the browser run's setup in sim time: the viewer page starts
// at t = 1 s; established impairs from the observation start, prejoin from 0.
func (br *wifiRun) scenario() (*Scenario, Window) {
	c := clock(br.Viewers[0].StartedAt)
	w := Window{c.sim(br.From), c.sim(br.To)}
	wifi := LinkConfig{Delay: delay(40 * ms), Jitter: 10 * ms, Loss: Loss{P: 0.01, R: 0.19, BadLoss: 1}}
	if br.History != "prejoin" {
		wifi.Start = w.From
	}
	return &Scenario{Name: "calib_wifi_" + br.History, Duration: w.To + time.Second, Native: Clock, Audio: true, Links: []LinkConfig{wifi},
		Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 80 * ms}}}, w
}

func modelStat(r *Result, w Window) wifiStat {
	v := r.Viewers[0]
	f := Frames(v.Timeline, w)
	st := wifiStat{n: 1, seconds: (w.To - w.From).Seconds(), frames: float64(len(f)), g2g: G2Gs(f)}
	for _, x := range f {
		st.network = append(st.network, x.At-x.Capture)
	}
	prev := w.From
	var freezes [][2]time.Duration
	var ends []Rendered
	for _, x := range f {
		if x.At-prev > 500*ms {
			freezes = append(freezes, [2]time.Duration{prev, x.At})
			ends = append(ends, x)
		}
		prev = x.At
	}
	if w.To-prev > 500*ms {
		freezes = append(freezes, [2]time.Duration{prev, w.To})
		ends = append(ends, Rendered{})
	}
	for i, fz := range freezes {
		d := fz[1] - fz[0]
		st.freezes++
		st.maxGap = max(st.maxGap, float64(d.Milliseconds()))
		if d > 2*time.Second {
			st.long++
		}
		// The source gap is judged at capture time: shift by the ending
		// frame's network-and-playout delay.
		shift := 300 * ms
		if e := ends[i]; e.At > 0 {
			shift = e.At - e.Capture
		}
		gap := time.Duration(0)
		last := time.Duration(-1)
		for _, c := range r.Frames[ctrl.Native] {
			if c < fz[0]-shift-time.Second || c > fz[1]-shift {
				continue
			}
			if last >= 0 {
				gap = max(gap, c-last)
			}
			last = c
		}
		replaced := false
		for _, at := range v.Replacements {
			replaced = replaced || at >= fz[0]-time.Second && at <= fz[1]
		}
		switch {
		case replaced:
			st.replace++
		case ends[i].Key:
			st.key++
		case gap > 450*ms:
			st.source++
		default:
			st.delivery++
		}
	}
	sent, got := int64(0), int64(0)
	for k := kindMedia; k <= kindAudio; k++ {
		sent += v.Sent[k]
		got += v.Received[k]
	}
	st.rawLoss = 1 - float64(got)/float64(max(1, sent))
	st.avgLoss = v.AverageLoss
	st.runLen = float64(v.LossPkts) / float64(max(1, v.LossRuns))
	// Chromium's packetsLost excludes NACK-repaired packets but not
	// FEC-recovered ones (recovered packets skip receive statistics).
	st.residual = float64(v.Lost+v.Recovered) / float64(max(1, v.Sent[kindMedia]))
	st.nacks = 1000 * float64(v.VideoNACKs) / float64(max(1, v.Received[kindMedia]+v.Received[kindRTX]))
	audio := float64(max(1, v.Sent[kindAudio]-int64(v.AudioRTX)))
	st.audioMissing = 100 * float64(v.AudioMissing) / audio
	st.audioRTX = 1000 * float64(v.AudioRTX) / audio
	st.audioNACK = 1000 * float64(v.AudioNACKs) / float64(max(1, v.Received[kindAudio]))
	var jb []time.Duration
	for _, x := range f {
		jb = append(jb, x.Buffered)
	}
	sum := time.Duration(0)
	for _, x := range jb {
		sum += x
	}
	st.jitter = float64(sum.Milliseconds()) / float64(max(1, len(jb)))
	st.playout = float64(v.PlayoutMean.Milliseconds())
	ss := Samples(v, w)
	for _, x := range ss {
		switch {
		case !x.Native:
			st.layers[3]++
		default:
			st.layers[2-x.TID]++
		}
	}
	for i := range st.layers {
		st.layers[i] /= float64(max(1, len(ss)))
	}
	for _, at := range v.Replacements {
		if w.has(at) {
			st.replacements++
		}
	}
	return st
}
