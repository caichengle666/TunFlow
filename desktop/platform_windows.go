//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

const tunFlowGateway = "198.18.0.1"
const tunFlowMask = "255.254.0.0"
const hostRouteMask = "255.255.255.255"

type routeState struct {
	active                  bool
	ready                   bool
	tunName                 string
	tunIndex                string
	physicalInterface       string
	physicalIndex           string
	physicalIP              string
	gateway                 string
	proxyIPs                []string
	proxyRoutesOwned        map[string]bool
	engineInterface         string
	engineReloadPending     bool
	tunRouteOwned           bool
	tunAddressChanged       bool
	originalTUNAddressState windowsTUNAddressState
}

type windowsRoute struct {
	destination string
	mask        string
	gateway     string
	ifaceIP     string
	metric      int
}

type windowsTUNAddress struct {
	IPAddress    string `json:"IPAddress"`
	PrefixLength int    `json:"PrefixLength"`
}

type windowsTUNAddressState struct {
	DHCP      bool                `json:"DHCP"`
	Gateway   string              `json:"Gateway"`
	Addresses []windowsTUNAddress `json:"Addresses"`
}

func runWindows(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func readWindowsRoutes() ([]windowsRoute, error) {
	out, err := exec.Command("route", "print", "-4").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("读取 Windows IPv4 路由表失败: %w", err)
	}
	var routes []windowsRoute
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if net.ParseIP(fields[0]) == nil || net.ParseIP(fields[1]) == nil || net.ParseIP(fields[2]) == nil || net.ParseIP(fields[3]) == nil {
			continue
		}
		metric, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}
		routes = append(routes, windowsRoute{destination: fields[0], mask: fields[1], gateway: fields[2], ifaceIP: fields[3], metric: metric})
	}
	return routes, nil
}

func interfaceByIPv4(ip string) (*net.Interface, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil, fmt.Errorf("无效接口 IPv4 地址: %s", ip)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("读取网络接口失败: %w", err)
	}
	for i := range interfaces {
		addrs, err := interfaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var candidate net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				candidate = v.IP
			case *net.IPAddr:
				candidate = v.IP
			}
			if candidate != nil && candidate.To4() != nil && candidate.To4().Equal(parsed.To4()) {
				return &interfaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("找不到承载 IPv4 %s 的网络接口", ip)
}

func interfaceByName(name string) (*net.Interface, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("网络接口名称为空")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("找不到网络接口 %q: %w", name, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("网络接口 %q 当前未启用", name)
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return nil, fmt.Errorf("网络接口 %q 不能作为物理出口接口", name)
	}
	return iface, nil
}

func physicalDefaultRoute(preferredInterface string) (windowsRoute, *net.Interface, error) {
	routes, err := readWindowsRoutes()
	if err != nil {
		return windowsRoute{}, nil, err
	}
	var preferred *net.Interface
	if strings.TrimSpace(preferredInterface) != "" {
		preferred, err = interfaceByName(preferredInterface)
		if err != nil {
			return windowsRoute{}, nil, err
		}
	}
	candidates := make([]windowsRoute, 0)
	for _, route := range routes {
		if route.destination != "0.0.0.0" || route.mask != "0.0.0.0" {
			continue
		}
		if route.gateway == tunFlowGateway || route.ifaceIP == tunFlowGateway {
			continue
		}
		iface, err := interfaceByIPv4(route.ifaceIP)
		if err != nil || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if preferred != nil && iface.Index != preferred.Index {
			continue
		}
		candidates = append(candidates, route)
	}
	if len(candidates) == 0 && preferred != nil {
		return windowsRoute{}, nil, fmt.Errorf("网络接口 %q 没有可用的 IPv4 默认路由", preferred.Name)
	}
	if len(candidates) == 0 {
		return windowsRoute{}, nil, errors.New("没有找到可用的物理 IPv4 默认路由")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].metric < candidates[j].metric })
	selected := candidates[0]
	iface, err := interfaceByIPv4(selected.ifaceIP)
	if err != nil {
		return windowsRoute{}, nil, err
	}
	return selected, iface, nil
}

func defaultGateway() (string, error) {
	route, _, err := physicalDefaultRoute("")
	if err != nil {
		return "", err
	}
	return route.gateway, nil
}

