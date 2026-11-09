package ctrl

import (
	"testing"
	"time"
)

// All contexts below are reconstructed owning inputs, not browser/default traces.
func lifecycleKey(t *testing.T, q *Queue, at time.Time, r Rendition, id uint64, n, size int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !q.Push(at, Packet{Kind: KindVideo, Class: ClassKey, Rendition: r, Frame: id, Key: true, Size: size, FrameSize: n * size, Packets: n, Last: i == n-1, Deadline: at.Add(KeyEnvelope), FreshUntil: at.Add(FrameDeadline)}) {
			t.Fatal("key rejected")
		}
	}
}
func lifecycleOwnedLow(t *testing.T) (*Viewer, *Queue, *commitEstimate, time.Time) {
	t.Helper()
	now := epoch.Add(10 * time.Second)
	k := NewRoomKeys()
	k.Request(now.Add(-4*time.Second), 1, Low, CauseJoin)
	k.Due(now.Add(-4 * time.Second))
	k.OnKey(now.Add(-4*time.Second), Low, 100, now.Add(-4*time.Second))
	v := NewViewer(1, k)
	v.started = true
	v.Probe = NoProber{}
	e := &commitEstimate{Estimate{Rate: 1500000, Proven: 1500000, Sustained: 1500000, Ceiling: 1500000}}
	v.Est = e
	q := v.Pacer.(*Queue)
	q.SetRate(now.Add(-time.Second), 1500000)
	lifecycleKey(t, q, now.Add(-time.Second), Low, 1, 1, 100)
	if _, ok := q.Pop(now.Add(-time.Second)); !ok {
		t.Fatal("initial Low")
	}
	q.KeyEvents()
	v.OnApplied(now.Add(-time.Second), Option{Low, 2})
	return v, q, e, now
}
func lifecycleQuietCosts() Costs {
	return Costs{Rate: [2][3]int{{1335818, 1719037, 2362168}, {59044, 82948, 115870}}, KeyBytes: [2]int{617480, 9963}, KeySamples: [2]int{2, 2}, AudioWire: 65202}
}

// This paired test uses pre-intent APIs only. A real initial allocation authorizes
// Native T0 at a lower p75. A Low refresh is already physically partial when the
// actual Native key arrives and raises p75. Live Native Hold and actual size fit.
func TestPreparedNativeBehindLowRefresh(t *testing.T) {
	v, q, _, now := lifecycleOwnedLow(t)
	c := lifecycleQuietCosts()
	c.Rate[Native][0] = 1200000
	c.KeyBytes[Native] = 264453
	d := v.Step(now, c)
	if d.Desired != (Option{Native, 0}) || !d.Requested {
		t.Fatalf("initial eligible target %+v", d)
	}
	// Source's Low refresh serves a different viewer; no forced Desired.
	lifecycleKey(t, q, now, Low, 2, 8, 1000)
	// BeforeSend uses the current Native desire but must preserve/finish old Low.
	v.BeforeSend(now, 0, d.Desired, c)
	if p, ok := q.Pop(now); !ok || p.Last || p.Rendition != Low {
		t.Fatalf("Low refresh FIRST %+v %v", p, ok)
	}
	lifecycleKey(t, q, now, Native, 73, 210, 1259)
	c = lifecycleQuietCosts()
	if !v.KeyFits(now, Native, 210*1259, c) || !nativeHolds(now, v.Est.Estimate(now), c, 0) {
		t.Fatal("not affordable actual prepared key")
	}
	forecast := v.PictureService(now, PictureQuery{Rendition: Native, Frame: 73, Generation: v.Generation(), Commit: true})
	t.Logf("hold=%d freshAdmit=%d actualKey=%d forecast=%+v", Hold(c, Option{Native, 0}, 0, 0), func() int { n, _ := Admit(c, Option{Native, 0}, 0, 0); return n }(), 210*1259, forecast)
	if !forecast.Known || forecast.Broken || forecast.Key.IsZero() {
		t.Fatal("counterexample not serviceable")
	}
	d = v.Step(now, c)
	t.Logf("decision=%+v prep=%v native=%v", d, v.SwitchOutcome(), v.delivering[Native])
	v.BeforeSend(now, 0, d.Desired, c)
	left, total := q.QueuedKey(Native)
	t.Logf("Native queued=%d/%d Low remaining=%d", left, total, len(q.originals[frameID{Low, 2}].packets))
	if d.Desired != (Option{Native, 0}) || left != 210*1259 {
		t.Error("live prepared Native was discarded while finishing Low refresh")
	}
}

