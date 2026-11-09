package passglue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const needle = "FAKE_SERVICE_KEY_NEVER_LOG_THIS_014873"

func key(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(p, []byte(needle+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestPassFileExactRequests(t *testing.T) {
	for _, kind := range []string{"snapshot", "recording"} {
		t.Run(kind, func(t *testing.T) {
			data := []byte("PRIVATE_ARTIFACT_NEEDLE")
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				name, mime := "snapshot.png", "image/png"
				if kind == "recording" {
					name, mime = "recording.webm", "video/webm"
				}
				if r.Method != "PUT" || r.URL.RequestURI() != "/api/v1/files?filename="+name || r.Header.Get("Authorization") != "Bearer "+needle || r.Header.Get("Content-Type") != mime {
					t.Errorf("request shape or auth mismatch")
				}
				b, _ := io.ReadAll(r.Body)
				if !bytes.Equal(b, data) {
					t.Error("wrong payload")
				}
				sum := sha256.Sum256(data)
				if r.Header.Get("X-Content-SHA256") != hex.EncodeToString(sum[:]) {
					t.Error("missing checksum")
				}
				fmt.Fprint(w, `{"url":"http://`+r.Host+`/s/result"}`)
			}))
			defer s.Close()
			u, e := (Config{PassFile: s.URL, PassFileKey: key(t)}).Upload(context.Background(), "passfile", kind, data)
			if e != nil || u != s.URL+"/s/result" || calls != 1 {
				t.Fatal(u, e, calls)
			}
		})
	}
}

func TestPassPageMultipartTar(t *testing.T) {
	data := []byte("PRIVATE_PNG_NEEDLE")
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.RequestURI() != "/api/shares" || r.Header.Get("Authorization") != "Bearer "+needle {
			t.Error("request shape or auth mismatch")
		}
		mr, e := r.MultipartReader()
		if e != nil {
			t.Error(e)
			return
		}
		p, e := mr.NextPart()
		if e != nil || p.FormName() != "archive" || p.FileName() != "snapshot.tar.gz" {
			t.Error("wrong archive part")
			return
		}
		gz, e := gzip.NewReader(p)
		if e != nil {
			t.Error(e)
			return
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		files := map[string][]byte{}
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Error(e)
				return
			}
			b, _ := io.ReadAll(tr)
			files[h.Name] = b
		}
		if len(files) != 2 || !bytes.Equal(files["snapshot.png"], data) || !bytes.Contains(files["index.html"], []byte(`src="snapshot.png"`)) {
			t.Error("invalid page archive")
		}
		if _, e := mr.NextPart(); e != io.EOF {
			t.Error("unexpected multipart field")
		}
		fmt.Fprint(w, `{"url":"http://`+r.Host+`/v/result/"}`)
	}))
	defer s.Close()
	u, e := (Config{PassPage: s.URL, PassPageKey: key(t)}).Upload(context.Background(), "passpage", "snapshot", data)
	if e != nil || u != s.URL+"/v/result/" || calls != 1 {
		t.Fatal(u, e, calls)
	}
}

func TestRedirectNeverForwardsKeyOrPayload(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer source.Close()
	_, e := (Config{PassFile: source.URL, PassFileKey: key(t)}).Upload(context.Background(), "passfile", "snapshot", []byte("private"))
	if e == nil || calls != 0 {
		t.Fatal("followed redirect", e, calls)
	}
}

func TestServiceErrorsNeverEchoSecrets(t *testing.T) {
	for _, status := range []int{401, 500, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, needle+" PRIVATE_ARTIFACT_NEEDLE")
			}))
			defer s.Close()
			_, e := (Config{PassFile: s.URL, PassFileKey: key(t)}).Upload(context.Background(), "passfile", "snapshot", []byte("PRIVATE_ARTIFACT_NEEDLE"))
			if e == nil || strings.Contains(e.Error(), needle) || strings.Contains(e.Error(), "PRIVATE_ARTIFACT") {
				t.Fatal("unsafe error")
			}
		})
	}
}

func TestReadsKeyAtUseTimeAndGuardsPermissions(t *testing.T) {
	p := key(t)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer changed_key" {
			t.Error("stale key")
		}
		fmt.Fprint(w, `{"url":"http://`+r.Host+`/s/result"}`)
	}))
	defer s.Close()
	c := Config{PassFile: s.URL, PassFileKey: p}
	os.WriteFile(p, []byte("changed_key"), 0600)
	if _, e := c.Upload(context.Background(), "passfile", "snapshot", nil); e != nil {
		t.Fatal(e)
	}
	os.Chmod(p, 0644)
	if _, e := c.Upload(context.Background(), "passfile", "snapshot", nil); e == nil || calls != 1 {
		t.Fatal("unsafe key used")
	}
}

func TestRejectUnsafeDestinationsAndUnsupportedArtifacts(t *testing.T) {
	for _, base := range []string{"http://files.example", "https://user:secret@example.test", "https://example.test/path", "https://example.test?secret", "file:///tmp/x", "http://localhost:1234"} {
		if _, e := Base(base); e == nil {
			t.Fatal("accepted unsafe destination")
		}
	}
	for _, base := range []string{DefaultPassFile, DefaultPassPage, "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if _, e := Base(base); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := (Config{}).Upload(context.Background(), "passpage", "recording", nil); e == nil {
		t.Fatal("accepted unsupported artifact")
	}
}

func TestReturnedLinkMustMatchServiceOrigin(t *testing.T) {
	for _, link := range []string{"https://another.example/s/result", "http://127.0.0.1:1/s/result", "javascript:alert(1)"} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"url":%q}`, link) }))
		_, e := (Config{PassFile: s.URL, PassFileKey: key(t)}).Upload(context.Background(), "passfile", "snapshot", []byte("png"))
		s.Close()
		if e == nil {
			t.Fatal("accepted a link from a different origin")
		}
	}
}

func TestUploadTimeoutScalesWithPayload(t *testing.T) {
	if UploadTimeout(0) != 45*time.Second || UploadTimeout(32<<20) != 301*time.Second {
		t.Fatal("upload budget does not allow 128 KiB/s plus setup margin")
	}
	if UploadTimeout(1<<20) >= UploadTimeout(32<<20) {
		t.Fatal("timeout does not scale")
	}
}
