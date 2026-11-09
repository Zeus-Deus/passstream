package sfu

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// This controlled seam honors recovery without exercising real upgrade
// eligibility; the independent approach review owns that separate question.
type phaseProbeAllocation struct{ desired ctrl.Option }

func (a *phaseProbeAllocation) Decide(_ time.Time, in ctrl.AllocInput) ctrl.Allocation {
	if in.Recovering {
		return forcedAllocation(in, ctrl.Option{Rendition: ctrl.Low, TID: 2})
	}
	return forcedAllocation(in, a.desired)
}
func (*phaseProbeAllocation) OnApplied(time.Time, ctrl.Option) {}
func (*phaseProbeAllocation) OnGeneration(time.Time, uint64)   {}

// Opt-in owning counterexample, not a new canonical gate or browser cohort.
// It separates a current-rate admission/pacing interruption from packet loss,
// broken dependencies and the KeyCompleted tail. The slow arm is expected red
// against the existing >500ms freeze definition until a design is reviewed.
func TestKeyPhaseProbe(t *testing.T) {
	if os.Getenv("SFU_KEY_PHASE_PROBE") != "1" {
		t.Skip("set SFU_KEY_PHASE_PROBE=1 for the retained slow-key counterexample")
	}
	for _, rate := range []int{5_000_000, 1_500_000} {
		name := "fast-control"
		if rate == 1_500_000 {
			name = "slow-key"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			v, w := h.viewer()
			lowContinuity := &ctrl.Dependency{}
			lowEmit := h.asm[ctrl.Low].emit
			h.asm[ctrl.Low].emit = func(now time.Time, p *picture) {
				if !lowContinuity.Allow(p.info, 2) {
					t.Fatal("continuing low source is not a valid dependency chain")
				}
				lowEmit(now, p)
			}
			h.asm[ctrl.Low].lose = func(time.Time, ctrl.FrameInfo) { t.Fatal("low control lost a picture") }
			nativeLosses := 0
			nativeLose := h.asm[ctrl.Native].lose
			h.asm[ctrl.Native].lose = func(now time.Time, info ctrl.FrameInfo) {
				nativeLosses++
				nativeLose(now, info)
			}
			allocation := &phaseProbeAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
			v.mu.Lock()
			v.ctl.Est = &fixedRepairEstimator{ctrl.FixedEstimator{Rate: rate}}
			v.ctl.Probe = ctrl.NoProber{}
			v.ctl.Alloc = allocation
			v.step(h.now)
			v.ctl.Pacer.SetCluster(nil)
			v.mu.Unlock()
			lastComplete := time.Time{}
			nativeComplete := time.Time{}
			deltaComplete := time.Time{}
			deltaFirstDeparture := time.Time{}
			deltaDeadline := time.Time{}
			nativeTimestamp := uint32(0)
			deltaTimestamp := uint32(0)
			projectedTimestamp := func(key bool) uint32 {
				for _, o := range v.pending {
					if o.kind == outMedia && o.frame.info.Rendition == ctrl.Native && o.frame.info.Key == key {
						copy := v.rewrite
						copy.start([]*rtp.Packet{o.pkt}, o.frame.capture, o.frame.source)
						return o.pkt.Timestamp + copy.tsOffset
					}
				}
				return 0
			}
			lowOffered, lowBeforeKey := 0, 0
			otherCompletions := 0
			tailChecked, tailAccepted := false, false
			pump := func(duration time.Duration) {
				for end := h.now.Add(duration); h.now.Before(end); h.now = h.now.Add(time.Millisecond) {
					v.mu.Lock()
					v.step(h.now)
					if !nativeComplete.IsZero() && !tailChecked && !h.now.Before(nativeComplete.Add(100*time.Millisecond)) {
						tailAccepted = v.ctl.OnDecoderStall(h.now)
						tailChecked = true
					}
					batch := v.drain(h.now, nil)
					v.mu.Unlock()
					for _, p := range batch {
						if _, err := p.writer.WriteRTP(&p.h, p.payload); err != nil {
							t.Fatal(err)
						}
						if deltaTimestamp != 0 && p.h.Timestamp == deltaTimestamp && deltaFirstDeparture.IsZero() {
							deltaFirstDeparture = h.now
							v.mu.Lock()
							deltaDeadline = v.cache.get(p.h.SequenceNumber).deadline
							v.mu.Unlock()
						}
						if p.h.Marker {
							if nativeTimestamp != 0 && p.h.Timestamp == nativeTimestamp {
								nativeComplete = h.now
								lowBeforeKey = lowOffered
							} else if deltaTimestamp != 0 && p.h.Timestamp == deltaTimestamp {
								deltaComplete = h.now
							} else if nativeTimestamp == 0 {
								lastComplete = h.now
							} else {
								otherCompletions++
							}
						}
					}
				}
			}
			h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
			pump(100 * time.Millisecond)
			h.send(ctrl.Low, desc{flexible: true, pdiff: []uint8{1}}, 1, 300)
			pump(10 * time.Millisecond)
			if lastComplete.IsZero() {
				t.Fatal("low control never completed")
			}
			v.mu.Lock()
			allocation.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}
			v.step(h.now)
			v.mu.Unlock()
			admitted := h.now
			// Flexible key: 235200 protected-body bytes + 200*4 descriptor
			// bytes + 200*(12+50) wire bytes = the original 248400B fixture.
			h.send(ctrl.Native, desc{key: true, flexible: true}, 200, 1176)
			v.mu.Lock()
			nativeTimestamp = projectedTimestamp(true)
			v.mu.Unlock()
			activeAccepted := false
			activeChecked := false
			deltaOffered, deltaClock := time.Time{}, time.Time{}
			deltaWire := 0
			for deadline := admitted.Add(3 * time.Second); (deltaComplete.IsZero() || !tailChecked) && h.now.Before(deadline); {
				// A continuing valid low source is available, but forwarding has
				// already switched to the native key. No packet loss/writer stall.
				h.send(ctrl.Low, desc{flexible: true, pdiff: []uint8{1}}, 1, 300)
				lowOffered++
				if deltaOffered.IsZero() && h.now.Sub(admitted) >= 66*time.Millisecond {
					deltaOffered = h.now
					// Controlled first TL0 delta references the key. Protected
					// body 151406B illustrates the observed 151386B plaintext
					// +20B SFrame case; it is not a distribution change.
					packets := h.packets(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 130, 1164)
					packets[len(packets)-1].Payload = append(packets[len(packets)-1].Payload, make([]byte, 86)...)
					for _, p := range packets {
						deltaWire += p.MarshalSize() + wireOverhead
						h.asm[ctrl.Native].push(h.now, p)
					}
					deltaClock = h.asm[ctrl.Native].capture
					v.mu.Lock()
					deltaTimestamp = projectedTimestamp(false)
					v.mu.Unlock()
					// Some key packets have already departed; check delta
					// ownership directly rather than inferring queue size.
					v.mu.Lock()
					n := 0
					for _, o := range v.pending {
						if o.kind == outMedia && o.frame.info.Rendition == ctrl.Native && !o.frame.info.Key {
							n++
						}
					}
					v.mu.Unlock()
					if deltaTimestamp == nativeTimestamp || n != 130 {
						t.Fatal("first dependent delta was not admitted intact")
					}
				}
				pump(66 * time.Millisecond)
				if !activeChecked && h.now.Sub(admitted) >= 250*time.Millisecond && nativeComplete.IsZero() {
					v.mu.Lock()
					activeAccepted = v.ctl.OnDecoderStall(h.now)
					v.mu.Unlock()
					activeChecked = true
				}
			}
			if nativeComplete.IsZero() || nativeComplete.Sub(admitted) >= ctrl.KeyEnvelope || deltaComplete.IsZero() {
				t.Fatal("key/delta did not complete within the bounded fixture")
			}
			if nativeLosses != 0 || otherCompletions != 0 || !deltaComplete.Before(deltaDeadline) {
				t.Fatal("measurement was lossy, forwarded another picture, or delta violated its effective queue deadline")
			}
			// After both measured departures, the real ingress assembler sees
			// an incomplete native base. The next picture closes it, invoking
			// viewer.ingressLost -> OnBroken, not a direct RoomKeys request.
			brokenAt := h.now
			incomplete := h.packets(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 2, 300)
			h.asm[ctrl.Native].push(h.now, incomplete[0])
			pump(66 * time.Millisecond)
			h.send(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 1, 300)
			v.mu.Lock()
			brokenWantsLow := v.want == ctrl.Low
			// Even a subsequent delta referring to the last intact delta
			// (before the missing base) is rejected once the chain is broken.
			oldBaseDenied := !v.ctl.Cont.Allow(ctrl.FrameInfo{Rendition: ctrl.Native, Picture: uint64(h.pid[ctrl.Native]) + 1, Flexible: true, Refs: []uint64{uint64(h.pid[ctrl.Native]) - 2}}, 2)
			v.mu.Unlock()
			// The already-assembled low key spent its ordinary shared interval.
			// Recovery must wait for that budget, then use a genuine new key.
			dueBeforeBudget := h.r.keys.Due(h.now)
			if budgetAt := admitted.Add(ctrl.KeyInterval); h.now.Before(budgetAt) {
				h.now = budgetAt
			}
			due := h.r.keys.Due(h.now)
			if len(dueBeforeBudget) != 0 || len(due) != 1 || due[0] != ctrl.Low {
				t.Fatalf("broken recovery bypassed/lost shared low-key budget: before=%v due=%v", dueBeforeBudget, due)
			}
			beforeRecovery := len(w.written())
			// The forced native timing seam must not override this separate
			// recovery control after RecoveryQuiet expires while awaiting the
			// room budget. Timing measurements above are already complete.
			allocation.desired = ctrl.Option{Rendition: ctrl.Low, TID: 2}
			h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
			pump(20 * time.Millisecond)
			v.mu.Lock()
			recoveredLow := v.ctl.Applied().Rendition == ctrl.Low && len(w.written()) > beforeRecovery
			v.mu.Unlock()
			gap := nativeComplete.Sub(lastComplete)
			row := map[string]any{
				"rate_bps": rate, "key_wire_bytes": 248400, "key_departure_ms": nativeComplete.Sub(admitted).Milliseconds(), "complete_picture_gap_ms": gap.Milliseconds(),
				"delta_protected_body_bytes": 151406, "delta_wire_bytes": deltaWire, "delta_offered_ms": deltaOffered.Sub(admitted).Milliseconds(),
				"delta_assembler_clock_ms": float64(deltaClock.Sub(admitted)) / float64(time.Millisecond), "delta_departure_ms": deltaComplete.Sub(admitted).Milliseconds(),
				"delta_age_from_offer_ms": deltaComplete.Sub(deltaOffered).Milliseconds(), "delta_age_from_assembler_clock_ms": float64(deltaComplete.Sub(deltaClock)) / float64(time.Millisecond),
				"delta_first_departure_ms": deltaFirstDeparture.Sub(admitted).Milliseconds(), "delta_effective_queue_deadline_ms": deltaDeadline.Sub(admitted).Milliseconds(),
				"key_to_delta_departure_ms": deltaComplete.Sub(nativeComplete).Milliseconds(), "valid_low_pictures_offered_until_key": lowBeforeKey, "valid_low_pictures_offered_until_delta": lowOffered,
				"active_decoder_request_accepted": activeAccepted, "tail_decoder_request_accepted": tailAccepted, "tail_offset_ms": 100,
				"broken_input_ms": brokenAt.Sub(admitted).Milliseconds(), "assembler_broken_wants_low": brokenWantsLow, "broken_chain_denies_delta": oldBaseDenied,
				"native_assembler_loss_callbacks_in_broken_control": nativeLosses,
				"low_key_emitted_before_budget":                     len(dueBeforeBudget) != 0, "one_low_key_emitted_at_budget": len(due) == 1 && due[0] == ctrl.Low, "new_low_key_departed_and_applied": recoveredLow,
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("KEY_PHASE_JSON %s", encoded)
			if !activeChecked || activeAccepted || !tailChecked || tailAccepted || nativeLosses == 0 || !brokenWantsLow || !oldBaseDenied || !recoveredLow {
				t.Fatal("phase/anti-spam control did not match existing policy")
			}
			if gap > 500*time.Millisecond {
				t.Errorf("complete-picture interruption %v exceeds existing500ms freeze definition BEFORE KeyCompleted tail can act", gap)
			}
			if deltaComplete.Sub(deltaClock) > ctrl.FrameDeadline {
				t.Errorf("first dependent delta departs aged %v, beyond existing1s freshness definition despite lossless delivery", deltaComplete.Sub(deltaClock))
			}
		})
	}
}
