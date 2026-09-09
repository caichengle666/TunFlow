package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"time"

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
	go func() {
		defer a.configWatchWG.Done()
		a.configWatchLoop(stop)
	}()
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

func (a *App) reloadConfigFromDisk() {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])

	a.mu.Lock()
	defer a.mu.Unlock()
	if checksum == a.configDiskChecksum {
		return
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.Proxy == "" || cfg.Device == "" {
		return
	}

	// Normalize without mutating the live config until the new configuration
	// has passed the same normalization path as normal GUI saves.
	tmp := &App{cfg: cfg}
	tmp.normalizeConfigLocked()
	cfg = tmp.cfg

	if reflect.DeepEqual(cfg, a.cfg) {
		a.configDiskChecksum = checksum
		return
	}

	oldCfg := a.cfg
	running := engine.Running()
	if err := setStartWithWindows(cfg.StartWithWindows); err != nil {
		a.err = err
		a.configDiskChecksum = checksum
		return
	}
	a.cfg = cfg
	if running {
		if err := a.applyRunningConfigLocked(oldCfg); err != nil {
			_ = setStartWithWindows(oldCfg.StartWithWindows)
			a.cfg = oldCfg
			a.err = err
			a.configDiskChecksum = checksum
			return
		}
	}
	a.err = nil
	a.configDiskChecksum = checksum
}
