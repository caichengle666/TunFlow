package routing

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/proxy/direct"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
)

const (
	ModeGlobal = "global"
	ModeDirect = "direct"
	ModeBypass = "bypass"
)

// Rule selects the DIRECT path for destinations inside Prefix.
type Rule struct {
	Prefix netip.Prefix `json:"prefix" yaml:"prefix"`
}

// Proxy is a small policy layer around an upstream proxy. It deliberately
// implements the existing proxy.Proxy interface so the tun2socks transport
// pipeline remains unchanged.
type Proxy struct {
	upstream proxy.Proxy
	direct   proxy.Proxy
	mode     string
	rules    []Rule
}

func New(upstream proxy.Proxy, mode string, prefixes []string) (*Proxy, error) {
	if upstream == nil {
		return nil, fmt.Errorf("routing: upstream proxy is nil")
	}
	if mode == "" {
		mode = ModeGlobal
	}
	switch mode {
	case ModeGlobal, ModeDirect, ModeBypass:
	default:
		return nil, fmt.Errorf("routing: unsupported mode %q", mode)
	}

	d, err := direct.New()
	if err != nil {
		return nil, err
	}

	r := make([]Rule, 0, len(prefixes))
	for _, raw := range prefixes {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("routing: invalid CIDR %q: %w", raw, err)
		}
		r = append(r, Rule{Prefix: p})
	}

	return &Proxy{upstream: upstream, direct: d, mode: mode, rules: r}, nil
}

func (r *Proxy) useDirect(metadata *M.Metadata) bool {
	switch r.mode {
	case ModeDirect:
		return true
	case ModeGlobal:
		return false
	case ModeBypass:
		if metadata == nil || !metadata.DstIP.IsValid() {
			return false
		}
		for _, rule := range r.rules {
			if rule.Prefix.Contains(metadata.DstIP) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (r *Proxy) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	if r.useDirect(metadata) {
		return r.direct.DialContext(ctx, metadata)
	}
	return r.upstream.DialContext(ctx, metadata)
}

func (r *Proxy) DialUDP(metadata *M.Metadata) (net.PacketConn, error) {
	if r.useDirect(metadata) {
		return r.direct.DialUDP(metadata)
	}
	return r.upstream.DialUDP(metadata)
}
