package routing

import (
	"context"
	"net"
	"net/netip"
	"testing"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
)

type testProxy struct{}

func (testProxy) DialContext(context.Context, *M.Metadata) (net.Conn, error) { return nil, nil }
func (testProxy) DialUDP(*M.Metadata) (net.PacketConn, error)                { return nil, nil }

func TestBypassCIDR(t *testing.T) {
	r, err := New(testProxy{}, ModeBypass, []string{"192.168.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}

	inside := &M.Metadata{DstIP: netip.MustParseAddr("192.168.1.10")}
	outside := &M.Metadata{DstIP: netip.MustParseAddr("8.8.8.8")}
	if !r.useDirect(inside) {
		t.Fatal("expected LAN destination to use direct path")
	}
	if r.useDirect(outside) {
		t.Fatal("expected public destination to use proxy path")
	}
}

func TestGlobalAndDirectModes(t *testing.T) {
	metadata := &M.Metadata{DstIP: netip.MustParseAddr("192.168.1.10")}

	global, err := New(testProxy{}, ModeGlobal, []string{"192.168.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if global.useDirect(metadata) {
		t.Fatal("global mode must use proxy path")
	}

	direct, err := New(testProxy{}, ModeDirect, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !direct.useDirect(metadata) {
		t.Fatal("direct mode must use direct path")
	}
}

func TestInvalidCIDR(t *testing.T) {
	if _, err := New(testProxy{}, ModeBypass, []string{"not-a-cidr"}); err == nil {
		t.Fatal("expected invalid CIDR error")
	}
}

func TestDomainRulesAreRejected(t *testing.T) {
	for _, rule := range []string{"domain:example.com", "geosite:cn"} {
		if _, err := New(testProxy{}, ModeRules, []string{rule}); err == nil {
			t.Fatalf("expected unsupported rule error for %q", rule)
		}
	}
}

func TestGeoIPFileIsNotRequiredWithoutGeoIPRules(t *testing.T) {
	if _, err := NewWithConfig(testProxy{}, Config{
		Mode:      ModeGlobal,
		GeoIPFile: "missing-geoip.dat",
	}); err != nil {
		t.Fatalf("unexpected error without GeoIP rules: %v", err)
	}
}
