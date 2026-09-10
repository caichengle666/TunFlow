//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
)

type routeState struct {
	active        bool
	tunName       string
	tunIndex      string
	physicalIndex string
	gateway       string
	proxyIP       string
	proxyIPs      []string
	proxyBypass   bool
	tunRoute      bool
	original      tunConfig
}

type tunConfig struct {
	DHCP         bool   `json:"dhcp"`
	Address      string `json:"address"`
	PrefixLength int    `json:"prefixLength"`
	Gateway      string `json:"gateway"`
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

func windowsTunDefaultRouteReady() (bool, error) {
	out, err := exec.Command("route", "print", "0.0.0.0").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("读取 TUN 默认路由失败: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "0.0.0.0" || fields[1] != "0.0.0.0" {
			continue
		}
		if fields[2] == "198.18.0.1" || fields[3] == "198.18.0.1" {
			return true, nil
		}
	}
	return false, nil
}

func defaultGateway(interfaceName string) (string, error) {
	var result []struct {
		Gateway string `json:"gateway"`
	}
	filter := ""
	if strings.TrimSpace(interfaceName) != "" {
		filter = fmt.Sprintf(" | Where-Object {$_.InterfaceAlias -eq '%s'}", escapePowerShell(interfaceName))
	}
	script := fmt.Sprintf(`@(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4%s | Where-Object {$_.NextHop -ne '0.0.0.0'} | Sort-Object RouteMetric,@{Expression={(Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4).InterfaceMetric}} | Select-Object @{N="gateway";E={$_.NextHop}}) | ConvertTo-Json -Compress`, filter)
	if err := runPowerShellJSON(script, &result); err != nil {
		return "", err
	}
	if len(result) == 0 || net.ParseIP(result[0].Gateway) == nil {
		return "", errors.New("没有找到可用的默认网关")
	}
	return result[0].Gateway, nil
}

func physicalInterfaceName(gateway string) (string, error) {
	var result []struct {
		Name string `json:"name"`
	}
	script := fmt.Sprintf(`@(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4 | Where-Object {$_.NextHop -eq '%s'} | Select-Object -First 1 @{N="name";E={$_.InterfaceAlias}}) | ConvertTo-Json -Compress`, escapePowerShell(gateway))
	if err := runPowerShellJSON(script, &result); err != nil {
		return "", err
	}
	if len(result) == 0 || result[0].Name == "" {
		return "", errors.New("找不到默认网关对应的物理网卡")
	}
	return result[0].Name, nil
}

func physicalInterfaceIndex(gateway string) (string, error) {
	var result []struct {
		Index int `json:"index"`
	}
	script := fmt.Sprintf(`@(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4 | Where-Object {$_.NextHop -eq '%s'} | Select-Object -First 1 @{N="index";E={$_.InterfaceIndex}}) | ConvertTo-Json -Compress`, escapePowerShell(gateway))
	if err := runPowerShellJSON(script, &result); err != nil {
		return "", err
	}
	if len(result) == 0 || result[0].Index <= 0 {
		return "", errors.New("找不到默认网关对应的物理网卡")
	}
	return strconv.Itoa(result[0].Index), nil
}

func currentNetworkSignature(interfaceName string) (string, error) {
	var result []struct {
		Gateway string `json:"gateway"`
		Index   int    `json:"index"`
	}
	filter := ""
	if strings.TrimSpace(interfaceName) != "" {
		filter = fmt.Sprintf(" | Where-Object {$_.InterfaceAlias -eq '%s'}", strings.ReplaceAll(interfaceName, "'", "''"))
	}
	script := fmt.Sprintf(`@(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4%s | Where-Object {$_.NextHop -ne '0.0.0.0'} | Sort-Object RouteMetric | Select-Object -First 1 @{N="gateway";E={$_.NextHop}},@{N="index";E={$_.InterfaceIndex}}) | ConvertTo-Json -Compress`, filter)
	if err := runPowerShellJSON(script, &result); err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", errors.New("没有找到默认网络出口")
	}
	return fmt.Sprintf("%s/%d", result[0].Gateway, result[0].Index), nil
}

