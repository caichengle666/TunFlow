package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

type App struct {
	ctx                context.Context
	mu                 sync.Mutex
	cfg                Config
	up                 routeState
	err                error
	trayEnd            func()
	configWatchStop    chan struct{}
	configWatchWG      sync.WaitGroup
	configDiskChecksum string
	configLoaded       bool
	networkSignature   string
	runtimeInterface   string
}

type Config struct {
	Proxy            string   `json:"proxy"`
	Device           string   `json:"device"`
	Interface        string   `json:"interface"`
	Mode             string   `json:"mode"`
	DirectCIDRs      []string `json:"directCIDRs,omitempty"`
	DirectRules      []string `json:"directRules,omitempty"`
	ProxyRules       []string `json:"proxyRules,omitempty"`
	DefaultRoute     string   `json:"defaultRoute"`
	GeoIPFile        string   `json:"geoIPFile,omitempty"`
	AutoRoute        bool     `json:"autoRoute"`
	StartWithWindows bool     `json:"startWithWindows"`
}

type Status struct {
	Running          bool   `json:"running"`
	Proxy            string `json:"proxy"`
	Device           string `json:"device"`
	Mode             string `json:"mode"`
	AutoRoute        bool   `json:"autoRoute"`
	RouteReady       bool   `json:"routeReady"`
	LastError        string `json:"lastError,omitempty"`
	StartWithWindows bool   `json:"startWithWindows"`
}

type NetworkInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
}

const (
	geoIPDownloadURL   = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
	geoSiteDownloadURL = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
)

func NewApp() *App {
	return &App{cfg: Config{
		Proxy:            "socks5://127.0.0.1:1080",
		Device:           "tun://TunFlow",
		Mode:             "global",
		DirectCIDRs:      []string{},
		DirectRules:      []string{},
		ProxyRules:       []string{},
		DefaultRoute:     "proxy",
		GeoIPFile:        "geoip.dat",
		AutoRoute:        true,
		StartWithWindows: false,
	}}
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx = ctx
	if err := a.load(); err != nil {
		if _, statErr := os.Stat(a.configPath()); os.IsNotExist(statErr) {
			a.normalizeConfigLocked()
			if a.saveLocked() == nil {
				a.configLoaded = true
			}
		}
	} else {
		a.configLoaded = true
	}
	a.normalizeConfigLocked()
	a.refreshConfigDiskChecksumLocked()
	a.startConfigWatcherLocked()
	a.startSystemTray()
}

func (a *App) shutdown(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	watchStop := a.configWatchStop
	a.configWatchStop = nil
	if watchStop != nil {
		close(watchStop)
	}
	trayEnd := a.trayEnd
	a.trayEnd = nil
	if engine.Running() || a.up.active {
		_ = a.stopLocked()
	}
	if a.configLoaded {
		_ = a.saveLocked()
	}
	a.mu.Unlock()
	if watchStop != nil {
		a.configWatchWG.Wait()
	}
	if trayEnd != nil {
		trayEnd()
	}
}

func (a *App) GetConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) GetConfigPath() string {
	return a.configPath()
}

func (a *App) GetNetworkInterfaces() ([]NetworkInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("读取网络接口失败: %w", err)
	}
	tunName := strings.TrimPrefix(strings.TrimPrefix(a.GetConfig().Device, "tun://"), "tun:")
	result := make([]NetworkInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || strings.EqualFold(iface.Name, tunName) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		addresses := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addresses = append(addresses, addr.String())
		}
		result = append(result, NetworkInterface{Name: iface.Name, Addresses: addresses})
	}
	return result, nil
}

func (a *App) GetStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Status{
		Running:          engine.Running(),
		Proxy:            a.cfg.Proxy,
		Device:           a.cfg.Device,
		Mode:             a.cfg.Mode,
		AutoRoute:        a.cfg.AutoRoute,
		RouteReady:       a.up.active,
		LastError:        errString(a.err),
		StartWithWindows: a.cfg.StartWithWindows,
	}
}

func (a *App) GetTrafficStats() engine.TrafficStats {
	return engine.GetTrafficStats()
}

func (a *App) UpdateGeoIP() error {
	return a.updateRuleFile(geoIPDownloadURL, "geoip.dat", "GeoIP")
}

func (a *App) UpdateGeoSite() error {
	return a.updateRuleFile(geoSiteDownloadURL, "geosite.dat", "GeoSite")
}

func (a *App) updateRuleFile(source, name, label string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir, err := executableDir()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	temp, err := downloadRuleFile(client, source, dir, name)
	if err != nil {
		return fmt.Errorf("更新 %s 失败: %w", label, err)
	}
	defer os.Remove(temp)
	if err := replaceRuleFile(temp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("替换 %s 文件失败: %w", label, err)
	}
	return nil
}

func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取程序目录失败: %w", err)
	}
	return filepath.Dir(exe), nil
}

