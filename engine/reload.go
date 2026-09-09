package engine

import (
	"errors"

	"github.com/xjasonlyu/tun2socks/v2/routing"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
)

// Reload applies proxy and routing changes to the running engine without
// tearing down the TUN device or gVisor stack. Existing connections keep
// their current outbound path; new connections use the new policy.
func Reload(k *Key) error {
	if k == nil {
		return errors.New("empty key")
	}
	_engineMu.Lock()
	defer _engineMu.Unlock()
	if _defaultDevice == nil || _defaultStack == nil {
		return errors.New("engine is not running")
	}
	if k.Proxy == "" {
		return errors.New("empty proxy")
	}

	proxyConn, err := parseProxy(k.Proxy)
	if err != nil {
		return err
	}
	mode := k.RoutingMode
	if mode == "" {
		mode = routing.ModeGlobal
	}
	directRules := append([]string{}, k.DirectRules...)
	if len(directRules) == 0 {
		directRules = append(directRules, k.DirectCIDRs...)
	}
	if mode == routing.ModeRules && len(directRules) == 0 && len(k.ProxyRules) == 0 {
		directRules = []string{"private", "geoip:cn"}
	}
	activeProxy, err := routing.NewWithConfig(proxyConn, routing.Config{
		Mode:        mode,
		DirectRules: directRules,
		ProxyRules:  k.ProxyRules,
		Default:     k.DefaultRoute,
		GeoIPFile:   k.GeoIPFile,
	})
	if err != nil {
		return err
	}

	_defaultProxy = proxyConn
	_defaultRoutingProxy = activeProxy
	_defaultKey = k
	tunnel.T().SetProxy(activeProxy)
	return nil
}
