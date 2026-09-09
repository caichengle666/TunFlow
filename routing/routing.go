package routing

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/proxy/direct"
)

const (
	ModeGlobal    = "global"
	ModeDirect    = "direct"
	ModeRules     = "rules"
	ModeBypass    = ModeRules
	DefaultProxy  = "proxy"
	DefaultDirect = "direct"
)

type RuleType int

const (
	RuleCIDR RuleType = iota
	RuleGeoIP
	RuleDomain
	RuleGeoSite
	RulePrivate
)

type Rule struct {
	Type   RuleType
	Value  string
	Prefix netip.Prefix
	CID    string
}

type Config struct {
	Mode        string
	DirectRules []string
	ProxyRules  []string
	Default     string
	GeoIPFile   string
}

// TrafficStats reports bytes sent/received through the SOCKS5 upstream.
type TrafficStats struct {
	UploadTotal       int64 `json:"uploadTotal"`
	DownloadTotal     int64 `json:"downloadTotal"`
	UploadPerSecond   int64 `json:"uploadPerSecond"`
	DownloadPerSecond int64 `json:"downloadPerSecond"`
}

type trafficCounter struct {
	uploadTotal   atomic.Int64
	downloadTotal atomic.Int64
	uploadTemp    atomic.Int64
	downloadTemp  atomic.Int64
	uploadRate    atomic.Int64
	downloadRate  atomic.Int64
	once          sync.Once
}

func (c *trafficCounter) ensureTicker() {
	c.once.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for range ticker.C {
				c.uploadRate.Store(c.uploadTemp.Swap(0))
				c.downloadRate.Store(c.downloadTemp.Swap(0))
			}
		}()
	})
}

func (c *trafficCounter) uploaded(n int) {
	if n <= 0 {
		return
	}
	c.ensureTicker()
	v := int64(n)
	c.uploadTotal.Add(v)
	c.uploadTemp.Add(v)
}

func (c *trafficCounter) downloaded(n int) {
	if n <= 0 {
		return
	}
	c.ensureTicker()
	v := int64(n)
	c.downloadTotal.Add(v)
	c.downloadTemp.Add(v)
}

func (c *trafficCounter) snapshot() TrafficStats {
	return TrafficStats{
		UploadTotal: c.uploadTotal.Load(), DownloadTotal: c.downloadTotal.Load(),
		UploadPerSecond: c.uploadRate.Load(), DownloadPerSecond: c.downloadRate.Load(),
	}
}

type Proxy struct {
	upstream proxy.Proxy
	direct   proxy.Proxy
	mode     string
	defaultD bool
	directR  []Rule
	proxyR   []Rule
	geoip    *GeoIPMatcher
	stats    trafficCounter
	mu       sync.RWMutex
}

func New(upstream proxy.Proxy, mode string, prefixes []string) (*Proxy, error) {
	return NewWithConfig(upstream, Config{Mode: mode, DirectRules: prefixes, Default: DefaultProxy})
}

func NewWithConfig(upstream proxy.Proxy, cfg Config) (*Proxy, error) {
	if upstream == nil {
		return nil, fmt.Errorf("routing: upstream proxy is nil")
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeGlobal
	}
	if cfg.Default == "" {
		cfg.Default = DefaultProxy
	}
	if cfg.Default != DefaultProxy && cfg.Default != DefaultDirect {
		return nil, fmt.Errorf("routing: invalid default route %q", cfg.Default)
	}
	if cfg.Mode != ModeGlobal && cfg.Mode != ModeDirect && cfg.Mode != ModeRules {
		return nil, fmt.Errorf("routing: unsupported mode %q", cfg.Mode)
	}
	d, err := direct.New()
	if err != nil {
		return nil, err
	}
	r := &Proxy{upstream: upstream, direct: d, mode: cfg.Mode, defaultD: cfg.Default == DefaultDirect}
	for _, raw := range cfg.DirectRules {
		rule, ok, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			r.directR = append(r.directR, rule)
		}
	}
	for _, raw := range cfg.ProxyRules {
		rule, ok, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			r.proxyR = append(r.proxyR, rule)
		}
	}
	if cfg.GeoIPFile != "" && hasGeoIPRule(r.directR, r.proxyR) {
		geo, err := LoadGeoIP(cfg.GeoIPFile)
		if err != nil {
			return nil, err
		}
		r.geoip = geo
	}
	return r, nil
}