func downloadRuleFile(client *http.Client, source, dir, name string) (string, error) {
	response, err := client.Get(source)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP 状态码 %d", response.StatusCode)
	}
	temp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tempName)
		}
	}()
	if _, err = io.Copy(temp, response.Body); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	info, err := os.Stat(tempName)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", errors.New("下载文件为空")
	}
	return tempName, nil
}

func replaceRuleFile(source, target string) error {
	backup := target + ".bak"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(target, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	return os.Remove(backup)
}

func (a *App) SaveConfig(cfg Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg.Proxy = strings.TrimSpace(cfg.Proxy)
	cfg.Device = strings.TrimSpace(cfg.Device)
	cfg.Interface = strings.TrimSpace(cfg.Interface)
	cfg.GeoIPFile = strings.TrimSpace(cfg.GeoIPFile)
	if err := validateAndNormalizeConfig(&cfg); err != nil {
		return err
	}

	// Normalize the path before writing so the checksum below matches the
	// exact bytes produced by saveLocked.
	normalized := &App{cfg: cfg}
	normalized.normalizeConfigLocked()
	cfg = normalized.cfg

	oldCfg := a.cfg
	running := engine.Running()
	a.cfg = cfg
	if err := a.saveLocked(); err != nil {
		a.cfg = oldCfg
		return err
	}
	a.configLoaded = true
	// Write config to disk first. Registry changes are best-effort and
	// must never block config persistence.
	_ = setStartWithWindows(cfg.StartWithWindows)
	if running {
		if err := a.applyRunningConfigLocked(oldCfg); err != nil {
			a.err = err
			// Keep the saved proxy on disk even when the running engine cannot
			// hot-reload. The user can stop/start to apply it cleanly.
			return nil
		}
	}
	a.err = nil
	return nil
}

func validateAndNormalizeConfig(cfg *Config) error {
	if strings.TrimSpace(cfg.Proxy) == "" {
		return errors.New("SOCKS5 地址不能为空")
	}
	if strings.TrimSpace(cfg.Device) == "" {
		return errors.New("TUN 设备不能为空")
	}
	switch cfg.Mode {
	case "", "global", "rules", "bypass", "direct":
	default:
		return fmt.Errorf("不支持的分流模式: %s", cfg.Mode)
	}
	if cfg.Mode == "bypass" {
		cfg.Mode = "rules"
	}
	if cfg.Mode == "" {
		cfg.Mode = "global"
	}
	if cfg.DefaultRoute != "direct" && cfg.DefaultRoute != "proxy" {
		cfg.DefaultRoute = "proxy"
	}
	if cfg.DirectRules == nil {
		cfg.DirectRules = []string{}
	}
	if cfg.ProxyRules == nil {
		cfg.ProxyRules = []string{}
	}
	if cfg.DirectCIDRs != nil {
		cfg.DirectRules = append(append([]string{}, cfg.DirectCIDRs...), cfg.DirectRules...)
	}
	if cfg.GeoIPFile == "" {
		cfg.GeoIPFile = "geoip.dat"
	}
	return nil
}

func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.startLocked()
	a.err = err
	return err
}

func (a *App) buildEngineKeyLocked(cfg Config) *engine.Key {
	interfaceName := strings.TrimSpace(cfg.Interface)
	if interfaceName == "" {
		interfaceName = a.runtimeInterface
	}
	return &engine.Key{
		Proxy:        strings.TrimSpace(cfg.Proxy),
		Device:       strings.TrimSpace(cfg.Device),
		Interface:    interfaceName,
		RoutingMode:  strings.TrimSpace(cfg.Mode),
		DirectCIDRs:  append([]string(nil), cfg.DirectCIDRs...),
		DirectRules:  append([]string(nil), cfg.DirectRules...),
		ProxyRules:   append([]string(nil), cfg.ProxyRules...),
		DefaultRoute: cfg.DefaultRoute,
		GeoIPFile:    cfg.GeoIPFile,
		LogLevel:     "info",
	}
}

func (a *App) startLocked() error {
	if engine.Running() {
		return nil
	}
	if strings.TrimSpace(a.cfg.Proxy) == "" {
		return errors.New("SOCKS5 地址不能为空")
	}
	if strings.TrimSpace(a.cfg.Device) == "" {
		return errors.New("TUN 设备不能为空")
	}
	a.normalizeConfigLocked()
	if err := checkProxyEndpoint(a.cfg.Proxy); err != nil {
		return err
	}
	if a.cfg.AutoRoute {
		if err := a.selectRuntimeInterfaceLocked(); err != nil {
			return err
		}
	}
	engine.Insert(a.buildEngineKeyLocked(a.cfg))
	if err := engine.StartE(); err != nil {
		return fmt.Errorf("启动核心失败: %w", err)
	}
	if a.cfg.AutoRoute {
		if err := a.setupRoutesLocked(); err != nil {
			if a.up.active {
				return fmt.Errorf("%w；系统路由仍需清理，核心保持运行", err)
			}
			_ = engine.StopE()
			return err
		}
	}
	return nil
}

