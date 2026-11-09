package daemon

import (
	"context"
	"crypto/sha256"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/state"
)

func TestOwnerHTTPTokenScopeExpiryAndBearerSyntax(t *testing.T) {
	st := &stream{rec: &state.Record{StreamID: "stream-a"}}
	token := "local_owner_token_needle"
	hash := sha256.Sum256([]byte(token))
	d := &Daemon{cfg: Config{Port: DefaultPort, PeerCheck: PeerLaunchFile}, ctx: context.Background(), st: st, featureTokens: map[string]featureToken{string(hash[:]): {stream: st, expires: time.Now().Add(time.Minute)}}}
	request := func(auth, origin, stream string) int {
		r := httptest.NewRequest("GET", d.origin()+"/local/v1/frame", nil)
		r.Header.Set("Authorization", auth)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if stream != "" {
			r.Header.Set("X-PassStream-Stream", stream)
		}
		w := httptest.NewRecorder()
		d.serveFeatures(w, r)
		if strings.Contains(w.Body.String(), token) {
			t.Fatal("credential echoed")
		}
		return w.Code
	}
	if request(token, "", "") != 401 {
		t.Fatal("accepted missing Bearer prefix")
	}
	if request("Bearer "+token, "", "") != 200 {
		t.Fatal("valid credential refused")
	}
	if request("Bearer "+token, "", "stream-b") != 409 {
		t.Fatal("wrong stream accepted")
	}
	d.featureTokens[string(hash[:])] = featureToken{stream: st, expires: time.Now().Add(-time.Second)}
	if request("Bearer "+token, "", "") != 401 {
		t.Fatal("expired credential accepted")
	}
	d.featureTokens[string(hash[:])] = featureToken{stream: st, expires: time.Now().Add(time.Minute)}
	d.st = &stream{rec: &state.Record{StreamID: "stream-b"}}
	if request("Bearer "+token, "", "") != 401 {
		t.Fatal("prior stream credential accepted")
	}
	if _, e := d.featureDispatch(context.Background(), "frame", nil, st); e == nil {
		t.Fatal("old stream dispatched against new share")
	}
}
