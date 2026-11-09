package sfusim

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWifiMissedFrameCauses is diagnostic only (C2 §5 Wi-Fi: "log
// missed-frame causes"). For every A2 bad_wifi freeze (a gap > 500 ms in
// the scored window) it attributes the pictures captured between the last
// displayed picture and the one that ended the freeze, in this order:
//
//   - PC replacement: the viewer app replaced its PeerConnection;
//
//   - source gap: the publisher captured nothing for > 450 ms;
//
//   - local queue drop: the SFU discarded or refused one of them;
//
//   - unrepaired base: a key or TL0 picture left the SFU and never
//     completed at the receiver;
//
//   - repair too late: one completed, but > 250 ms after its last packet
//     left (repair rounds, or parity, arrived after it was due);
//
//   - unrepaired enhancement: a TL1/TL2 picture never completed;
//
//   - ended by a key: a rendition switch, an earlier base picture that
//     never completed, or another broken chain;
//
//   - other: nothing of the rendition forwarded in the span, or
//     decode/playout.
//
//     SFUSIM_CAUSES=1 [SFUSIM_SEEDS=n] go test ./internal/sfusim -run TestWifiMissedFrameCauses -v
func TestWifiMissedFrameCauses(t *testing.T) {
	if os.Getenv("SFUSIM_CAUSES") == "" {
		t.Skip("set SFUSIM_CAUSES=1")
	}
	seeds := 100
	if s := os.Getenv("SFUSIM_SEEDS"); s != "" {
		fmt.Sscan(s, &seeds)
	}
	type fate struct {
		dropped              bool
		sent, complete       time.Duration
		hasSent, hasComplete bool
		base                 bool
	}
	defer func() { fateLog = nil }()
	causes := map[string]int{}
	order := []string{"PC replacement", "source gap", "local queue drop", "unrepaired base", "repair too late", "unrepaired enhancement", "key: rendition switch", "key: earlier unrepaired base", "key: other", "other: nothing forwarded", "other: decode or playout"}
	freezes, seconds := 0, 0.0
	p := badWifi()
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		fates := map[*Frame]*fate{}
		fateLog = func(_ int, f *Frame, kind int, at time.Duration) {
			x := fates[f]
			if x == nil {
				x = &fate{base: f.Key || f.TID == 0}
				fates[f] = x
			}
			switch kind {
			case fateDropped:
				x.dropped = true
			case fateSent:
				x.sent, x.hasSent = at, true
			case fateComplete:
				x.complete, x.hasComplete = at, true
			}
		}
		r := Run(p.Build(false), seed, Default)
		v := r.Viewers[0]
		w := Window{6 * time.Second, r.Scenario.Duration}
		seconds += (w.To - w.From).Seconds()
		f := Frames(v.Timeline, w)
		prev := Rendered{At: w.From, Capture: w.From - 300*ms}
		classify := func(a, b Rendered, end time.Duration) {
			freezes++
			for _, at := range v.Replacements {
				if at >= a.At-time.Second && at <= end {
					causes["PC replacement"]++
					return
				}
			}
			rend := b.Rend
			last, gap := time.Duration(-1), time.Duration(0)
			for _, c := range r.Frames[rend] {
				if c < a.Capture || c > b.Capture {
					continue
				}
				if last >= 0 {
					gap = max(gap, c-last)
				}
				last = c
			}
			if gap > 450*ms {
				causes["source gap"]++
				return
			}
			var span []*fate
			for fr, x := range fates {
				if fr.Rend == rend && fr.Capture > a.Capture && fr.Capture <= b.Capture {
					span = append(span, x)
				}
			}
			has := func(cond func(*fate) bool) bool {
				for _, x := range span {
					if cond(x) {
						return true
					}
				}
				return false
			}
			switch {
			case has(func(x *fate) bool { return x.dropped }):
				causes["local queue drop"]++
			case has(func(x *fate) bool { return x.base && x.hasSent && !x.hasComplete }):
				causes["unrepaired base"]++
			case has(func(x *fate) bool { return x.hasSent && x.hasComplete && x.complete-x.sent > 250*ms }):
				causes["repair too late"]++
			case has(func(x *fate) bool { return !x.base && x.hasSent && !x.hasComplete }):
				causes["unrepaired enhancement"]++
			case b.Key && b.Rend != a.Rend:
				causes["key: rendition switch"]++
			case b.Key:
				// The chain broke before the span: an earlier base picture
				// that never completed, or a picture whose reference died.
				broken := false
				for fr, x := range fates {
					if fr.Rend == rend && x.base && x.hasSent && !x.hasComplete && fr.Capture > a.Capture-2*time.Second && fr.Capture <= a.Capture {
						broken = true
					}
				}
				if broken {
					causes["key: earlier unrepaired base"]++
				} else {
					causes["key: other"]++
				}
			case len(span) == 0:
				causes["other: nothing forwarded"]++
			default:
				causes["other: decode or playout"]++
			}
		}
		for _, x := range f {
			if x.At-prev.At > 500*ms {
				classify(prev, x, x.At)
			}
			prev = x
		}
		if w.To-prev.At > 500*ms {
			classify(prev, Rendered{Rend: prev.Rend, Capture: w.To}, w.To)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d freezes over %d seeds (%.2f per 300 s):\n", freezes, seeds, float64(freezes)/(seconds/300))
	for _, c := range order {
		fmt.Fprintf(&b, "  %-24s %4d  %.2f per 300 s\n", c, causes[c], float64(causes[c])/(seconds/300))
	}
	t.Log("\n" + b.String())
}
