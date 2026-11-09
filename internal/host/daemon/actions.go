package daemon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
)

type localAction struct {
	fingerprint [32]byte
	done        chan struct{}
	at          time.Time
	data        any
	err         error
	retryable   bool
}

func localActionID(op string, args map[string]any) (string, error) {
	v, present := args["action_id"]
	if !present {
		return "", nil
	}
	switch op {
	case "kick", "unfreeze", "pin.rotate", "pin.set", "knock.approve":
	default:
		return "", proto.E("invalid_argument", "action_id is not supported for this op")
	}
	id, ok := v.(string)
	if !ok || !serverapi.ValidActionID(id) {
		return "", proto.E("invalid_argument", "invalid action_id")
	}
	return id, nil
}

func withoutActionID(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if k != "action_id" {
			out[k] = v
		}
	}
	return out
}

func actionForwardID(stream, action string) string {
	h := sha256.Sum256([]byte(stream + ":" + action))
	return "a" + hex.EncodeToString(h[:])
}

var actionFingerprintKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}()

func actionFingerprint(op string, args map[string]any) [32]byte {
	b, _ := json.Marshal([]any{op, args})
	mac := hmac.New(sha256.New, actionFingerprintKey)
	mac.Write(b)
	var fp [32]byte
	copy(fp[:], mac.Sum(nil))
	return fp
}

// actionForward shares an in-flight local action and remembers terminal answers.
// Arguments may contain a PIN: retain only a process-keyed HMAC, never plaintext
// or an unkeyed digest that permits offline PIN enumeration.
func (d *Daemon) actionForward(op string, args map[string]any, by, id string) (any, error) {
	if id == "" {
		return d.forward(op, args, by, "", nil)
	}
	fp := actionFingerprint(op, args)
	d.mu.Lock()
	st := d.st
	if st == nil || st.ending {
		d.mu.Unlock()
		return nil, proto.E("not_live", "no active share")
	}
	if st.actions == nil {
		st.actions = map[string]*localAction{}
	}
	for k, a := range st.actions {
		select {
		case <-a.done:
			if time.Since(a.at) > 10*time.Minute {
				delete(st.actions, k)
			}
		default:
		}
	}
	if a := st.actions[id]; a != nil {
		if a.fingerprint != fp {
			d.mu.Unlock()
			return nil, proto.E("idempotency_conflict", "action_id reused for another command")
		}
		select {
		case <-a.done:
			if !a.retryable {
				d.mu.Unlock()
				return a.data, a.err
			}
			// Preserve the fingerprint across retryable attempts, while each
			// attempt's waiters keep their immutable original answer.
			delete(st.actions, id)
		default:
			d.mu.Unlock()
			select {
			case <-a.done:
				return a.data, a.err
			case <-d.ctx.Done():
				return nil, proto.E("sender_timeout", "daemon stopped")
			}
		}
	}
	if len(st.actions) >= 256 {
		// Evict the oldest completed action; never duplicate an in-flight one.
		oldest := ""
		for k, a := range st.actions {
			select {
			case <-a.done:
				if oldest == "" || a.at.Before(st.actions[oldest].at) {
					oldest = k
				}
			default:
			}
		}
		if oldest == "" {
			d.mu.Unlock()
			return nil, proto.E("rate_limited", "too many actions in flight")
		}
		delete(st.actions, oldest)
	}
	a := &localAction{fingerprint: fp, done: make(chan struct{}), at: time.Now()}
	st.actions[id] = a
	d.mu.Unlock()
	data, err := d.forward(op, args, by, id, st)
	d.mu.Lock()
	a.data, a.err, a.at = data, err, time.Now()
	if err != nil {
		e := proto.AsError(err)
		switch e.Code {
		case "sender_timeout", "sender_offline", "internal", "browser_failed", "rate_limited", "rotation_pending", "storage":
			if e.Code != "storage" || e.Details["pending"] != true && e.Details["applied"] != true {
				a.retryable = true
			}
		}
	}
	close(a.done)
	d.mu.Unlock()
	return data, err
}
