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

// Calibration replays browser torture rows through the model: the archived
// publisher's keys and rates drive the source, and the old controller's
// per-sample decisions (forwarded layer, wire budget, FEC share) drive a
// replay controller. Differences then come from the link, source and
// receiver model, not from controller design. The replay is approximate:
// heartbeat labels lag by the path's queueing, and the old SFU's flight
// limits are represented only by its observed delivery rate.
//
//	SFUSIM_CALIB=test/torture/results/<run>.json go test -run TestCalibration -v ./internal/sfusim

type bFrame struct {
	At    float64  `json:"at"`
	Delay *float64 `json:"delay"`
}

type bSample struct {
	Timestamp        float64 `json:"timestamp"`
	Layer            string  `json:"layer"`
	Width            int     `json:"width"`
	Bitrate          float64 `json:"bitrate"`
	RTT              float64 `json:"rtt"`
	Loss             float64 `json:"loss"`
	FPS              float64 `json:"fps"`
	Keyframes        int     `json:"keyframes"`
	ForwardedPackets int     `json:"forwardedPackets"`
	Repair           struct {
		WireBudget          float64   `json:"wireBudget"`
		WireBytes           float64   `json:"wireBytes"`
		RepairBytes         float64   `json:"repairBytes"`
		PaddingBytes        float64   `json:"paddingBytes"`
		FECPackets          int       `json:"fecPackets"`
		MediaRates          []float64 `json:"mediaRates"`
		SourceKeyframeBytes int       `json:"sourceKeyframeBytes"`
	} `json:"repair"`
}

type bViewer struct {
	StartedAt float64   `json:"startedAt"`
	Samples   []bSample `json:"samples"`
	Timeline  []bFrame  `json:"timeline"`
}

type bRow struct {
	Name       string  `json:"name"`
	From       float64 `json:"from"`
	To         float64 `json:"to"`
	Start      float64 `json:"start"`
	TCPShare   float64 `json:"tcp_link_share"`
	QueueProbe *struct {
		Metrics struct {
			P50 float64 `json:"p50_ms"`
			P95 float64 `json:"p95_ms"`
		} `json:"metrics"`
	} `json:"queue_probe"`
	Intervals []struct {
		Start, From, To float64
		Rate            string
	} `json:"intervals"`
	Impairment json.RawMessage `json:"impairment"` // an object or a description string
	Viewers    []bViewer       `json:"viewers"`
	Publisher  bViewer         `json:"publisher"`
}

func loadRows(t *testing.T, path string) map[string]*bRow {
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
		Commit  string  `json:"commit"`
		Results []*bRow `json:"results"`
	}
	if e = json.NewDecoder(r).Decode(&doc); e != nil {
		t.Fatal(e)
	}
	t.Logf("browser run %s commit %s", path, doc.Commit)
	out := map[string]*bRow{}
	for _, row := range doc.Results {
		if out[row.Name] == nil {
			out[row.Name] = row
		}
	}
	return out
}

// clock maps browser epoch milliseconds to sim time: the viewer page start
// is sim t = 1 s, where the sim viewer joins.
type clock float64

func (c clock) sim(ms float64) time.Duration {
	return time.Second + time.Duration((ms-float64(c))*float64(time.Millisecond))
}

func optionOf(label string) ctrl.Option {
	o := ctrl.Option{Rendition: ctrl.Native, TID: 2}
	if strings.HasPrefix(label, "180p/") {
		o.Rendition = ctrl.Low
	}
	if n := len(label); n > 0 && label[n-1] >= '1' && label[n-1] <= '3' {
		o.TID = label[n-1] - '1'
	}
	return o
}

// replayTrace is the old controller's observed decisions for one viewer.
type replayTrace struct {
	at     []time.Duration
	option []ctrl.Option
	rate   []int
	fec    []float64
	pps    []float64 // observed SFU→viewer packets/s: media, parity, repair (~700 B), padding (~1200 B)
}

// queueDelay estimates the forward-path queueing at browser time ms as the
// nearest frame's G2G above the row's minimum. Heartbeat labels cross the
// same impaired path, so a sample describes an SFU decision this much older.
func queueDelay(v bViewer, ms float64) float64 {
	floor, best, at := math.Inf(1), 0.0, math.Inf(1)
	for _, f := range v.Timeline {
		if f.Delay == nil {
			continue
		}
		floor = min(floor, *f.Delay)
		if d := math.Abs(f.At - ms); d < at {
			at, best = d, *f.Delay
		}
	}
	if math.IsInf(floor, 1) {
		return 0
	}
	return max(0, best-floor)
}