func TestLifecycleBranchBasisAndKeyIdentity(t *testing.T) {
	now := epoch.Add(20 * time.Second)
	a := &LayerAllocator{}
	completed := Option{Native, 2}
	a.OnApplied(now.Add(-3*time.Second), completed)
	changed := a.changed
	c := Costs{Rate: [2][3]int{{100000, 200000, 300000}, {10000, 20000, 30000}}, KeyBytes: [2]int{1000, 1000}, KeySamples: [2]int{8, 8}}
	e := Estimate{Rate: 1000000, Proven: 1000000, Sustained: 1000000}
	low := Option{Low, 0}
	// Evidence from Native2 cannot become Low's temporal clock.
	a.Decide(now.Add(-2*time.Second), AllocInput{Applied: completed, Operating: &completed, Costs: c, Estimate: e})
	d := a.Decide(now, AllocInput{Applied: completed, Operating: &low, Costs: c, Estimate: e})
	if d.Intent != Retain || d.Desired != low || a.up != now || a.changed != changed {
		t.Fatalf("basis leak %+v %+v", d, a)
	}
	if d = a.Decide(now.Add(TemporalUpTime-time.Nanosecond), AllocInput{Applied: completed, Operating: &low, Costs: c, Estimate: e}); d.Intent != Retain {
		t.Fatal("short temporal clock")
	}
	d = a.Decide(now.Add(TemporalUpTime), AllocInput{Applied: completed, Operating: &low, Costs: c, Estimate: e})
	if d.Intent != Upgrade || d.Desired != (Option{Low, 1}) {
		t.Fatalf("lost temporal upgrade %+v", d)
	}
	// Same-rendition temporal reduction must not demand a new key.
	v, q, est, at := lifecycleOwnedLow(t)
	est.value = Estimate{Rate: 80000, Proven: 80000, Sustained: 80000}
	c = Costs{Rate: [2][3]int{{1000000, 1500000, 2000000}, {10000, 20000, 100000}}}
	v.Step(at, c)
	d2 := v.Step(at.Add(TrendWindow), c)
	if d2.Intent != Required || d2.Desired != (Option{Low, 1}) || d2.Requested {
		t.Fatalf("temporal reduction acquired reset %+v", d2)
	}
	// Authorization mismatch must neither release nor grant a different identity.
	lifecycleKey(t, q, at.Add(TrendWindow), Native, 30, 2, 1000)
	auth, _ := q.Key(Native)
	auth.Option = Option{Native, 0}
	auth.Intent = Upgrade
	before, _ := q.Queued()
	for _, mutate := range []func(*KeyAuthorization){func(x *KeyAuthorization) { x.Frame++ }, func(x *KeyAuthorization) { x.Generation++ }, func(x *KeyAuthorization) { x.Option.Rendition = Low }, func(x *KeyAuthorization) { x.Option.TID = 3 }} {
		b := auth
		mutate(&b)
		if q.Authorize(b) {
			t.Fatal("foreign authorization accepted")
		}
	}
	after, _ := q.Queued()
	if before != after || q.authorization != nil {
		t.Fatal("foreign authorization mutated queue")
	}
}