func interfaceIndex(name string) (string, error) {
	iface, err := interfaceByName(name)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(iface.Index), nil
}

func proxyHost(proxyURL string) (string, error) {
	if !strings.Contains(proxyURL, "://") {
		proxyURL = "socks5://" + proxyURL
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("SOCKS5 地址没有主机名")
	}
	return host, nil
}

func resolveProxyIPs(proxyURL string) ([]string, error) {
	host, err := proxyHost(proxyURL)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return []string{v4.String()}, nil
		}
		return nil, errors.New("当前 Windows 自动路由暂只支持 IPv4 SOCKS5 地址")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("解析 SOCKS5 地址失败: %w", err)
	}
	set := make(map[string]struct{})
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			set[v4.String()] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for ip := range set {
		result = append(result, ip)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil, errors.New("SOCKS5 地址没有 IPv4 地址")
	}
	return result, nil
}

func resolveProxyIP(proxyURL string) (string, error) {
	ips, err := resolveProxyIPs(proxyURL)
	if err != nil {
		return "", err
	}
	return ips[0], nil
}

func captureTUNAddressState(name string) (windowsTUNAddressState, error) {
	script := fmt.Sprintf(`$i=Get-NetIPInterface -InterfaceAlias %s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1 Dhcp; $g=(Get-NetRoute -InterfaceAlias %s -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Select-Object -First 1 NextHop).NextHop; $a=@(Get-NetIPAddress -InterfaceAlias %s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object IPAddress,PrefixLength); [pscustomobject]@{DHCP=[bool]($i.Dhcp -eq 'Enabled');Gateway=$g;Addresses=$a} | ConvertTo-Json -Compress`, strconv.Quote(name), strconv.Quote(name), strconv.Quote(name))
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return windowsTUNAddressState{}, fmt.Errorf("读取 TUN 原始地址配置失败: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var state windowsTUNAddressState
	if strings.TrimSpace(string(out)) == "" {
		return state, nil
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return windowsTUNAddressState{}, fmt.Errorf("解析 TUN 原始地址配置失败: %w", err)
	}
	return state, nil
}

func restoreTUNAddressState(name string, state windowsTUNAddressState) error {
	if state.DHCP {
		return runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=dhcp")
	}
	if len(state.Addresses) == 0 {
		return runWindows("netsh", "interface", "ipv4", "delete", "address", "name="+name, "addr="+tunFlowGateway)
	}
	for _, addr := range state.Addresses {
		if prefixToMask(addr.PrefixLength) == "" {
			return fmt.Errorf("无法恢复 TUN 前置地址 %s/%d", addr.IPAddress, addr.PrefixLength)
		}
	}
	gateway := "none"
	if net.ParseIP(state.Gateway) != nil && state.Gateway != "0.0.0.0" {
		gateway = state.Gateway
	}
	if err := runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr="+state.Addresses[0].IPAddress, "mask="+prefixToMask(state.Addresses[0].PrefixLength), "gateway="+gateway); err != nil {
		return err
	}
	for _, addr := range state.Addresses[1:] {
		if err := runWindows("netsh", "interface", "ipv4", "add", "address", "name="+name, "addr="+addr.IPAddress, "mask="+prefixToMask(addr.PrefixLength)); err != nil {
			return err
		}
	}
	return nil
}

func prefixToMask(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return ""
	}
	return net.IP(net.CIDRMask(prefix, 32)).String()
}

func exactDefaultRoute(routes []windowsRoute, tunIndex string) (windowsRoute, bool) {
	for _, route := range routes {
		if route.destination == "0.0.0.0" && route.mask == "0.0.0.0" && route.gateway == tunFlowGateway {
			iface, err := interfaceByIPv4(route.ifaceIP)
			if err == nil && strconv.Itoa(iface.Index) == tunIndex {
				return route, true
			}
		}
	}
	return windowsRoute{}, false
}

func exactHostRoute(routes []windowsRoute, ip, gateway, physicalIP string) (windowsRoute, bool) {
	for _, route := range routes {
		if route.destination == ip && route.mask == hostRouteMask && route.gateway == gateway && route.ifaceIP == physicalIP {
			return route, true
		}
	}
	return windowsRoute{}, false
}

