package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

type App struct {
	ctx context.Context
	mu  sync.Mutex
	cfg Config
	up  routeState
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
	StartWithWindows bool   `json:"startWithWindows"`
}

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
	a.load()
	a.normalizeConfigLocked()
}

func (a *App) shutdown(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	defer a.mu.Unlock()
	if engine.Running() || a.up.active {
		_ = a.stopLocked()
	}
	_ = a.saveLocked()
}

func (a *App) GetConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
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
		StartWithWindows: a.cfg.StartWithWindows,
	}
}

func (a *App) GetTrafficStats() engine.TrafficStats {
	return engine.GetTrafficStats()
}

func (a *App) SaveConfig(cfg Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	if err := setStartWithWindows(cfg.StartWithWindows); err != nil {
		return err
	}
	a.cfg = cfg
	return a.saveLocked()
}

func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
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

	key := &engine.Key{
		Proxy:        strings.TrimSpace(a.cfg.Proxy),
		Device:       strings.TrimSpace(a.cfg.Device),
		Interface:    strings.TrimSpace(a.cfg.Interface),
		RoutingMode:  strings.TrimSpace(a.cfg.Mode),
		DirectCIDRs:  append([]string(nil), a.cfg.DirectCIDRs...),
		DirectRules:  append([]string(nil), a.cfg.DirectRules...),
		ProxyRules:   append([]string(nil), a.cfg.ProxyRules...),
		DefaultRoute: a.cfg.DefaultRoute,
		GeoIPFile:    a.cfg.GeoIPFile,
		LogLevel:     "info",
	}
	engine.Insert(key)
	if err := engine.StartE(); err != nil {
		return fmt.Errorf("启动核心失败: %w", err)
	}
	if a.cfg.AutoRoute {
		if err := a.setupRoutesLocked(); err != nil {
			_ = engine.StopE()
			return err
		}
	}
	return nil
}

func (a *App) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopLocked()
}

func (a *App) stopLocked() error {
	var firstErr error
	if a.up.active {
		if err := a.teardownRoutesLocked(); err != nil {
			firstErr = err
		}
	}
	if err := engine.StopE(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (a *App) normalizeConfigLocked() {
	if a.cfg.Mode == "bypass" {
		a.cfg.Mode = "rules"
	}
	if a.cfg.Mode == "" {
		a.cfg.Mode = "global"
	}
	if a.cfg.DefaultRoute != "direct" && a.cfg.DefaultRoute != "proxy" {
		a.cfg.DefaultRoute = "proxy"
	}
	if a.cfg.DirectRules == nil {
		a.cfg.DirectRules = []string{}
	}
	if a.cfg.ProxyRules == nil {
		a.cfg.ProxyRules = []string{}
	}
	if a.cfg.GeoIPFile == "" {
		a.cfg.GeoIPFile = "geoip.dat"
	}
	if path, ok := bundledDataPath(a.cfg.GeoIPFile); ok {
		a.cfg.GeoIPFile = path
	}
}

func bundledDataPath(name string) (string, bool) {
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		return name, false
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
	dir, err := os.UserConfigDir()
	if err != nil {
		return "tunflow.json"
	}
	return filepath.Join(dir, "TunFlow", "config.json")
}

func (a *App) load() {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return
	}
	var cfg Config
	if json.Unmarshal(data, &cfg) == nil && cfg.Proxy != "" && cfg.Device != "" {
		a.cfg = cfg
	}
}

func (a *App) saveLocked() error {
	path := a.configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