func TestLifecycleStartedAndCompletionCap(t *testing.T) {
	for _, arm := range []string{"raise", "reduce", "foreign-low", "generation"} {
		t.Run(arm, func(t *testing.T) {
			v, q, e, now := lifecycleOwnedLow(t)
			c := Costs{Rate: [2][3]int{{100000, 200000, 300000}, {10000, 20000, 30000}}, KeyBytes: [2]int{20000, 1000}, KeySamples: [2]int{8, 8}}
			// Use real allocator choice and normal preparation; ceiling constrains fast goal.
			e.value = Estimate{Rate: 200000, Proven: 200000, Sustained: 200000, Ceiling: 200000}
			d := v.Step(now, c)
			if d.Desired != (Option{Native, 0}) {
				t.Fatalf("initial T0 admission %+v", d)
			}
			lifecycleKey(t, q, now, Native, 40, 20, 1000)
			v.Step(now, c)
			v.BeforeSend(now, 0, d.Desired, c)
			p, ok := q.Pop(now)
			if !ok || p.Last || p.Rendition != Native {
				t.Fatalf("not started %+v", p)
			}
			q.CancelKey(now, Native)
			if !q.KeyInFlight() {
				t.Fatal("started key unstarted-cancelled")
			}
			want := Option{Native, 0}
			if arm == "raise" || arm == "reduce" {
				e.value = Estimate{Rate: 1000000, Proven: 1000000, Sustained: 1000000}
				d = v.Step(now, c)
				v.BeforeSend(now, 0, d.Desired, c)
				want.TID = 2
			}
			if arm == "reduce" {
				e.value = Estimate{Rate: 160000, Proven: 160000, Sustained: 160000, Ceiling: 160000}
				d = v.Step(now, c)
				want.TID = 0
			}
			if arm == "foreign-low" {
				e.value.Overuse = true
				d = v.Step(now, c)
				if d.Intent != Required || d.Desired.Rendition != Low {
					t.Fatal("not Required Low")
				}
			}
			if arm == "generation" {
				v.OnPathChange(now)
				d = v.Step(now, c)
				if d.Desired.Rendition != Low || d.Intent != Required {
					t.Fatal("old generation remained eligible")
				}
			}
			for i := 0; i < 1000; i++ {
				v.BeforeSend(now, 0, d.Desired, c)
				p, ok = q.Pop(now)
				if ok && p.Last {
					o, valid := v.CompletionOption(now, p, c)
					if arm == "generation" {
						if valid {
							t.Fatal("old-generation LAST applied")
						}
						return
					}
					if !valid || o != want {
						t.Fatalf("wrong cap %v/%v want%v", o, valid, want)
					}
					for _, bad := range []Packet{{Key: true, Last: true, Rendition: Low, Frame: p.Frame, Generation: p.Generation}, {Key: true, Last: true, Rendition: Native, Frame: p.Frame + 1, Generation: p.Generation}, {Key: true, Last: true, Rendition: Native, Frame: p.Frame, Generation: p.Generation + 1}} {
						if _, ok := v.CompletionOption(now, bad, c); ok {
							t.Fatal("foreign LAST used cap")
						}
					}
					return
				}
				if !ok {
					next := q.NextAt(now)
					if next.IsZero() {
						break
					}
					now = next
				}
			}
			t.Fatal("no finite completion")
		})
	}
}

