package sfusim

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Outcome is one property's result over all seeds.
type Outcome struct {
	ID       string
	Seeds    int
	Passed   int
	Need     int
	Pooled   *Verdict
	Failing  []uint64
	Examples []string
	Elapsed  time.Duration
}

func (o Outcome) Pass() bool {
	if o.Pooled != nil {
		return o.Pooled.Pass
	}
	return o.Passed >= o.Need
}

func seedCount(short bool) int {
	if short {
		return 5
	}
	return 100
}

// Evaluate runs a property over seeds 1..n in parallel.
func Evaluate(p Property, c Controller, short bool) Outcome {
	n := seedCount(short)
	start := time.Now()
	results := make([]*Result, n)
	controls := make([]*Result, n)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				seed := uint64(i + 1)
				results[i] = Run(p.Build(short), seed, c)
				if p.Control != nil {
					controls[i] = Run(p.Control(short), seed, c)
				}
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	o := Outcome{ID: p.ID, Seeds: n, Need: n}
	if p.Pooled != nil {
		v := p.Pooled(results)
		o.Pooled = &v
	} else {
		for i, r := range results {
			v := p.Check(r, controls[i])
			if v.Pass {
				o.Passed++
			} else {
				o.Failing = append(o.Failing, r.Seed)
				if len(o.Examples) < 3 {
					o.Examples = append(o.Examples, v.Detail)
				}
			}
		}
	}
	o.Elapsed = time.Since(start)
	return o
}

func report(t *testing.T, o Outcome) {
	if o.Pooled != nil {
		t.Logf("%-28s pooled %v: %s (%v)", o.ID, o.Pass(), o.Pooled.Detail, o.Elapsed.Round(time.Millisecond))
		return
	}
	t.Logf("%-28s %d/%d pass (need %d) failing seeds %v %v (%v)", o.ID, o.Passed, o.Seeds, o.Need, head(o.Failing), o.Examples, o.Elapsed.Round(time.Millisecond))
}

func head(s []uint64) []uint64 {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// TestTierA is the Tier A gate (docs/GATES.md). It enforces only once
// ctrl.Stub is false.
func TestTierA(t *testing.T) {
	start := time.Now()
	for _, p := range Properties() {
		t.Run(p.ID, func(t *testing.T) {
			o := Evaluate(p, Default, testing.Short())
			report(t, o)
			if !o.Pass() {
				if ctrl.Stub {
					t.Skip("not enforced while ctrl.Stub is true")
				}
				t.Fail()
			}
		})
	}
	t.Logf("Tier A suite: %v", time.Since(start).Round(time.Millisecond))
}

// TestOracleSatisfiable runs each property against an omniscient estimator
// to show the gate is attainable in the model. It reports and never fails,
// unless SFUSIM_ORACLE_ENFORCE=1.
func TestOracleSatisfiable(t *testing.T) {
	if os.Getenv("SFUSIM_ORACLE") == "" && !testing.Short() {
		t.Skip("set SFUSIM_ORACLE=1 to run the oracle over 100 seeds")
	}
	for _, p := range Properties() {
		t.Run(p.ID, func(t *testing.T) {
			o := Evaluate(p, Oracle(p.Capacity), testing.Short())
			report(t, o)
			if !o.Pass() && os.Getenv("SFUSIM_ORACLE_ENFORCE") == "1" {
				t.Fail()
			}
		})
	}
}