func conflictingHostRoute(routes []windowsRoute, ip string, gateway, physicalIP string) bool {
	for _, route := range routes {
		if route.destination == ip && route.mask == hostRouteMask && (route.gateway != gateway || route.ifaceIP != physicalIP) {
			return true
		}
	}
	return false
}

func windowsTunDefaultRouteReady(tunIndex string) (bool, error) {
	routes, err := readWindowsRoutes()
	if err != nil {
		return false, err
	}
	_, ok := exactDefaultRoute(routes, tunIndex)
	return ok, nil
}

func (a *App) effectiveEngineInterfaceLocked(cfg Config) string {
	if strings.TrimSpace(cfg.Interface) != "" || !cfg.AutoRoute {
		return strings.TrimSpace(cfg.Interface)
	}
	route, iface, err := physicalDefaultRoute("")
	if err != nil || iface == nil || route.gateway == "" {
		return ""
	}
	return iface.Name
}

func (a *App) setupRoutesLocked() (err error) {
	name := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(a.cfg.Device), "tun://"), "tun:")
	if name == "" {
		name = "TunFlow"
	}
	if a.up.active {
		if a.up.ready {
			return errors.New("Windows 自动路由已经处于活动状态")
		}
		if cleanupErr := a.teardownRoutesLocked(); cleanupErr != nil {
			return fmt.Errorf("清理上一次未完成的 Windows 路由事务失败: %w", cleanupErr)
		}
	}

	preferredInterface := strings.TrimSpace(a.cfg.Interface)
	physicalRoute, physicalIface, err := physicalDefaultRoute(preferredInterface)
	if err != nil {
		return err
	}
	proxyIPs, err := resolveProxyIPs(a.cfg.Proxy)
	if err != nil {
		return err
	}
	state := routeState{
		tunName:           name,
		physicalInterface: physicalIface.Name,
		physicalIndex:     strconv.Itoa(physicalIface.Index),
		physicalIP:        physicalRoute.ifaceIP,
		gateway:           physicalRoute.gateway,
		proxyIPs:          append([]string(nil), proxyIPs...),
		proxyRoutesOwned:  make(map[string]bool),
	}
	state.tunIndex, err = interfaceIndex(name)
	if err != nil {
		return err
	}
	state.originalTUNAddressState, err = captureTUNAddressState(name)
	if err != nil {
		return err
	}
	a.up = state
	defer func() {
		if err == nil {
			return
		}
		rollbackErr := a.rollbackRouteSetupLocked()
		if rollbackErr != nil {
			err = fmt.Errorf("%w；同时回滚 Windows 网络状态失败: %v", err, rollbackErr)
		}
	}()

	if err = runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr="+tunFlowGateway, "mask="+tunFlowMask, "gateway=none"); err != nil {
		return fmt.Errorf("配置 TUN 地址失败: %w", err)
	}
	a.up.tunAddressChanged = true
	a.up.active = true

	routes, err := readWindowsRoutes()
	if err != nil {
		return err
	}
	for _, proxyIP := range proxyIPs {
		if net.ParseIP(proxyIP).IsLoopback() {
			continue
		}
		if conflictingHostRoute(routes, proxyIP, physicalRoute.gateway, physicalRoute.ifaceIP) {
			return fmt.Errorf("SOCKS5 地址 %s 已存在冲突的主机路由，拒绝启动以避免环路", proxyIP)
		}
		if _, exists := exactHostRoute(routes, proxyIP, physicalRoute.gateway, physicalRoute.ifaceIP); exists {
			continue
		}
		if err = runWindows("route", "ADD", proxyIP, "MASK", hostRouteMask, physicalRoute.gateway, "METRIC", "1", "IF", state.physicalIndex); err != nil {
			return fmt.Errorf("添加 SOCKS5 防环路路由 %s 失败: %w", proxyIP, err)
		}
		a.up.proxyRoutesOwned[proxyIP] = true
	}

	routes, err = readWindowsRoutes()
	if err != nil {
		return err
	}
	if _, exists := exactDefaultRoute(routes, state.tunIndex); !exists {
		if err = runWindows("route", "ADD", "0.0.0.0", "MASK", "0.0.0.0", tunFlowGateway, "METRIC", "1", "IF", state.tunIndex); err != nil {
			return fmt.Errorf("添加 TUN 默认路由失败: %w", err)
		}
	}
	a.up.tunRouteOwned = true
	a.up.active = true

	ready, err := windowsTunDefaultRouteReady(state.tunIndex)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("TUN 默认路由未出现在 Windows 路由表中")
	}
	a.up.ready = true
	return nil
}