func hasGeoIPRule(groups ...[]Rule) bool {
	for _, rules := range groups {
		for _, rule := range rules {
			if rule.Type == RuleGeoIP {
				return true
			}
		}
	}
	return false
}

func parseRule(raw string) (Rule, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Rule{}, false, nil
	}
	lower := strings.ToLower(raw)
	if lower == "private" || lower == "geoip:private" {
		return Rule{Type: RulePrivate, Value: "geoip:private"}, true, nil
	}
	if strings.HasPrefix(lower, "geoip:") {
		return Rule{Type: RuleGeoIP, Value: raw, CID: strings.ToLower(strings.TrimSpace(raw[len("geoip:"):]))}, true, nil
	}
	if strings.HasPrefix(lower, "geosite:") {
		return Rule{}, false, fmt.Errorf("routing: geosite rules are not supported without domain metadata")
	}
	if strings.HasPrefix(lower, "domain:") {
		return Rule{}, false, fmt.Errorf("routing: domain rules are not supported without domain metadata")
	}
	if strings.Contains(raw, "/") {
		pfx, err := netip.ParsePrefix(raw)
		if err != nil {
			return Rule{}, false, fmt.Errorf("routing: invalid CIDR %q: %w", raw, err)
		}
		return Rule{Type: RuleCIDR, Value: raw, Prefix: pfx}, true, nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return Rule{}, false, fmt.Errorf("routing: invalid rule %q: %w", raw, err)
	}
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	return Rule{Type: RuleCIDR, Value: raw, Prefix: netip.PrefixFrom(addr, bits)}, true, nil
}

func (r *Proxy) routeIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.directR {
		if r.matchIP(rule, ip) {
			return true
		}
	}
	for _, rule := range r.proxyR {
		if r.matchIP(rule, ip) {
			return false
		}
	}
	return r.defaultD
}

func (r *Proxy) matchIP(rule Rule, ip netip.Addr) bool {
	switch rule.Type {
	case RuleCIDR:
		return rule.Prefix.Contains(ip)
	case RulePrivate:
		return isPrivate(ip)
	case RuleGeoIP:
		return r.geoip != nil && r.geoip.ContainsCountry(ip, rule.CID)
	case RuleGeoSite, RuleDomain:
		return false
	default:
		return false
	}
}

func (r *Proxy) useDirect(metadata *M.Metadata) bool {
	if r.mode == ModeDirect {
		return true
	}
	if r.mode == ModeGlobal {
		return false
	}
	if metadata == nil || !metadata.DstIP.IsValid() {
		return r.defaultD
	}
	return r.routeIP(metadata.DstIP)
}

func (r *Proxy) Stats() TrafficStats { return r.stats.snapshot() }

func (r *Proxy) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	if r.useDirect(metadata) {
		return r.direct.DialContext(ctx, metadata)
	}
	conn, err := r.upstream.DialContext(ctx, metadata)
	if err != nil {
		return nil, err
	}
	return &statsConn{Conn: conn, c: &r.stats}, nil
}

func (r *Proxy) DialUDP(metadata *M.Metadata) (net.PacketConn, error) {
	if r.useDirect(metadata) {
		return r.direct.DialUDP(metadata)
	}
	conn, err := r.upstream.DialUDP(metadata)
	if err != nil {
		return nil, err
	}
	return &statsPacketConn{PacketConn: conn, c: &r.stats}, nil
}

type statsConn struct {
	net.Conn
	c *trafficCounter
}

