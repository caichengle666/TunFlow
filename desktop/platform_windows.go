//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

type routeState struct {
	active      bool
	tunName     string
	tunIndex    string
	gateway     string
	proxyIP     string
	proxyBypass bool
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

func defaultGateway() (string, error) {
	out, err := exec.Command("route", "print", "0.0.0.0").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("读取默认路由失败: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "0.0.0.0" || fields[1] != "0.0.0.0" {
			continue
		}
		if net.ParseIP(fields[2]) == nil {
			continue
		}
		return fields[2], nil
	}
	return "", errors.New("没有找到可用的默认网关")
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
		if strings.EqualFold(strings.Join(fields[4:], " "), name) || strings.EqualFold(fields[len(fields)-1], name) {
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
	host, err := proxyHost(proxyURL)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4().String(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", fmt.Errorf("解析 SOCKS5 地址失败: %w", err)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", errors.New("SOCKS5 地址没有 IPv4 地址")
}

func (a *App) setupRoutesLocked() error {
	name := strings.TrimPrefix(strings.TrimPrefix(a.cfg.Device, "tun://"), "tun:")
	if name == "" {
		name = "TunFlow"
	}
	gateway, err := defaultGateway()
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

	proxyIP, err := resolveProxyIP(a.cfg.Proxy)
	if err != nil {
		return err
	}

	// A remote SOCKS5 endpoint must stay on the physical interface, otherwise
	// the default route through TUN would feed the proxy connection back into TUN.
	proxyBypass := proxyIP != "" && !net.ParseIP(proxyIP).IsLoopback()
	if proxyBypass {
		if err := runWindows("route", "ADD", proxyIP, "MASK", "255.255.255.255", gateway, "METRIC", "1"); err != nil {
			return fmt.Errorf("添加 SOCKS5 防环路路由失败: %w", err)
		}
	}

	if err := runWindows("route", "ADD", "0.0.0.0", "MASK", "0.0.0.0", "198.18.0.1", "METRIC", "1", "IF", idx); err != nil {
		if proxyBypass {
			_ = runWindows("route", "DELETE", proxyIP, "MASK", "255.255.255.255", gateway)
		}
		return fmt.Errorf("添加 TUN 默认路由失败: %w", err)
	}

	a.up = routeState{
		active:      true,
		tunName:     name,
		tunIndex:    idx,
		gateway:     gateway,
		proxyIP:     proxyIP,
		proxyBypass: proxyBypass,
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
	}
	if a.up.proxyBypass {
		if err := runWindows("route", "DELETE", a.up.proxyIP, "MASK", "255.255.255.255", a.up.gateway); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	a.up = routeState{}
	return firstErr
}

var _ = bufio.NewScanner
