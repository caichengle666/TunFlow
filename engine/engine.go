package engine

import (
	"errors"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/docker/go-units"
	"github.com/google/shlex"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/option"
	"github.com/xjasonlyu/tun2socks/v2/dialer"
	"github.com/xjasonlyu/tun2socks/v2/log"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/restapi"
	"github.com/xjasonlyu/tun2socks/v2/routing"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
	"github.com/xjasonlyu/tun2socks/v2/tunnel/statistic"
)

var (
	_engineMu            sync.Mutex
	_defaultKey          *Key
	_defaultProxy        proxy.Proxy
	_defaultRoutingProxy *routing.Proxy
	_defaultDevice       device.Device
	_defaultStack        *stack.Stack
	_icmpHandler         adapter.NetworkHandler
)

type TrafficStats struct {
	DownloadTotal          int64 `json:"downloadTotal"`
	UploadTotal            int64 `json:"uploadTotal"`
	DownloadPerSecond      int64 `json:"downloadPerSecond"`
	UploadPerSecond        int64 `json:"uploadPerSecond"`
	ProxyDownloadTotal     int64 `json:"proxyDownloadTotal"`
	ProxyUploadTotal       int64 `json:"proxyUploadTotal"`
	ProxyDownloadPerSecond int64 `json:"proxyDownloadPerSecond"`
	ProxyUploadPerSecond   int64 `json:"proxyUploadPerSecond"`
}

func Start() {
	if err := StartE(); err != nil {
		log.Fatalf("[ENGINE] failed to start: %v", err)
	}
}
func StartE() error { return start() }
func Stop() {
	if err := StopE(); err != nil {
		log.Fatalf("[ENGINE] failed to stop: %v", err)
	}
}
func StopE() error { return stop() }

func Running() bool {
	_engineMu.Lock()
	defer _engineMu.Unlock()
	return _defaultDevice != nil && _defaultStack != nil
}

func GetTrafficStats() TrafficStats {
	up, down := statistic.DefaultManager.Now()
	snap := statistic.DefaultManager.Snapshot()
	var proxyStats routing.TrafficStats
	_engineMu.Lock()
	if _defaultRoutingProxy != nil {
		proxyStats = _defaultRoutingProxy.Stats()
	}
	_engineMu.Unlock()
	return TrafficStats{
		DownloadTotal:          snap.DownloadTotal,
		UploadTotal:            snap.UploadTotal,
		DownloadPerSecond:      down,
		UploadPerSecond:        up,
		ProxyDownloadTotal:     proxyStats.DownloadTotal,
		ProxyUploadTotal:       proxyStats.UploadTotal,
		ProxyDownloadPerSecond: proxyStats.DownloadPerSecond,
		ProxyUploadPerSecond:   proxyStats.UploadPerSecond,
	}
}

func Insert(k *Key)                           { _engineMu.Lock(); _defaultKey = k; _engineMu.Unlock() }
func SetICMPHandler(h adapter.NetworkHandler) { _engineMu.Lock(); _icmpHandler = h; _engineMu.Unlock() }

func start() error {
	_engineMu.Lock()
	defer _engineMu.Unlock()
	if _defaultKey == nil {
		return errors.New("empty key")
	}
	for _, f := range []func(*Key) error{general, restAPI, netstack} {
		if err := f(_defaultKey); err != nil {
			_ = restapi.Stop()
			return err
		}
	}
	return nil
}

func stop() (err error) {
	if err = restapi.Stop(); err != nil {
		return err
	}
	_engineMu.Lock()
	if _defaultDevice != nil {
		_defaultDevice.Close()
		_defaultDevice = nil
	}
	if _defaultStack != nil {
		_defaultStack.Close()
		_defaultStack.Wait()
		_defaultStack = nil
	}
	_defaultRoutingProxy = nil
	_engineMu.Unlock()
	return nil
}

func execCommand(cmd string) error {
	parts, err := shlex.Split(cmd)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("empty command")
	}
	_, err = exec.Command(parts[0], parts[1:]...).Output()
	return err
}