func (a *App) selectRuntimeInterfaceLocked() error {
	if strings.TrimSpace(a.cfg.Interface) != "" {
		a.runtimeInterface = strings.TrimSpace(a.cfg.Interface)
		return nil
	}
	gateway, err := defaultGateway("")
	if err != nil {
		return err
	}
	name, err := physicalInterfaceName(gateway)
	if err != nil {
		return err
	}
	a.runtimeInterface = name
	return nil
}

func interfaceIndex(name string) (string, error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("读取网络接口失败: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		idx := fields[0]
		if _, err := strconv.Atoi(idx); err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(strings.Join(fields[4:], " ")), name) {
			return idx, nil
		}
	}
	return "", fmt.Errorf("找不到 TUN 接口: %s", name)
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

func resolveProxyIP(proxyURL string) (string, error) {
	ips, err := resolveProxyIPs(proxyURL)
	if err != nil {
		return "", err
	}
	return ips[0], nil
}

func escapePowerShell(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func runPowerShellJSON(script string, target any) error {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("PowerShell 查询失败: %w: %s", err, strings.TrimSpace(string(out)))
	}

	payload := bytes.TrimSpace(out)
	if len(payload) == 0 {
		return errors.New("解析 PowerShell 查询结果失败: 输出为空")
	}

	// Windows PowerShell 5.1 emits a bare JSON object when the pipeline
	// produces exactly one item, even when the producer is wrapped in @(...).
	// Most callers intentionally decode into []struct{...}. Normalize that
	// single-object shape to a one-element slice before falling back to the
	// regular json.Unmarshal path.
	if payload[0] == '{' {
		rv := reflect.ValueOf(target)
		if rv.IsValid() && rv.Kind() == reflect.Ptr && !rv.IsNil() && rv.Elem().Kind() == reflect.Slice {
			elem := reflect.New(rv.Elem().Type().Elem())
			if err := json.Unmarshal(payload, elem.Interface()); err != nil {
				return fmt.Errorf("解析 PowerShell 查询结果失败: %w", err)
			}
			slice := reflect.MakeSlice(rv.Elem().Type(), 1, 1)
			slice.Index(0).Set(elem.Elem())
			rv.Elem().Set(slice)
			return nil
		}
	}

	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("解析 PowerShell 查询结果失败: %w", err)
	}
	return nil
}

func readTunConfig(name string) (tunConfig, error) {
	var cfg tunConfig
	quoted := strings.ReplaceAll(name, "'", "''")
	script := fmt.Sprintf(`$i=Get-NetIPInterface -InterfaceAlias '%s' -AddressFamily IPv4; $a=Get-NetIPAddress -InterfaceAlias '%s' -AddressFamily IPv4 | Where-Object {$_.AddressState -eq 'Preferred'} | Select-Object -First 1; $g=Get-NetIPConfiguration -InterfaceAlias '%s' | Select-Object -ExpandProperty IPv4DefaultGateway | Select-Object -First 1; [pscustomobject]@{dhcp=([string]$i.Dhcp -eq 'Enabled');address=$a.IPAddress;prefixLength=$a.PrefixLength;gateway=$g.NextHop} | ConvertTo-Json -Compress`, quoted, quoted, quoted)
	return cfg, runPowerShellJSON(script, &cfg)
}

func restoreTunConfig(name string, cfg tunConfig) error {
	if cfg.DHCP || cfg.Address == "" {
		return runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=dhcp")
	}
	mask := net.CIDRMask(cfg.PrefixLength, 32)
	maskText := net.IPv4(mask[0], mask[1], mask[2], mask[3]).String()
	gateway := "gateway=none"
	if net.ParseIP(cfg.Gateway) != nil {
		gateway = "gateway=" + cfg.Gateway
	}
	return runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr="+cfg.Address, "mask="+maskText, gateway)
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
		return nil, errors.New("当前 Windows 路由管理暂只支持 IPv4 SOCKS5 地址")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("解析 SOCKS5 地址失败: %w", err)
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(ips))
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && !seen[v4.String()] {
			seen[v4.String()] = true
			result = append(result, v4.String())
		}
	}
	if len(result) == 0 {
		return nil, errors.New("SOCKS5 地址没有 IPv4 地址")
	}
	return result, nil
}

