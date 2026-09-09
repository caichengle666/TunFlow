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
	Proxy       string   `json:"proxy"`
	Device      string   `json:"device"`
	Interface   string   `json:"interface"`
	Mode        string   `json:"mode"`
	DirectCIDRs []string `json:"directCIDRs"`
	AutoRoute   bool     `json:"autoRoute"`
}

type Status struct {
	Running    bool   `json:"running"`
	Proxy      string `json:"proxy"`
	Device     string `json:"device"`
	Mode       string `json:"mode"`
	AutoRoute  bool   `json:"autoRoute"`
	RouteReady bool   `json:"routeReady"`
}

func NewApp() *App {
	return &App{cfg: Config{
		Proxy:       "socks5://127.0.0.1:1080",
		Device:      "tun://TunFlow",
		Mode:        "global",
		DirectCIDRs: []string{},
		AutoRoute:   true,
	}}
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.load()
	a.mu.Unlock()
}

func (a *App) shutdown(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	defer a.mu.Unlock()
	if engine.Running() {
		_ = a.stopLocked()
	}
	a.saveLocked()
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
		Running:    engine.Running(),
		Proxy:      a.cfg.Proxy,
		Device:     a.cfg.Device,
		Mode:       a.cfg.Mode,
		AutoRoute:  a.cfg.AutoRoute,
		RouteReady: a.up.active,
	}
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
	if cfg.Mode == "" {
		cfg.Mode = "global"
	}
	if cfg.DirectCIDRs == nil {
		cfg.DirectCIDRs = []string{}
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

	key := &engine.Key{
		Proxy:       strings.TrimSpace(a.cfg.Proxy),
		Device:      strings.TrimSpace(a.cfg.Device),
		Interface:   strings.TrimSpace(a.cfg.Interface),
		RoutingMode: strings.TrimSpace(a.cfg.Mode),
		DirectCIDRs: append([]string(nil), a.cfg.DirectCIDRs...),
		LogLevel:    "info",
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
	if a.up.active {
		if err := a.teardownRoutesLocked(); err != nil {
			return err
		}
	}
	return engine.StopE()
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
		if cfg.Mode == "" {
			cfg.Mode = "global"
		}
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