func traceOf(c clock, v bViewer) *replayTrace {
	tr := &replayTrace{}
	for i, s := range v.Samples {
		tr.at = append(tr.at, c.sim(s.Timestamp-queueDelay(v, s.Timestamp)))
		tr.option = append(tr.option, optionOf(s.Layer))
		send := s.Repair.WireBudget
		if send == 0 {
			send = 1.5 * s.Bitrate
		}
		tr.rate = append(tr.rate, max(ctrl.RateMin, int(send)))
		fec := 0.0
		if i > 0 {
			p := v.Samples[i-1]
			if dp := s.ForwardedPackets - p.ForwardedPackets; dp > 0 {
				fec = max(0, float64(s.Repair.FECPackets-p.Repair.FECPackets)/float64(dp))
			}
		}
		tr.fec = append(tr.fec, min(1, fec))
		pps := 0.0
		if i > 0 {
			p := v.Samples[i-1]
			if dt := (s.Timestamp - p.Timestamp) / 1000; dt > 0 {
				pkts := float64(s.ForwardedPackets-p.ForwardedPackets+s.Repair.FECPackets-p.Repair.FECPackets) +
					max(0, s.Repair.RepairBytes-p.Repair.RepairBytes)/700 + max(0, s.Repair.PaddingBytes-p.Repair.PaddingBytes)/1200
				pps = max(0, pkts) / dt
			}
		}
		tr.pps = append(tr.pps, pps)
	}
	return tr
}

func (tr *replayTrace) index(t time.Duration) int {
	// Shifted sample times need not be monotonic; take the latest sample
	// whose decision time has passed.
	best := 0
	for i, at := range tr.at {
		if at <= t && at >= tr.at[best] {
			best = i
		}
	}
	return best
}

// boundedPacer is the old pacer's essential property for replay: a token
// bucket with bounded, dependency-aware queueing.
type boundedPacer struct {
	ctrl.FIFOPacer
	queue []ctrl.Packet
	rate  int
	tok   float64
	at    time.Time
	// fill tops the packet rate up to the observed one with 200 B padding
	// (standing in for the old SFU's parity, repair copies and padding), so
	// netem's per-packet Gilbert-Elliott state sees the browser's packet clock.
	tr   *replayTrace
	fill float64
	pad  uint64
}

func (p *boundedPacer) SetRate(now time.Time, bps int) { p.refill(now); p.rate = bps }
func (p *boundedPacer) Push(_ time.Time, k ctrl.Packet) bool {
	p.queue = append(p.queue, k)
	return true
}
func (p *boundedPacer) Queued() (int, int) { return len(p.queue), 0 }

// Historical diagnostics keep their FIFO/drop rules in deadline predictions.
func (p *boundedPacer) RTXDeparture(now time.Time, kind ctrl.Kind, size, writerPending int) (time.Time, bool) {
	if size <= 0 || writerPending != 0 || p.rate <= 0 {
		return time.Time{}, false
	}
	q := *p
	q.queue = append([]ctrl.Packet(nil), p.queue...)
	used := map[uint64]bool{}
	for _, k := range q.queue {
		used[k.ID] = true
	}
	id := uint64(0)
	for used[id] {
		id++
	}
	q.queue = append(q.queue, ctrl.Packet{ID: id, Size: size, Kind: kind, Class: ctrl.ClassRTX, Enqueued: now})
	at, limit := now, 2*len(q.queue)+4
	for steps := 0; steps < limit; steps++ {
		if k, ok := q.Pop(at); ok && k.ID == id {
			return at, true
		}
		next := q.NextAt(at)
		if next.IsZero() {
			return time.Time{}, false
		}
		if !next.After(at) {
			next = at.Add(time.Nanosecond)
		}
		at = next
	}
	return time.Time{}, false
}
func (p *boundedPacer) refill(now time.Time) {
	if !p.at.IsZero() {
		dt := now.Sub(p.at).Seconds()
		p.tok = min(p.tok+dt*float64(p.rate)/8, 0.01*float64(p.rate)/8)
		if p.tr != nil {
			r := p.tr.pps[p.tr.index(now.Sub(epoch))]
			p.fill = min(p.fill+dt*r, 0.05*r+1)
		}
	}
	p.at = now
}
func (p *boundedPacer) Pop(now time.Time) (ctrl.Packet, bool) {
	p.refill(now)
	// Dependency-aware: aged enhancement layers (TL1/TL2) drop after 100 ms,
	// base-layer packets after 500 ms; keys are never dropped.
	n := 0
	for _, k := range p.queue {
		age := now.Sub(k.Enqueued)
		if !k.Key && (k.Class == ctrl.ClassTL1 || k.Class == ctrl.ClassTL2) && age > 100*time.Millisecond ||
			k.Class == ctrl.ClassTL0 && age > 500*time.Millisecond {
			continue
		}
		p.queue[n] = k
		n++
	}
	p.queue = p.queue[:n]
	if p.tok < 0 {
		return ctrl.Packet{}, false
	}
	if len(p.queue) == 0 {
		if p.fill < 1 {
			return ctrl.Packet{}, false
		}
		p.pad++
		p.fill--
		return ctrl.Packet{ID: 1<<62 + p.pad, Size: 200, Class: ctrl.ClassPadding, Enqueued: now}, true
	}
	k := p.queue[0]
	p.queue = p.queue[1:]
	p.tok -= float64(k.Size)
	p.fill--
	return k, true
}
func (p *boundedPacer) NextAt(now time.Time) time.Time {
	if len(p.queue) == 0 {
		if p.tr == nil {
			return time.Time{}
		}
		return now.Add(10 * time.Millisecond) // padding poll
	}
	if p.tok >= 0 || p.rate <= 0 {
		return now
	}
	return now.Add(time.Duration(-p.tok * 8 / float64(p.rate) * float64(time.Second)))
}