func TestPreparedNativeOverlapControls(t *testing.T) {
	for _, arm := range []string{"no-partial-refresh", "fresh-admission-still-fits", "actual-hold-loss"} {
		t.Run(arm, func(t *testing.T) {
			v, q, _, now := lifecycleOwnedLow(t)
			c := lifecycleQuietCosts()
			c.Rate[Native][0] = 1200000
			c.KeyBytes[Native] = 264453
			d := v.Step(now, c)
			if d.Desired.Rendition != Native {
				t.Fatal("no initial admission")
			}
			if arm != "no-partial-refresh" {
				lifecycleKey(t, q, now, Low, 2, 8, 1000)
				v.BeforeSend(now, 0, d.Desired, c)
				q.Pop(now)
			}
			lifecycleKey(t, q, now, Native, 73, 210, 1259)
			if arm != "fresh-admission-still-fits" {
				c = lifecycleQuietCosts()
			}
			if arm == "actual-hold-loss" {
				c.Rate[Native][0] = 1600000
			}
			d = v.Step(now, c)
			v.BeforeSend(now, 0, d.Desired, c)
			left, _ := q.QueuedKey(Native)
			keep := arm != "actual-hold-loss"
			if (left > 0) != keep || (d.Desired.Rendition == Native) != keep {
				t.Fatalf("control %s %+v left%d", arm, d, left)
			}
		})
	}
}

func TestFastPreparedNativeBehindLowRefresh(t *testing.T) {
	v, q, e, now := lifecycleOwnedLow(t)
	oldApplied := v.Applied()
	changed := v.Alloc.(*LayerAllocator).changed
	e.value = Estimate{Rate: 440001, Proven: 440001, Sustained: 440001, Ceiling: 440001}
	q.SetRate(now, 440001)
	c := Costs{Rate: [2][3]int{{350000, 700000, 1000000}, {50000, 70000, 100000}}, KeyBytes: [2]int{10000, 9000}, KeySamples: [2]int{8, 8}}
	d := v.Step(now, c)
	if d.Desired != (Option{Native, 0}) || !d.Requested {
		t.Fatalf("not admitted initially %+v", d)
	}
	lifecycleKey(t, q, now, Low, 2, 2, 1000)
	v.BeforeSend(now, 0, d.Desired, c)
	if p, ok := q.Pop(now); !ok || p.Last || p.Rendition != Low {
		t.Fatal("no partial Low")
	}
	lifecycleKey(t, q, now, Native, 73, 13, 1000)
	c.Rate[Native][0] = 400000
	c.KeyBytes[Native] = 13000
	forecast := v.PictureService(now, PictureQuery{Rendition: Native, Frame: 73, Generation: v.Generation(), Commit: true})
	if !v.KeyFits(now, Native, 13000, c) || !nativeHolds(now, e.value, c, 0) || !forecast.Known || forecast.Broken || forecast.Key.IsZero() || forecast.Key.Sub(now) > KeyFast {
		t.Fatalf("not fast service %+v", forecast)
	}
	t.Logf("Native Hold%d freshSteady%d forecastLAST%s (<KeyFast%s)", Hold(c, Option{Native, 0}, 0, 0), func() int { _, steady := Admit(c, Option{Native, 0}, 0, 0); return steady }(), forecast.Key.Sub(now), KeyFast)
	real := q.snapshot()
	real.SetCommit(Native, true)
	at := now
	actual := time.Time{}
	for i := 0; i < 1000; i++ {
		p, ok := real.Pop(at)
		if ok && p.Key && p.Last && p.Rendition == Native {
			actual = at
			break
		}
		if !ok {
			next := real.NextAt(at)
			if next.IsZero() {
				break
			}
			at = next
		}
	}
	if actual != forecast.Key {
		t.Fatalf("forecast/actual differ %s %s", forecast.Key.Sub(now), actual.Sub(now))
	}
	t.Logf("actualPopLAST=%s", actual.Sub(now))
	d = v.Step(now, c)
	v.BeforeSend(now, 0, d.Desired, c)
	left, _ := q.QueuedKey(Native)
	if d.Desired != (Option{Native, 0}) || left != 13000 {
		t.Fatalf("fast prepared key discarded: %+v queued%d", d, left)
	}
	// Exercise the live queue, not only the private forecast/replay. Low's
	// whole refresh and its bound cap finish before Native changes ownership.
	if v.Operating() != oldApplied || v.Applied() != oldApplied || v.Alloc.(*LayerAllocator).changed != changed {
		t.Fatal("candidate selection changed physical/completed Low ownership")
	}
	lowComplete, nativeStarted := false, false
	for at, steps := now, 0; steps < 1000; steps++ {
		v.BeforeSend(at, 0, d.Desired, c)
		p, ok := q.Pop(at)
		if ok {
			if p.Rendition == Low {
				if nativeStarted {
					t.Fatal("old Low followed Native FIRST")
				}
				if p.Last {
					o, valid := v.CompletionOption(at, p, c)
					if !valid || o != oldApplied {
						t.Fatal("Native candidate cap leaked into Low completion")
					}
					v.OnApplied(at, o)
					lowComplete = true
				}
			} else {
				if !lowComplete {
					t.Fatal("optional Native leapfrogged a started Low refresh")
				}
				nativeStarted = true
				if v.Applied() != oldApplied || v.Alloc.(*LayerAllocator).changed != changed {
					t.Fatal("Native FIRST changed completed dwell")
				}
				if p.Last {
					o, valid := v.CompletionOption(at, p, c)
					if !valid || o != (Option{Native, 0}) || at != actual {
						t.Fatalf("live completion differs from exact bound/service: %v %v at%s", o, valid, at.Sub(now))
					}
					v.OnApplied(at, o)
					if v.Alloc.(*LayerAllocator).changed != at || v.Operating() != o {
						t.Fatal("Native LAST did not reconcile ownership/dwell")
					}
					t.Logf("live Low→Native LAST=%s", at.Sub(now))
					return
				}
			}
		} else {
			at = q.NextAt(at)
			if at.IsZero() {
				break
			}
		}
	}
	t.Fatal("live prepared candidate did not finish")
}