func (a *App) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.stopLocked()
	a.err = err
	return err
}

func (a *App) stopLocked() (firstErr error) {
	if a.up.active {
		if err := a.teardownRoutesLocked(); err != nil {
			return fmt.Errorf("停止核心前清理系统路由失败: %w", err)
		}
	}
	if err := engine.StopE(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (a *App) applyRunningConfigLocked(oldCfg Config) error {
	if !engine.Running() {
		return nil
	}
	newCfg := a.cfg
	if oldCfg.Device != newCfg.Device || oldCfg.Interface != newCfg.Interface {
		if err := a.stopLocked(); err != nil {
			return fmt.Errorf("应用新配置前停止旧核心失败: %w", err)
		}
		if err := a.startLocked(); err != nil {
			_ = a.startWithConfigLocked(oldCfg)
			return fmt.Errorf("应用新配置失败: %w", err)
		}
		return nil
	}

	oldRoute := a.up
	routeNeedsRebuild := oldCfg.AutoRoute && newCfg.AutoRoute && oldCfg.Proxy != newCfg.Proxy
	if !newCfg.AutoRoute && oldRoute.active {
		if err := a.teardownRoutesLocked(); err != nil {
			return fmt.Errorf("关闭 Windows 自动路由失败: %w", err)
		}
	}
	if routeNeedsRebuild {
		if err := a.teardownRoutesLocked(); err != nil {
			return fmt.Errorf("重建 SOCKS5 防环路由失败: %w", err)
		}
	}
	if err := engine.Reload(a.buildEngineKeyLocked(newCfg)); err != nil {
		if routeNeedsRebuild || (oldRoute.active && !newCfg.AutoRoute) {
			if a.up.active {
				_ = a.teardownRoutesLocked()
			}
			a.cfg = oldCfg
			if oldCfg.AutoRoute {
				_ = a.setupRoutesLocked()
			}
		}
		a.cfg = newCfg
		return fmt.Errorf("热更新核心配置失败: %w", err)
	}

	if newCfg.AutoRoute && !oldRoute.active {
		if err := a.setupRoutesLocked(); err != nil {
			_ = engine.Reload(a.buildEngineKeyLocked(oldCfg))
			a.cfg = oldCfg
			if oldCfg.AutoRoute {
				_ = a.setupRoutesLocked()
			}
			return fmt.Errorf("启用 Windows 自动路由失败: %w", err)
		}
	}
	if routeNeedsRebuild {
		if err := a.setupRoutesLocked(); err != nil {
			_ = engine.Reload(a.buildEngineKeyLocked(oldCfg))
			a.cfg = oldCfg
			if oldCfg.AutoRoute {
				_ = a.setupRoutesLocked()
			}
			return fmt.Errorf("重建 Windows 路由失败: %w", err)
		}
	}
	return nil
}

func (a *App) startWithConfigLocked(cfg Config) error {
	saved := a.cfg
	a.cfg = cfg
	err := a.startLocked()
	if err != nil {
		a.cfg = saved
	}
	return err
}

func (a *App) normalizeConfigLocked() {
	_ = validateAndNormalizeConfig(&a.cfg)
	if path, ok := bundledDataPath(a.cfg.GeoIPFile); ok {
		a.cfg.GeoIPFile = path
	}
}

func bundledDataPath(name string) (string, bool) {
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		name = filepath.Base(name)
	}
	exe, err := os.Executable()
	if err == nil {
		path := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return name, false
}

func (a *App) configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func (a *App) load() error {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return err
	}
	data = []byte(strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")))
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmtConfigError(err)
	}
	if cfg.Proxy == "" || cfg.Device == "" {
		return errors.New("配置文件缺少 proxy 或 device")
	}
	a.cfg = cfg
	return nil
}

func (a *App) saveLocked() error {
	path := a.configPath()
	data, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("保存配置后无法读取文件: %w", err)
	}
	if !bytes.Equal(stored, data) {
		return errors.New("保存配置校验失败: 文件内容与当前配置不一致")
	}
	sum := sha256.Sum256(data)
	a.configDiskChecksum = hex.EncodeToString(sum[:])
	return nil
}

func (a *App) refreshConfigDiskChecksumLocked() {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return
	}
	sum := sha256.Sum256(data)
	a.configDiskChecksum = hex.EncodeToString(sum[:])
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func checkProxyEndpoint(rawURL string) error {
	proxyURL := rawURL
	if !strings.Contains(proxyURL, "://") {
		proxyURL = "socks5://" + proxyURL
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("SOCKS5 地址无效: %w", err)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return errors.New("SOCKS5 地址必须包含主机和端口")
	}
	address := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		return fmt.Errorf("SOCKS5 入口不可达 %s: %w", address, err)
	}
	_ = conn.Close()
	return nil
}