// replayEstimator reports the old wire budget, bounded by 1.1× the link
// rate: the old SFU's flight limits kept its sending near the delivered
// rate, which a nominal budget alone does not express.
type replayEstimator struct {
	ctrl.FixedEstimator
	tr   *replayTrace
	link *LinkConfig
}

func (e *replayEstimator) Estimate(now time.Time) ctrl.Estimate {
	t := now.Sub(epoch)
	r := e.tr.rate[e.tr.index(t)]
	if e.link != nil && t >= e.link.Start {
		bps := 0
		for _, p := range e.link.Rate {
			if p.At <= t {
				bps = p.Bps
			}
		}
		if bps > 0 {
			r = min(r, int(1.1*float64(bps)))
		}
	}
	return ctrl.Estimate{Rate: max(ctrl.RateMin, r)}
}

type replayAllocator struct{ tr *replayTrace }

// replayRepair is the trace's FEC (via Adjust); resends follow ResendNACK.
type replayRepair struct{}

func (replayRepair) Resend(ctrl.RepairQuery) bool           { return true }
func (replayRepair) FEC(ctrl.Estimate, bool) (float64, int) { return 0, 1 }

func (replayAllocator) OnApplied(time.Time, ctrl.Option) {}
func (replayAllocator) OnGeneration(time.Time, uint64)   {}
func (a replayAllocator) Decide(now time.Time, in ctrl.AllocInput) ctrl.Allocation {
	return forcedAllocation(in, a.tr.option[a.tr.index(now.Sub(epoch))])
}

func replayController(trs []*replayTrace, link *LinkConfig) Controller {
	return Controller{Name: "replay", Keys: ctrl.NewRoomKeys,
		// The old SFU makes up to 11 NACK attempts within its repair window.
		ResendNACK: func(age time.Duration, sends int) bool { return age < time.Second && sends < 11 },
		// Round-4 repair redundancy: an extra copy after max(100, min(160, 1.5×RTT)) ms.
		RTXCopy: 120 * time.Millisecond,
		FECAll:  true,
		// The archived browsers' own replacements are not replayed.
		NoAppRecovery: true,
		Adjust: func(id int, now time.Duration, d *ctrl.Decision) {
			tr := trs[id]
			d.FEC = tr.fec[tr.index(now)]
		},
		Viewer: func(id int, keys ctrl.Keys) *ctrl.Viewer {
			v := ctrl.NewViewer(id, keys)
			v.Est = &replayEstimator{tr: trs[id], link: link}
			v.Alloc = replayAllocator{trs[id]}
			v.Pacer = &boundedPacer{tr: trs[id]}
			v.Repair = replayRepair{}
			return v
		}}
}

