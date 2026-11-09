// Package passglue publishes an owner-selected artifact to one configured Pass
// service. Keys are read at use time, and redirects and automatic retries are off.
package passglue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/host/keyfile"
	"github.com/passstream/passstream/internal/host/proto"
)

const DefaultPassFile = "https://passfile.space"
const DefaultPassPage = "https://passpage.space"

type Config struct {
	PassFile, PassPage       string
	PassFileKey, PassPageKey string
	Transport                http.RoundTripper
}

// Base restricts plaintext credentials to literal loopback test/development
// servers. No userinfo, query, fragments or paths may redirect the destination.
func Base(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", proto.E("invalid_argument", "invalid Pass service base URL")
	}
	a, e := netip.ParseAddr(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && e == nil && a.IsLoopback()) {
		return "", proto.E("invalid_argument", "Pass services require HTTPS (HTTP allowed on literal loopback)")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// Destination is fixed by local config, never request arguments.
func (c Config) Destination(service string) (string, error) {
	base := c.PassFile
	if service == "passfile" && base == "" {
		base = DefaultPassFile
	}
	if service == "passpage" {
		base = c.PassPage
		if base == "" {
			base = DefaultPassPage
		}
	} else if service != "passfile" {
		return "", proto.E("invalid_argument", "unknown Pass service")
	}
	return Base(base)
}

// 32 MiB in 45 s requires 5.97 Mbit/s before overhead. Give transfers an
// additional second per 128 KiB (301 s at 32 MiB), with a 45 s setup margin.
func UploadTimeout(size int) time.Duration {
	return 45*time.Second + time.Duration((size+(128<<10)-1)/(128<<10))*time.Second
}
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
func (c Config) Upload(ctx context.Context, service, kind string, data []byte) (string, error) {
	base, keyPath, method, path, mime := c.PassFile, c.PassFileKey, http.MethodPut, "/api/v1/files", "image/png"
	filename := "snapshot.png"
	if kind == "recording" {
		filename, mime = "recording.webm", "video/webm"
	} else if kind != "snapshot" {
		return "", proto.E("invalid_argument", "unknown artifact")
	}
	if service == "passpage" {
		if kind != "snapshot" {
			return "", proto.E("invalid_argument", "PassPage accepts snapshots only")
		}
		base, keyPath, method, path = c.PassPage, c.PassPageKey, http.MethodPost, "/api/shares"
		if base == "" {
			base = DefaultPassPage
		}
	} else if service != "passfile" {
		return "", proto.E("invalid_argument", "unknown Pass service")
	}
	if base == "" {
		base = DefaultPassFile
	}
	base, err := Base(base)
	if err != nil {
		return "", err
	}
	key, status := keyfile.Read(keyPath)
	if status != keyfile.OK {
		return "", proto.E("key_"+status, "the selected Pass service key is %s", status)
	}
	payload := data
	if service == "passpage" {
		payload, mime, err = page(data)
		if err != nil {
			return "", proto.E("internal", "cannot prepare snapshot page")
		}
	} else {
		path += "?filename=" + url.QueryEscape(filename)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return "", proto.E("invalid_argument", "invalid upload request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mime)
	if service == "passfile" {
		sum := sha256.Sum256(data)
		req.Header.Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
	}
	client := http.Client{Transport: c.Transport, Timeout: UploadTimeout(len(payload)), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	// Transport errors and service bodies can contain credentials or payloads.
	if err != nil {
		return "", proto.E("upload_failed", "Pass service upload failed; inspect the service before retrying")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", proto.E("upload_failed", "Pass service returned HTTP %d; inspect the service before retrying", resp.StatusCode)
	}
	var result struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result) != nil {
		return "", proto.E("upload_failed", "Pass service returned an invalid result")
	}
	u, e := url.Parse(result.URL)
	origin, _ := url.Parse(base)
	if e != nil || strings.Contains(result.URL, key) || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || !sameOrigin(u, origin) {
		return "", proto.E("upload_failed", "Pass service returned no valid link")
	}
	return result.URL, nil
}

func page(data []byte) ([]byte, string, error) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"index.html", []byte(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>PassStream snapshot</title><style>body{margin:0;background:#171717}img{display:block;max-width:100%;height:auto;margin:auto}</style><img src="snapshot.png" alt="Shared screen snapshot"></html>`)},
		{"snapshot.png", data},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0600, Size: int64(len(f.body))}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	p, err := mw.CreateFormFile("archive", "snapshot.tar.gz")
	if err != nil {
		return nil, "", err
	}
	if _, err = p.Write(archive.Bytes()); err != nil {
		return nil, "", err
	}
	if err = mw.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), mw.FormDataContentType(), nil
}
