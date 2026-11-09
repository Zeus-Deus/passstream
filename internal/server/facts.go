package server

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"unicode"

	"github.com/oschwald/maxminddb-golang"
	"github.com/passstream/passstream/internal/hostview"
	"github.com/pion/webrtc/v4"
)

const geoAttribution = "IP Geolocation by DB-IP (https://db-ip.com), DB-IP Lite City + ASN, CC BY 4.0"

type factsDB struct {
	city, asn *maxminddb.Reader
	proxies   []netip.Prefix
}

func openFacts(c Config) (*factsDB, error) {
	f := &factsDB{}
	for _, v := range strings.Split(c.TrustProxy, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		p, e := netip.ParsePrefix(v)
		if e != nil {
			a, err := netip.ParseAddr(v)
			if err != nil {
				return nil, errors.New("invalid trusted proxy CIDR")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		f.proxies = append(f.proxies, p)
	}
	for _, item := range []struct {
		path string
		dst  **maxminddb.Reader
	}{{c.GeoIPCity, &f.city}, {c.GeoIPASN, &f.asn}} {
		if item.path == "" {
			continue
		}
		r, e := maxminddb.Open(item.path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			f.close()
			return nil, errors.New("cannot open GeoIP database")
		}
		*item.dst = r
	}
	return f, nil
}
func (f *factsDB) close() {
	if f.city != nil {
		_ = f.city.Close()
	}
	if f.asn != nil {
		_ = f.asn.Close()
	}
}
func (f *factsDB) trusted(a netip.Addr) bool {
	for _, p := range f.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
func (f *factsDB) address(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	a, e := netip.ParseAddr(host)
	if e != nil {
		return ""
	}
	a = a.Unmap()
	if !f.trusted(a) {
		return a.String()
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		next, e := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if e != nil {
			return a.String()
		}
		a = next.Unmap()
		if !f.trusted(a) {
			break
		}
	}
	return a.String()
}
func (f *factsDB) geo(ip string) *hostview.Geo {
	if f.city == nil && f.asn == nil {
		return nil
	}
	g := &hostview.Geo{}
	var city struct {
		Country struct {
			ISO string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
	}
	if f.city != nil && f.city.Lookup(net.ParseIP(ip), &city) == nil {
		if city.Country.ISO != "" {
			g.Country = &city.Country.ISO
		}
		if n := clean(city.City.Names["en"], 64, 256); n != "" {
			g.City = &n
		}
	}
	var asn struct {
		Number uint64 `maxminddb:"autonomous_system_number"`
		Org    string `maxminddb:"autonomous_system_organization"`
	}
	if f.asn != nil && f.asn.Lookup(net.ParseIP(ip), &asn) == nil {
		if asn.Number != 0 {
			g.ASN = &asn.Number
		}
		if n := clean(asn.Org, 64, 256); n != "" {
			g.ASOrg = &n
		}
	}
	return g
}
func clean(v string, chars, bytes int) string {
	var b strings.Builder
	n := 0
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n >= chars || b.Len()+len(string(r)) > bytes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
func device(r *http.Request) *string {
	ua := r.UserAgent()
	os := strings.Trim(r.Header.Get("Sec-CH-UA-Platform"), "\"")
	if os == "" {
		for _, x := range []struct{ find, name string }{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Windows", "Windows"}, {"Macintosh", "macOS"}, {"Linux", "Linux"}} {
			if strings.Contains(ua, x.find) {
				os = x.name
				break
			}
		}
	}
	browser := ""
	hints := r.Header.Get("Sec-CH-UA")
	for _, x := range []struct{ find, name string }{{"Microsoft Edge", "Edge"}, {"Google Chrome", "Chrome"}, {"Chromium", "Chromium"}} {
		if strings.Contains(hints, x.find) {
			browser = x.name
			break
		}
	}
	if browser == "" {
		for _, x := range []struct{ find, name string }{{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
			if strings.Contains(ua, x.find) {
				browser = x.name
				break
			}
		}
	}
	model := strings.Trim(r.Header.Get("Sec-CH-UA-Model"), "\"")
	parts := []string{}
	if model != "" {
		parts = append(parts, model)
	}
	if os != "" {
		parts = append(parts, os)
	}
	if browser != "" {
		parts = append(parts, browser)
	}
	v := clean(strings.Join(parts, " · "), 64, 256)
	if v == "" {
		return nil
	}
	return &v
}
func (s *Server) transportFacts(p *signalPeer, c hostview.Connection) hostview.Connection {
	report := p.media.PC.GetStats()
	for _, item := range report {
		pair, ok := item.(webrtc.ICECandidatePairStats)
		if !ok || !pair.Nominated || pair.State != webrtc.StatsICECandidatePairStateSucceeded {
			continue
		}
		remote, ok := report[pair.RemoteCandidateID].(webrtc.ICECandidateStats)
		if !ok {
			continue
		}
		path := remote.Protocol
		if remote.CandidateType == webrtc.ICECandidateTypeRelay {
			switch remote.RelayProtocol {
			case "tcp":
				path = "turn_tcp"
			case "tls":
				path = "turn_tls"
			case "udp":
				path = "turn_udp"
			default:
				path = ""
			}
		}
		if path == "udp" || path == "tcp" || strings.HasPrefix(path, "turn_") {
			c.Path = &path
		}
		rtt := pair.CurrentRoundTripTime * 1000
		layer := uint64(p.media.Layer.Load())
		c.Stats = &hostview.Stats{RTTMS: &rtt, TemporalLayer: &layer}
		measuredRTT, loss, wire, ready := p.media.ObservedStats()
		if ready {
			c.Stats.RTTMS = &measuredRTT
			c.Stats.LossPct = &loss
		}
		now := timeNowMS()
		if p.wireAt > 0 && now > p.wireAt && wire >= p.wireBytes {
			kbps := (wire - p.wireBytes) * 8 / uint64(now-p.wireAt)
			c.Stats.BitrateKbps = &kbps
		}
		p.wireAt = now
		p.wireBytes = wire
		c.StatsAt = &now
		break
	}
	return c
}