// sourceReplay rebuilds the publisher's native encoding from its samples.
func sourceReplay(c clock, row *bRow, prof Profile) *Replay {
	r := &Replay{}
	lastKeys := -1
	for _, s := range row.Publisher.Samples {
		at := c.sim(s.Timestamp)
		layer := prof.Layer
		if s.Bitrate > 0 {
			scale := s.Bitrate / (prof.Layer[0] + prof.Layer[1] + prof.Layer[2])
			for i := range layer {
				layer[i] *= scale
			}
		}
		fps := prof.FPS
		if s.FPS > 0 {
			fps = s.FPS
		}
		r.At, r.FPS, r.Layer = append(r.At, max(0, at)), append(r.FPS, fps), append(r.Layer, layer)
		if lastKeys >= 0 && s.Keyframes > lastKeys {
			r.Keys = append(r.Keys, KeyEvent{At: max(0, at-250*time.Millisecond), Bytes: s.Repair.SourceKeyframeBytes})
		}
		lastKeys = s.Keyframes
	}
	return r
}

// browserWindow returns frames, G2G values and gaps for a browser viewer.
func browserWindow(v bViewer, from, to float64) (g2g []time.Duration, freezes int, maxGap time.Duration) {
	prev := from
	for _, f := range v.Timeline {
		if f.At < from || f.At > to {
			continue
		}
		if f.Delay != nil {
			g2g = append(g2g, time.Duration(*f.Delay*float64(time.Millisecond)))
		}
		if gap := f.At - prev; gap > 500 {
			freezes++
			maxGap = max(maxGap, time.Duration(gap*float64(time.Millisecond)))
		}
		prev = f.At
	}
	if gap := to - prev; gap > 500 {
		freezes++
		maxGap = max(maxGap, time.Duration(gap*float64(time.Millisecond)))
	}
	return
}

func browserBitrate(v bViewer, from, to float64) float64 {
	sum, n := 0.0, 0
	for _, s := range v.Samples {
		if s.Timestamp >= from && s.Timestamp <= to && s.Bitrate > 0 {
			sum += s.Bitrate
			n++
		}
	}
	return sum / float64(max(1, n))
}

type calibCase struct {
	name      string
	sc        *Scenario
	ctl       Controller
	windows   []Window     // sim time
	browser   [][2]float64 // browser ms windows, same order
	viewer    bViewer
	fit       string // profile whose Endpoint is fitted on this row's G2G p5
	fitWindow int    // which window the fit uses
}

const calibSeeds = 20

