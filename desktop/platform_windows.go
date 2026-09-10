//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

type routeState struct {
	active bool
	tunName string
	tunIndex string
	physicalIndex string
	gateway string
	proxyIPs []string
	original tunConfig
}

type tunConfig struct {
	DHCP bool
	Address string
	PrefixLength int
	Gateway string
}

func runWindows(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" { return fmt.Errorf("%w: %s", err, msg) }
		return err
	}
	return nil
}

func runPowerShellText(script string) (string, error) {
	// Do not parse PowerShell through JSON. Windows PowerShell 5.1 emits a
	// single object as a JSON object instead of an array, and errors/noise can
	// otherwise corrupt stdout. This runner makes stdout a plain UTF-8 value
	// and treats any non-zero PowerShell exit as a hard failure.
	wrapper := `$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); $OutputEncoding = New-Object System.Text.UTF8Encoding($false); ` + script
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", wrapper)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if msg == "" { msg = strings.TrimSpace(string(out)) }
			return "", fmt.Errorf("PowerShell 查询失败: %s", msg)
		}
		return "", fmt.Errorf("启动 PowerShell 失败: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func psQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func defaultGateway(interfaceName string) (string, error) {
	filter := ""
	if strings.TrimSpace(interfaceName) != "" { filter = fmt.Sprintf(" | Where-Object { $_.InterfaceAlias -eq %s }", psQuote(interfaceName)) }
	script := fmt.Sprintf(`$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4%s | Where-Object {$_.NextHop -ne '0.0.0.0'} | Sort-Object RouteMetric,@{Expression={(Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4).InterfaceMetric}} | Select-Object -First 1; if ($null -eq $r) { exit 20 }; Write-Output ([string]$r.NextHop)`, filter)
	value, err := runPowerShellText(script); if err != nil { return "", err }
	if net.ParseIP(value) == nil { return "", fmt.Errorf("PowerShell 返回的默认网关无效: %q", value) }
	return value, nil
}

func physicalInterfaceName(gateway string) (string, error) {
	script := fmt.Sprintf(`$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4 | Where-Object {$_.NextHop -eq %s} | Select-Object -First 1; if ($null -eq $r) { exit 21 }; Write-Output ([string]$r.InterfaceAlias)`, psQuote(gateway))
	value, err := runPowerShellText(script); if err != nil { return "", err }
	if value == "" { return "", errors.New("找不到默认网关对应的物理网卡") }
	return value, nil
}

func physicalInterfaceIndex(gateway string) (string, error) {
	script := fmt.Sprintf(`$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4 | Where-Object {$_.NextHop -eq %s} | Select-Object -First 1; if ($null -eq $r) { exit 22 }; Write-Output ([string]$r.InterfaceIndex)`, psQuote(gateway))
	value, err := runPowerShellText(script); if err != nil { return "", err }
	idx, err := strconv.Atoi(value); if err != nil || idx <= 0 { return "", fmt.Errorf("物理网卡索引无效: %q", value) }
	return strconv.Itoa(idx), nil
}

func currentNetworkSignature(interfaceName string) (string, error) {
	filter := ""
	if strings.TrimSpace(interfaceName) != "" { filter = fmt.Sprintf(" | Where-Object { $_.InterfaceAlias -eq %s }", psQuote(interfaceName)) }
	script := fmt.Sprintf(`$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4%s | Where-Object {$_.NextHop -ne '0.0.0.0'} | Sort-Object RouteMetric | Select-Object -First 1; if ($null -eq $r) { exit 23 }; Write-Output (([string]$r.NextHop)+'/'+([string]$r.InterfaceIndex))`, filter)
	value, err := runPowerShellText(script); if err != nil { return "", err }
	return value, nil
}

func (a *App) selectRuntimeInterfaceLocked() error {
	if name := strings.TrimSpace(a.cfg.Interface); name != "" { a.runtimeInterface = name; return nil }
	gateway, err := defaultGateway(""); if err != nil { return err }
	name, err := physicalInterfaceName(gateway); if err != nil { return err }
	a.runtimeInterface = name
	return nil
}

func interfaceIndex(name string) (string, error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces").CombinedOutput()
	if err != nil { return "", fmt.Errorf("读取网络接口失败: %w", err) }
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line); if len(fields) < 5 { continue }
		idx := fields[0]; if _, err := strconv.Atoi(idx); err != nil { continue }
		candidate := strings.TrimSpace(strings.Join(fields[4:], " "))
		if strings.EqualFold(candidate, name) { return idx, nil }
	}
	return "", fmt.Errorf("找不到 TUN 接口: %s", name)
}

func proxyHost(proxyURL string) (string, error) {
	proxyURL = normalizeProxy(proxyURL)
	u, err := url.Parse(proxyURL); if err != nil { return "", err }
	if u.Hostname() == "" { return "", errors.New("SOCKS5 地址没有主机名") }
	return u.Hostname(), nil
}

func resolveProxyIPs(proxyURL string) ([]string, error) {
	host, err := proxyHost(proxyURL); if err != nil { return nil, err }
	if ip := net.ParseIP(host); ip != nil { if v4 := ip.To4(); v4 != nil { return []string{v4.String()}, nil }; return nil, errors.New("当前 Windows 路由管理暂只支持 IPv4 SOCKS5 地址") }
	ips, err := net.LookupIP(host); if err != nil { return nil, fmt.Errorf("解析 SOCKS5 地址失败: %w", err) }
	seen := map[string]bool{}; result := make([]string, 0, len(ips))
	for _, ip := range ips { if v4 := ip.To4(); v4 != nil && !seen[v4.String()] { seen[v4.String()] = true; result = append(result, v4.String()) } }
	if len(result) == 0 { return nil, errors.New("SOCKS5 地址没有 IPv4 地址") }
	return result, nil
}