func (c *statsConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.c.downloaded(n)
	return n, err
}
func (c *statsConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.c.uploaded(n)
	return n, err
}

func (c *statsConn) CloseRead() error {
	if v, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return v.CloseRead()
	}
	return nil
}
func (c *statsConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return nil
}

type statsPacketConn struct {
	net.PacketConn
	c *trafficCounter
}

func (c *statsPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	c.c.downloaded(n)
	return n, addr, err
}
func (c *statsPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, addr)
	c.c.uploaded(n)
	return n, err
}

func isPrivate(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.Is4() {
		v := ip.As4()
		return v[0] == 100 && v[1] >= 64 && v[1] <= 127
	}
	return false
}

type GeoIPMatcher struct{ countries map[string][]netip.Prefix }

func LoadGeoIP(path string) (*GeoIPMatcher, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routing: read GeoIP file %q: %w", path, err)
	}
	m := &GeoIPMatcher{countries: make(map[string][]netip.Prefix)}
	for off := 0; off < len(data); {
		field, wt, n := protowire.ConsumeTag(data[off:])
		if n <= 0 {
			break
		}
		off += n
		if wt != protowire.BytesType {
			n = protowire.ConsumeFieldValue(field, wt, data[off:])
			if n <= 0 {
				break
			}
			off += n
			continue
		}
		entry, n := protowire.ConsumeBytes(data[off:])
		if n <= 0 {
			break
		}
		off += n
		country, prefixes := parseGeoEntry(entry)
		if country != "" {
			m.countries[country] = append(m.countries[country], prefixes...)
		}
	}
	return m, nil
}

func parseGeoEntry(data []byte) (string, []netip.Prefix) {
	var country string
	var prefixes []netip.Prefix
	for off := 0; off < len(data); {
		field, wt, n := protowire.ConsumeTag(data[off:])
		if n <= 0 {
			break
		}
		off += n
		if wt != protowire.BytesType {
			n = protowire.ConsumeFieldValue(field, wt, data[off:])
			if n <= 0 {
				break
			}
			off += n
			continue
		}
		value, n := protowire.ConsumeBytes(data[off:])
		if n <= 0 {
			break
		}
		off += n
		switch field {
		case 1:
			country = strings.ToLower(string(value))
		case 2:
			if p, ok := parseGeoCIDR(value); ok {
				prefixes = append(prefixes, p)
			}
		}
	}
	return country, prefixes
}

func parseGeoCIDR(data []byte) (netip.Prefix, bool) {
	var ipBytes []byte
	prefixBits := -1
	for off := 0; off < len(data); {
		field, wt, n := protowire.ConsumeTag(data[off:])
		if n <= 0 {
			break
		}
		off += n
		switch wt {
		case protowire.BytesType:
			v, nn := protowire.ConsumeBytes(data[off:])
			if nn <= 0 {
				break
			}
			off += nn
			if field == 1 {
				ipBytes = append([]byte(nil), v...)
			}
		case protowire.VarintType:
			v, nn := protowire.ConsumeVarint(data[off:])
			if nn <= 0 {
				break
			}
			off += nn
			if field == 2 {
				prefixBits = int(v)
			}
		default:
			nn := protowire.ConsumeFieldValue(field, wt, data[off:])
			if nn <= 0 {
				break
			}
			off += nn
		}
	}
	if len(ipBytes) == 0 || prefixBits < 0 {
		return netip.Prefix{}, false
	}
	addr, ok := netip.AddrFromSlice(ipBytes)
	if !ok {
		return netip.Prefix{}, false
	}
	max := 128
	if addr.Is4() {
		max = 32
	}
	if prefixBits > max {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr.Unmap(), prefixBits), true
}

func (m *GeoIPMatcher) ContainsCountry(ip netip.Addr, country string) bool {
	if m == nil {
		return false
	}
	ps, ok := m.countries[strings.ToLower(strings.TrimSpace(country))]
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