func calibCases(t *testing.T, rows map[string]*bRow) []calibCase {
	var cs []calibCase
	link := func(c clock, startMS float64, cfg LinkConfig) LinkConfig { cfg.Start = c.sim(startMS); return cfg }
	if row := rows["screen_native_5mbit_established"]; row != nil {
		v := row.Viewers[0]
		c := clock(v.StartedAt)
		sc := &Scenario{Name: "calib_b1_5m", Duration: c.sim(row.To) + time.Second, Native: Detailed,
			Links:   []LinkConfig{link(c, row.Start, fifo(5_000_000, 40*ms))},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		sc.Replay[ctrl.Native] = sourceReplay(c, row, Detailed)
		ctl := replayController([]*replayTrace{traceOf(c, v)}, &sc.Links[0])
		cs = append(cs, calibCase{name: "B1 detailed 5M established", sc: sc, ctl: ctl, viewer: v, fit: "Detailed",
			windows: []Window{{c.sim(row.From), c.sim(row.To)}}, browser: [][2]float64{{row.From, row.To}}})
	}
	if row := rows["bad_wifi"]; row != nil {
		v := row.Viewers[0]
		c := clock(v.StartedAt)
		// The archived spec reorders (netem jitter without rate).
		wifi := link(c, row.From, LinkConfig{Delay: delay(40 * ms), Jitter: 10 * ms, Reorder: true, Loss: Loss{P: 0.01, R: 0.19, BadLoss: 1}})
		sc := &Scenario{Name: "calib_bad_wifi", Duration: c.sim(row.To) + time.Second, Native: Clock, Links: []LinkConfig{wifi},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 80 * ms}}}
		sc.Replay[ctrl.Native] = sourceReplay(c, row, Clock)
		cs = append(cs, calibCase{name: "bad_wifi (clock source, reordering netem)", sc: sc, ctl: replayController([]*replayTrace{traceOf(c, v)}, &sc.Links[0]), viewer: v, fit: "Clock",
			windows: []Window{{c.sim(row.From), c.sim(row.To)}}, browser: [][2]float64{{row.From, row.To}}})
	}
	var evidence struct {
		Counters []struct {
			At float64 `json:"at"`
		} `json:"counters"`
	}
	if row := rows["screen_viewer_256k"]; row != nil && json.Unmarshal(row.Impairment, &evidence) == nil && len(evidence.Counters) > 0 {
		v := row.Viewers[0]
		c := clock(v.StartedAt)
		imp := evidence.Counters[0].At
		sc := &Scenario{Name: "calib_256k", Duration: c.sim(imp+41000) + time.Second, Native: Detailed,
			Links:   []LinkConfig{link(c, imp, fifo(256_000, 40*ms))},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		sc.Replay[ctrl.Native] = sourceReplay(c, row, Detailed)
		w := [2]float64{imp + 10000, imp + 40000}
		cs = append(cs, calibCase{name: "screen_viewer_256k", sc: sc, ctl: replayController([]*replayTrace{traceOf(c, v)}, &sc.Links[0]), viewer: v,
			windows: []Window{{c.sim(w[0]), c.sim(w[1])}}, browser: [][2]float64{w}})
	}
	if row := rows["screen_native_flap"]; row != nil && len(row.Intervals) == 3 {
		v := row.Viewers[0]
		c := clock(v.StartedAt)
		l := fifo(5_000_000, 40*ms)
		l.Rate = nil
		for _, in := range row.Intervals {
			bps := map[string]int{"5mbit": 5_000_000, "256kbit": 256_000}[in.Rate]
			l.Rate = append(l.Rate, RatePoint{c.sim(in.Start), bps})
		}
		l.Rate[0].At = 0
		l = link(c, row.Intervals[0].Start-1000, l)
		sc := &Scenario{Name: "calib_flap", Duration: c.sim(row.Intervals[2].To) + time.Second, Native: Detailed, Links: []LinkConfig{l},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		sc.Replay[ctrl.Native] = sourceReplay(c, row, Detailed)
		cc := calibCase{name: "screen_native_flap 5M→256k→5M", sc: sc, ctl: replayController([]*replayTrace{traceOf(c, v)}, &sc.Links[0]), viewer: v, fit: "Low", fitWindow: 1}
		for _, in := range row.Intervals {
			cc.windows = append(cc.windows, Window{c.sim(in.From), c.sim(in.To)})
			cc.browser = append(cc.browser, [2]float64{in.From, in.To})
		}
		cs = append(cs, cc)
	}
	return cs
}

type calibStat struct {
	p50, p95        time.Duration
	freezes, maxGap float64
	bitrate         float64
}

func simStats(rs []*Result, w Window) calibStat {
	var g2g []time.Duration
	var st calibStat
	for _, r := range rs {
		v := r.Viewers[0]
		f := Frames(v.Timeline, w)
		g2g = append(g2g, G2Gs(f)...)
		st.freezes += float64(len(Gaps(f, w)))
		st.maxGap += MaxGap(f, w).Seconds() * 1000
		st.bitrate += ReceivedRate(v, w)
	}
	n := float64(len(rs))
	st.p50, st.p95 = Pct(g2g, .5), Pct(g2g, .95)
	st.freezes, st.maxGap, st.bitrate = st.freezes/n, st.maxGap/n, st.bitrate/n
	return st
}

// Tolerances from the task brief: G2G p95 within ±25%; freeze rate within ×2
// (compared as (count + 0.5) ratios so zero counts are comparable).
func within(simP95, browserP95 time.Duration, simF, browserF float64) (bool, bool) {
	p := browserP95 > 0 && math.Abs(float64(simP95-browserP95)) <= 0.25*float64(browserP95)
	f := math.Abs(math.Log2((simF+0.5)/(browserF+0.5))) <= 1
	return p, f
}

func TestCalibration(t *testing.T) {
	path := os.Getenv("SFUSIM_CALIB")
	if path == "" {
		t.Skip("set SFUSIM_CALIB to a browser torture result JSON")
	}
	rows := loadRows(t, path)
	cases := calibCases(t, rows)
	run := func(cc calibCase) []*Result {
		rs := make([]*Result, calibSeeds)
		for i := range rs {
			rs[i] = Run(cc.sc, uint64(i+1), cc.ctl)
		}
		return rs
	}
	// Fit each profile's endpoint delay on one designated row: browser G2G
	// p5 minus the modelled network p5 with a zero endpoint (p5 carries the
	// least queueing). Every p95, freeze and gap comparison below is then a
	// validation, not a fit.
	for _, cc := range cases {
		if cc.fit == "" {
			continue
		}
		z := *cc.sc
		z.Native.Endpoint, z.Low.Endpoint = 0, 0
		if z.Low.FPS == 0 {
			z.Low = LowProfile
			z.Low.Endpoint = 0
		}
		zc := cc
		zc.sc = &z
		i := cc.fitWindow
		var net []time.Duration
		for _, r := range run(zc) {
			net = append(net, G2Gs(Frames(r.Viewers[0].Timeline, cc.windows[i]))...)
		}
		g, _, _ := browserWindow(cc.viewer, cc.browser[i][0], cc.browser[i][1])
		t.Logf("endpoint fit %-8s on %s window %d: browser p5 %v − model network p5 %v = %v", cc.fit, cc.name, i+1, Pct(g, .05), Pct(net, .05), Pct(g, .05)-Pct(net, .05))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| Row / window | Browser G2G p50 / p95 ms | Model G2G p50 / p95 ms | p95 within ±25%% | Browser freezes >500 ms | Model freezes (mean of %d seeds) | Within ×2 | Browser / model max gap ms | Browser / model kbit/s |\n|---|---|---|---|---|---|---|---|---|\n", calibSeeds)
	for _, cc := range cases {
		rs := run(cc)
		for i, w := range cc.windows {
			bw := cc.browser[i]
			g, bf, bg := browserWindow(cc.viewer, bw[0], bw[1])
			st := simStats(rs, w)
			okP, okF := within(st.p95, Pct(g, .95), st.freezes, float64(bf))
			name := cc.name
			if len(cc.windows) > 1 {
				name = fmt.Sprintf("%s, interval %d", cc.name, i+1)
			}
			fmt.Fprintf(&b, "| %s | %d / %d | %d / %d | %v | %d | %.2f | %v | %d / %.0f | %.0f / %.0f |\n", name,
				Pct(g, .5).Milliseconds(), Pct(g, .95).Milliseconds(), st.p50.Milliseconds(), st.p95.Milliseconds(), okP, bf, st.freezes, okF,
				bg.Milliseconds(), st.maxGap, browserBitrate(cc.viewer, bw[0], bw[1])/1000, st.bitrate/1000)
		}
	}
	if row := rows["fifo_tcp_only_512kbit"]; row != nil && row.QueueProbe != nil {
		var probe []time.Duration
		share := 0.0
		for seed := uint64(1); seed <= calibSeeds; seed++ {
			r := Run(fifoScenario("fifo_tcp_only", 512_000, 1, false), seed, Default)
			probe = append(probe, ProbeDelays(r, score)...)
			share += TCPGoodput(r, 0, score) / 512_000 / calibSeeds
		}
		bp := time.Duration(row.QueueProbe.Metrics.P95 * float64(time.Millisecond))
		okP, _ := within(Pct(probe, .95), bp, 0, 0)
		fmt.Fprintf(&b, "| fifo_tcp_only_512kbit queue probe (rwnd %d B) | %.0f / %.0f | %d / %d | %v | — | — | — | — | TCP share %.3f / %.3f |\n", DefaultRwnd,
			row.QueueProbe.Metrics.P50, row.QueueProbe.Metrics.P95, Pct(probe, .5).Milliseconds(), Pct(probe, .95).Milliseconds(), okP, row.TCPShare, share)
	}
	t.Log("\n" + b.String())
}

// TestRwndSweep prints the TCP-only queue probe for candidate window caps;
// the browser target is fifo_tcp_only_512kbit's probe p50/p95.
func TestRwndSweep(t *testing.T) {
	if os.Getenv("SFUSIM_RWND_SWEEP") == "" {
		t.Skip("set SFUSIM_RWND_SWEEP=1")
	}
	saved := DefaultRwnd
	defer func() { DefaultRwnd = saved }()
	for rw := 17_000; rw <= 21_000; rw += 1_000 {
		DefaultRwnd = rw
		var probe []time.Duration
		share := 0.0
		loss := [2]int{}
		for seed := uint64(1); seed <= calibSeeds; seed++ {
			r := Run(fifoScenario("fifo_tcp_only", 512_000, 1, false), seed, Default)
			probe = append(probe, ProbeDelays(r, score)...)
			share += TCPGoodput(r, 0, score) / 512_000 / calibSeeds
			loss[0] += r.TCPLoss[0][0]
			loss[1] += r.TCPLoss[0][1]
		}
		t.Logf("rwnd %d: probe p50 %v p95 %v max %v share %.3f rto %d fastrtx %d", rw, Pct(probe, .5), Pct(probe, .95), Pct(probe, 1), share, loss[0], loss[1])
	}
}