func (a *App) rollbackRouteSetupLocked() error {
	return a.teardownRoutesLockedWithRestore(true)
}

func (a *App) teardownRoutesLockedWithRestore(restoreAddress bool) (firstErr error) {
	if !a.up.active && !a.up.tunAddressChanged && !a.up.tunRouteOwned && len(a.up.proxyRoutesOwned) == 0 {
		return nil
	}
	a.up.ready = false

	if a.up.tunRouteOwned {
		if err := runWindows("route", "DELETE", "0.0.0.0", "MASK", "0.0.0.0", "IF", a.up.tunIndex); err != nil {
			firstErr = err
		} else {
			a.up.tunRouteOwned = false
		}
	}
	for ip, owned := range a.up.proxyRoutesOwned {
		if !owned {
			delete(a.up.proxyRoutesOwned, ip)
			continue
		}
		if err := runWindows("route", "DELETE", ip, "MASK", hostRouteMask, "IF", a.up.physicalIndex); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		delete(a.up.proxyRoutesOwned, ip)
	}
	if restoreAddress && a.up.tunAddressChanged {
		if err := restoreTUNAddressState(a.up.tunName, a.up.originalTUNAddressState); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			a.up.tunAddressChanged = false
		}
	}
	a.up.active = a.up.tunAddressChanged || a.up.tunRouteOwned || len(a.up.proxyRoutesOwned) > 0
	if !a.up.active {
		a.up = routeState{}
	}
	return firstErr
}

func (a *App) teardownRoutesLocked() error {
	return a.teardownRoutesLockedWithRestore(true)
}

func (a *App) reconcileRoutesLocked() error {
	if !a.cfg.AutoRoute || !a.up.active {
		return nil
	}
	wantEngineInterface := a.effectiveEngineInterfaceLocked(a.cfg)
	previousEngineInterface := a.up.engineInterface
	engineReloadPending := a.up.engineReloadPending

	currentRoute, currentIface, err := physicalDefaultRoute(strings.TrimSpace(a.cfg.Interface))
	if err != nil {
		return err
	}
	proxyIPs, err := resolveProxyIPs(a.cfg.Proxy)
	if err != nil {
		return err
	}
	routeChanged := currentRoute.gateway != a.up.gateway || currentRoute.ifaceIP != a.up.physicalIP || currentIface.Name != a.up.physicalInterface || !sameStringSet(proxyIPs, a.up.proxyIPs)
	if !routeChanged {
		ready, verifyErr := windowsTunDefaultRouteReady(a.up.tunIndex)
		if verifyErr != nil {
			return verifyErr
		}
		if !ready {
			routeChanged = true
		} else if !engineReloadPending && previousEngineInterface == wantEngineInterface {
			a.up.ready = true
			return nil
		}
	}
	if routeChanged {
		if err := a.teardownRoutesLocked(); err != nil {
			return fmt.Errorf("网络变化时清理旧路由失败: %w", err)
		}
		if err := a.setupRoutesLocked(); err != nil {
			return fmt.Errorf("网络变化时重建路由失败: %w", err)
		}
	}
	if engineReloadPending || previousEngineInterface != wantEngineInterface {
		if err := engine.Reload(a.buildEngineKeyLocked(a.cfg)); err != nil {
			a.up.engineReloadPending = true
			return fmt.Errorf("刷新核心网络接口失败: %w", err)
		}
		a.up.engineInterface = wantEngineInterface
		a.up.engineReloadPending = false
	} else {
		a.up.engineInterface = previousEngineInterface
	}
	a.up.ready = true
	return nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (a *App) startRouteMonitorLocked() {
	if a.routeMonitorStop != nil {
		return
	}
	stop := make(chan struct{})
	a.routeMonitorStop = stop
	a.routeMonitorWG.Add(1)
	go func() {
		defer a.routeMonitorWG.Done()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				a.mu.Lock()
				if engine.Running() && a.cfg.AutoRoute && a.up.active {
					if err := a.reconcileRoutesLocked(); err != nil {
						a.err = err
					}
				}
				a.mu.Unlock()
			}
		}
	}()
}