func (a *App) setupRoutesLocked() error {
	if err := a.selectRuntimeInterfaceLocked(); err != nil {
		return err
	}
	name := strings.TrimPrefix(strings.TrimPrefix(a.cfg.Device, "tun://"), "tun:")
	if name == "" {
		name = "TunFlow"
	}
	original, err := readTunConfig(name)
	if err != nil {
		return err
	}
	gateway, err := defaultGateway(a.runtimeInterface)
	if err != nil {
		return err
	}

	if err := runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr=198.18.0.1", "mask=255.254.0.0", "gateway=none"); err != nil {
		return fmt.Errorf("配置 TUN 地址失败: %w", err)
	}
	idx, err := interfaceIndex(name)
	if err != nil {
		return err
	}

	proxyIPs, err := resolveProxyIPs(a.cfg.Proxy)
	if err != nil {
		return err
	}
	physicalIndex, err := physicalInterfaceIndex(gateway)
	if err != nil {
		_ = restoreTunConfig(name, original)
		return err
	}
	proxyIP := proxyIPs[0]

	// Keep the remote SOCKS5 endpoint on the physical interface. Without this
	// host route, the default route below would send the proxy connection back
	// into TUN and create a routing loop.
	proxyBypass := proxyIP != "" && !net.ParseIP(proxyIP).IsLoopback()
	if proxyBypass {
		for _, ip := range proxyIPs {
			if net.ParseIP(ip).IsLoopback() {
				continue
			}
			if err := runWindows("route", "ADD", ip, "MASK", "255.255.255.255", gateway, "METRIC", "1", "IF", physicalIndex); err != nil {
				_ = restoreTunConfig(name, original)
				return fmt.Errorf("添加 SOCKS5 防环路路由失败: %w", err)
			}
		}
	}

	if err := runWindows("route", "ADD", "0.0.0.0", "MASK", "0.0.0.0", "198.18.0.1", "METRIC", "1", "IF", idx); err != nil {
		if proxyBypass {
			for _, ip := range proxyIPs {
				_ = runWindows("route", "DELETE", ip, "MASK", "255.255.255.255", gateway, "IF", physicalIndex)
			}
		}
		_ = restoreTunConfig(name, original)
		return fmt.Errorf("添加 TUN 默认路由失败: %w", err)
	}
	// Mark the route before verification so a failed verification can still
	// remove it during rollback.
	a.up = routeState{active: true, tunName: name, tunIndex: idx, physicalIndex: physicalIndex, gateway: gateway, proxyIP: proxyIP, proxyIPs: proxyIPs, proxyBypass: proxyBypass, tunRoute: true, original: original}
	ready, err := windowsTunDefaultRouteReady()
	if err != nil {
		return a.teardownRoutesLocked()
	}
	if !ready {
		_ = a.teardownRoutesLocked()
		return errors.New("TUN 默认路由未出现在 Windows 路由表中")
	}
	if signature, err := currentNetworkSignature(a.cfg.Interface); err == nil {
		a.networkSignature = signature
	}

	return nil
}

func (a *App) teardownRoutesLocked() error {
	if !a.up.active {
		return nil
	}
	var firstErr error
	if err := runWindows("route", "DELETE", "0.0.0.0", "MASK", "0.0.0.0", "198.18.0.1", "IF", a.up.tunIndex); err != nil {
		firstErr = err
	} else {
		a.up.tunRoute = false
	}
	if a.up.proxyBypass && firstErr == nil {
		ips := a.up.proxyIPs
		if len(ips) == 0 {
			ips = []string{a.up.proxyIP}
		}
		for _, ip := range ips {
			if net.ParseIP(ip).IsLoopback() {
				continue
			}
			if err := runWindows("route", "DELETE", ip, "MASK", "255.255.255.255", "IF", a.up.physicalIndex); err != nil {
				firstErr = err
				break
			}
		}
		if firstErr == nil {
			a.up.proxyBypass = false
		}
	}
	if firstErr == nil {
		if err := restoreTunConfig(a.up.tunName, a.up.original); err != nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		a.up = routeState{}
	}
	return firstErr
}
