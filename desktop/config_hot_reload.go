package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/xjasonlyu/tun2socks/v2/engine"
)

const configWatchInterval = 700 * time.Millisecond

func (a *App) startConfigWatcherLocked() {
	if a.configWatchStop != nil {
		return
	}
	stop := make(chan struct{})
	a.configWatchStop = stop
	a.configWatchWG.Add(1)
	go func() { defer a.configWatchWG.Done(); a.configWatchLoop(stop) }()
}

func (a *App) configWatchLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			a.reloadConfigFromDisk()
		}
	}
}

func readConfig(path string) (Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, nil, err
	}
	clean := trimBOMSpace(data)
	if len(clean) == 0 {
		return Config{}, nil, errors.New("配置文件为空")
	}
	var cfg Config
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return Config{}, nil, fmt.Errorf("JSON 配置格式无效: %w", err)
	}
	cfg.Proxy = normalizeProxy(cfg.Proxy)
	if err := normalizeConfig(&cfg); err != nil {
		return Config{}, nil, err
	}
	return cfg, clean, nil
}

func trimBOMSpace(data []byte) []byte {
	for len(data) > 0 && (data[0] == ' ' || data[0] == '\t' || data[0] == '\r' || data[0] == '\n') {
		data = data[1:]
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	for len(data) > 0 && (data[len(data)-1] == ' ' || data[len(data)-1] == '\t' || data[len(data)-1] == '\r' || data[len(data)-1] == '\n') {
		data = data[:len(data)-1]
	}
	return data
}

func checksum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (a *App) reloadConfigFromDisk() {
	cfg, data, err := readConfig(a.configPath())
	if err != nil {
		return
	}
	sum := checksum(data)
	a.mu.Lock()
	if sum == a.configDiskChecksum {
		a.mu.Unlock()
		return
	}
	cfg2, data2, err2 := readConfig(a.configPath())
	if err2 != nil || checksum(data2) != sum || !reflect.DeepEqual(cfg, cfg2) {
		a.mu.Unlock()
		return
	}

	old := cloneConfig(a.cfg)
	running := engine.Running()
	a.cfg = cfg
	if running {
		if err := a.applyRunningConfigLocked(old); err != nil {
			a.cfg = old
			a.configDiskChecksum = sum
			a.err = err
			a.mu.Unlock()
			a.emitConfigReload("failed", err.Error())
			return
		}
	}
	a.configDiskChecksum = sum
	a.configLoaded = true
	a.err = nil
	a.mu.Unlock()
	a.emitConfigReload("applied", "")
}

func (a *App) emitConfigReload(status, message string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "config-reloaded", map[string]string{"status": status, "message": message})
}