func general(k *Key) error {
	level, err := log.ParseLevel(k.LogLevel)
	if err != nil {
		return err
	}
	log.SetLogger(log.Must(log.NewLeveled(level)))
	dialer.Reset()
	if k.Interface != "" {
		iface, err := net.InterfaceByName(k.Interface)
		if err != nil {
			return err
		}
		dialer.RegisterSockOpt(dialer.WithBindToInterface(iface))
		log.Infof("[DIALER] bind to interface: %s", k.Interface)
	}
	if k.Mark != 0 {
		dialer.RegisterSockOpt(dialer.WithRoutingMark(k.Mark))
		log.Infof("[DIALER] set fwmark: %#x", k.Mark)
	}
	if k.UDPTimeout > 0 {
		if k.UDPTimeout < time.Second {
			return errors.New("invalid udp timeout value")
		}
		tunnel.T().SetUDPTimeout(k.UDPTimeout)
	}
	return nil
}

func restAPI(k *Key) error {
	if k.RestAPI == "" {
		return nil
	}
	u, err := parseRestAPI(k.RestAPI)
	if err != nil {
		return err
	}
	host, token := u.Host, u.User.String()
	restapi.SetStatsFunc(func() tcpip.Stats {
		_engineMu.Lock()
		defer _engineMu.Unlock()
		if _defaultStack == nil {
			return tcpip.Stats{}
		}
		return _defaultStack.Stats()
	})
	go func() {
		if err := restapi.Start(host, token); err != nil {
			log.Errorf("[RESTAPI] failed to start: %v", err)
		}
	}()
	log.Infof("[RESTAPI] serve at: %s", u.Host)
	return nil
}

func netstack(k *Key) (err error) {
	if k.Proxy == "" {
		return errors.New("empty proxy")
	}
	if k.Device == "" {
		return errors.New("empty device")
	}
	if k.TUNPreUp != "" {
		log.Infof("[TUN] pre-execute command: `%s`", k.TUNPreUp)
		if preUpErr := execCommand(k.TUNPreUp); preUpErr != nil {
			log.Errorf("[TUN] failed to pre-execute: %s: %v", k.TUNPreUp, preUpErr)
		}
	}
	defer func() {
		if err != nil && _defaultDevice != nil {
			_defaultDevice.Close()
			_defaultDevice = nil
		}
	}()
	defer func() {
		if k.TUNPostUp == "" || err != nil {
			return
		}
		log.Infof("[TUN] post-execute command: `%s`", k.TUNPostUp)
		if postUpErr := execCommand(k.TUNPostUp); postUpErr != nil {
			log.Errorf("[TUN] failed to post-execute: %s: %v", k.TUNPostUp, postUpErr)
		}
	}()
	multicastGroups, err := parseMulticastGroups(k.MulticastGroups)
	if err != nil {
		return err
	}
	if _defaultProxy, err = parseProxy(k.Proxy); err != nil {
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
	activeProxy, err := routing.NewWithConfig(_defaultProxy, routing.Config{Mode: mode, DirectRules: directRules, ProxyRules: k.ProxyRules, Default: k.DefaultRoute, GeoIPFile: k.GeoIPFile})
	if err != nil {
		return err
	}
	_defaultRoutingProxy = activeProxy
	tunnel.T().SetProxy(activeProxy)
	if _defaultDevice, err = parseDevice(k.Device, uint32(k.MTU)); err != nil {
		return err
	}
	var opts []option.Option
	if k.TCPModerateReceiveBuffer {
		opts = append(opts, option.WithTCPModerateReceiveBuffer(true))
	}
	if k.TCPSendBufferSize != "" {
		size, e := units.RAMInBytes(k.TCPSendBufferSize)
		if e != nil {
			return e
		}
		opts = append(opts, option.WithTCPSendBufferSize(int(size)))
	}
	if k.TCPReceiveBufferSize != "" {
		size, e := units.RAMInBytes(k.TCPReceiveBufferSize)
		if e != nil {
			return e
		}
		opts = append(opts, option.WithTCPReceiveBufferSize(int(size)))
	}
	if _defaultStack, err = core.CreateStack(&core.Config{LinkEndpoint: _defaultDevice, TransportHandler: tunnel.T(), ICMPHandler: _icmpHandler, MulticastGroups: multicastGroups, Options: opts}); err != nil {
		return err
	}
	log.Infof("[STACK] %s <-> %s", k.Device, k.Proxy)
	return nil
}