func TestLifecycleBoundedAuthorizationWhileWriterUnknown(t *testing.T) {
	v, q, e, now := lifecycleOwnedLow(t)
	e.value = Estimate{Rate: 440001, Proven: 440001, Sustained: 440001, Ceiling: 440001}
	c := Costs{Rate: [2][3]int{{350000, 700000, 1000000}, {50000, 70000, 100000}}, KeyBytes: [2]int{10000, 9000}, KeySamples: [2]int{8, 8}}
	releases := 0
	q.SetRelease(func(Packet) { releases++ })
	low, _ := q.Operating()
	for i := 0; i < 1000; i++ {
		at := now.Add(time.Duration(i) * time.Millisecond)
		v.Step(at, c)
		lifecycleKey(t, q, at, Native, uint64(100+i), 2, 1000)
		v.Step(at, c)
		v.BeforeSend(at, 1, Option{Native, 0}, c)
		if active, _ := q.Operating(); !sameKey(active, low) || q.authorization != nil || !q.UncommittedKey(Native) {
			t.Fatal("unknown writer granted/replaced physical owner")
		}
		q.CancelKey(at, Native)
		v.Step(at, c)
		bytes, frames := q.Queued()
		drops, events := q.Retained()
		if bytes != 0 || frames != 0 || drops != 0 || events > 4 || q.authorization != nil || !v.recoveryAt.IsZero() || v.refusedFor != 0 {
			t.Fatalf("lifecycle leak %d bytes%d frames%d drops%d events%d", i, bytes, frames, drops, events)
		}
	}
	if releases != 2000 {
		t.Fatalf("payload release count %d", releases)
	}
}