func readTunConfig(name string) (tunConfig, error) {
	quoted := psQuote(name)
	script := fmt.Sprintf(`$i=Get-NetIPInterface -InterfaceAlias %s -AddressFamily IPv4; $a=Get-NetIPAddress -InterfaceAlias %s -AddressFamily IPv4 | Where-Object {$_.AddressState -eq 'Preferred'} | Select-Object -First 1; $g=Get-NetIPConfiguration -InterfaceAlias %s | Select-Object -ExpandProperty IPv4DefaultGateway | Select-Object -First 1; if ($null -eq $i -or $null -eq $a) { exit 24 }; $dhcp=([string]$i.Dhcp -eq 'Enabled'); $addr=[string]$a.IPAddress; $prefix=[string]$a.PrefixLength; $gateway=''; if ($null -ne $g) { $gateway=[string]$g.NextHop }; Write-Output ($dhcp.ToString().ToLower()+"`t"+$addr+"`t"+$prefix+"`t"+$gateway)`, quoted, quoted, quoted)
	value, err := runPowerShellText(script); if err != nil { return tunConfig{}, err }
	parts := strings.Split(value, "\t"); if len(parts) < 4 { return tunConfig{}, fmt.Errorf("读取 TUN 配置返回值无效: %q", value) }
	prefix, err := strconv.Atoi(parts[2]); if err != nil { return tunConfig{}, fmt.Errorf("TUN 前缀长度无效: %q", parts[2]) }
	return tunConfig{DHCP: parts[0] == "true", Address: parts[1], PrefixLength: prefix, Gateway: parts[3]}, nil
}

func restoreTunConfig(name string, cfg tunConfig) error {
	if cfg.DHCP || cfg.Address == "" { return runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=dhcp") }
	mask := net.CIDRMask(cfg.PrefixLength, 32); maskText := net.IPv4(mask[0], mask[1], mask[2], mask[3]).String(); gateway := "gateway=none"
	if net.ParseIP(cfg.Gateway) != nil { gateway = "gateway="+cfg.Gateway }
	return runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr="+cfg.Address, "mask="+maskText, gateway)
}

func windowsTunDefaultRouteReady() (bool, error) {
	out, err := exec.Command("route", "print", "0.0.0.0").CombinedOutput(); if err != nil { return false, fmt.Errorf("读取 TUN 默认路由失败: %w", err) }
	for _, line := range strings.Split(string(out), "\n") { fields := strings.Fields(line); if len(fields) >= 4 && fields[0] == "0.0.0.0" && fields[1] == "0.0.0.0" && (fields[2] == "198.18.0.1" || fields[3] == "198.18.0.1") { return true, nil } }
	return false, nil
}

func (a *App) setupRoutesLocked() error {
	if err := a.selectRuntimeInterfaceLocked(); err != nil { return err }
	name := strings.TrimPrefix(strings.TrimPrefix(a.cfg.Device, "tun://"), "tun:"); if name == "" { name = "TunFlow" }
	original, err := readTunConfig(name); if err != nil { return err }
	gateway, err := defaultGateway(a.runtimeInterface); if err != nil { return err }
	idx, err := interfaceIndex(name); if err != nil { return err }
	proxyIPs, err := resolveProxyIPs(a.cfg.Proxy); if err != nil { return err }
	physicalIndex, err := physicalInterfaceIndex(gateway); if err != nil { return err }
	if err := runWindows("netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "addr=198.18.0.1", "mask=255.254.0.0", "gateway=none"); err != nil { return fmt.Errorf("配置 TUN 地址失败: %w", err) }

	added := make([]string, 0, len(proxyIPs))
	cleanup := func() { for _, ip := range added { _ = runWindows("route", "DELETE", ip, "MASK", "255.255.255.255", gateway, "IF", physicalIndex) }; _ = restoreTunConfig(name, original) }
	for _, ip := range proxyIPs {
		if net.ParseIP(ip).IsLoopback() { continue }
		if err := runWindows("route", "ADD", ip, "MASK", "255.255.255.255", gateway, "METRIC", "1", "IF", physicalIndex); err != nil { cleanup(); return fmt.Errorf("添加 SOCKS5 防环路路由失败: %w", err) }
		added = append(added, ip)
	}
	if err := runWindows("route", "ADD", "0.0.0.0", "MASK", "0.0.0.0", "198.18.0.1", "METRIC", "1", "IF", idx); err != nil { cleanup(); return fmt.Errorf("添加 TUN 默认路由失败: %w", err) }
	a.up = routeState{active:true, tunName:name, tunIndex:idx, physicalIndex:physicalIndex, gateway:gateway, proxyIPs:added, original:original}
	ready, err := windowsTunDefaultRouteReady(); if err != nil || !ready { _ = a.teardownRoutesLocked(); if err != nil { return err }; return errors.New("TUN 默认路由未出现在 Windows 路由表中") }
	if signature, err := currentNetworkSignature(a.cfg.Interface); err == nil { a.networkSignature = signature }
	return nil
}

func (a *App) teardownRoutesLocked() error {
	if !a.up.active { return nil }
	var firstErr error
	if err := runWindows("route", "DELETE", "0.0.0.0", "MASK", "0.0.0.0", "198.18.0.1", "IF", a.up.tunIndex); err != nil { firstErr = err }
	if firstErr == nil { for _, ip := range a.up.proxyIPs { if err := runWindows("route", "DELETE", ip, "MASK", "255.255.255.255", "IF", a.up.physicalIndex); err != nil { firstErr = err; break } } }
	if firstErr == nil { if err := restoreTunConfig(a.up.tunName, a.up.original); err != nil { firstErr = err } }
	if firstErr == nil { a.up = routeState{} }
	return firstErr
}