// A fresh optional acquisition after actual key refusal is not retention of an
// admitted transaction. Existing prepared state must not override NativeKey=false.
func TestSyntheticPreparationHonorsRefusal(t *testing.T) {
	v, _, e, now := lifecycleOwnedLow(t)
	e.value = Estimate{Rate: 600000, Proven: 600000, Sustained: 600000, Ceiling: 600000}
	c := Costs{Rate: [2][3]int{{100000, 200000, 300000}, {10000, 20000, 30000}}, KeyBytes: [2]int{10000, 1000}, KeySamples: [2]int{8, 8}}
	for i := 0; i < 8; i++ {
		at := now.Add(-4*time.Second + time.Duration(i)*time.Nanosecond)
		v.Keys.OnKey(at, Native, 10000, at)
	}
	d := v.Step(now, c)
	if d.Desired.Rendition != Native || !d.Requested {
		t.Fatalf("no real eligible request %+v", d)
	}
	due := v.Keys.Due(now)
	if len(due) != 1 || due[0] != Native {
		t.Fatal("no ordinary original PLI")
	}
	// A single large actual observation need not move p75 of eight prior keys.
	v.Keys.OnKey(now, Native, 200000, now)
	if size, n := v.Keys.Size(now, Native); size != 10000 || n != 8 {
		t.Fatal("actual history did not retain p75")
	}
	if v.KeyFits(now, Native, 200000, c) || v.refusedFor != KeyRefusedMin {
		t.Fatal("not an actual first key refusal")
	}
	if v.Pacer.KeyInFlight() {
		t.Fatal("a real admitted key would change this contract")
	}
	originalUntil := v.refusedUntil
	at := now.Add(KeyRetry)
	d = v.Step(at, c)
	t.Logf("refusedFor=%s remaining=%s desired=%v requested=%v status=%v prep=%v", v.refusedFor, originalUntil.Sub(at), d.Desired, d.Requested, d.Key, v.SwitchOutcome())
	if d.Desired.Rendition == Native || d.Requested {
		t.Error("synthetic prepared Frame0 bypassed actual refusal for a fresh optional acquisition")
	}
	if v.SwitchOutcome() != SwitchIdle {
		t.Fatal("refused unadmitted round was not retired")
	}
	if v.refusedUntil != originalUntil || v.refusedFor != KeyRefusedMin {
		t.Fatal("probe altered refusal values")
	}
	d = v.Step(originalUntil, c)
	if d.Desired.Rendition != Native || !d.Requested {
		t.Fatalf("refusal expiry starved eligible Native %+v", d)
	}
}

// A refusal of a different assembly cannot turn an existing useful key into
// a synthetic target. No new Native acquisition is made while it is retained.
func TestPreparedCandidateAndSyntheticRefusalAreDistinct(t *testing.T) {
	for _, actual := range []bool{false, true} {
		t.Run(map[bool]string{false: "unadmitted", true: "actual-key"}[actual], func(t *testing.T) {
			v, q, e, now := lifecycleOwnedLow(t)
			e.value = Estimate{Rate: 440001, Proven: 440001, Sustained: 440001, Ceiling: 440001}
			c := Costs{Rate: [2][3]int{{350000, 700000, 1000000}, {50000, 70000, 100000}}, KeyBytes: [2]int{10000, 9000}, KeySamples: [2]int{8, 8}}
			d := v.Step(now, c)
			if !d.Requested || d.Desired != (Option{Native, 0}) {
				t.Fatal("no normal eligible preparation")
			}
			lifecycleKey(t, q, now, Low, 2, 2, 1000)
			v.BeforeSend(now, 0, d.Desired, c)
			q.Pop(now)
			if actual {
				lifecycleKey(t, q, now, Native, 73, 13, 1000)
			}
			if v.KeyFits(now, Native, 200000, c) || v.refusedFor != KeyRefusedMin {
				t.Fatal("no real refusal")
			}
			c.Rate[Native][0], c.KeyBytes[Native] = 400000, 13000
			d = v.Step(now, c)
			if d.Requested || (d.Desired.Rendition == Native) != actual {
				t.Fatalf("physical candidate/new acquisition conflated: actual%v %+v", actual, d)
			}
			v.BeforeSend(now, 1, d.Desired, c)
			left, _ := q.QueuedKey(Native)
			if (left > 0) != actual || v.Operating().Rendition != Low || q.authorization != nil {
				t.Fatal("unknown physical writer changed ownership or lost a real candidate")
			}
		})
	}
}
