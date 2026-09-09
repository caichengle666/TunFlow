package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

const configWatchInterval = 500 * time.Millisecond

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

func readConfigFile(path string) (Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, nil, errors.New("配置文件为空")
	}
	// Be tolerant of editors that save UTF-8 with a BOM.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, nil, fmtConfigError(err)
	}
	if strings.TrimSpace(cfg.Proxy) == "" {
		return Config{}, nil, errors.New("SOCKS5 地址不能为空")
	}
	if strings.TrimSpace(cfg.Device) == "" {
		return Config{}, nil, errors.New("TUN 设备不能为空")
	}
	return cfg, data, nil
}

func fmtConfigError(err error) error {
	return errors.New("JSON 配置格式无效: " + err.Error())
}

func (a *App) reloadConfigFromDisk() {
	path := a.configPath()
	cfg, data, err := readConfigFile(path)
	if err != nil {
		// Invalid/incomplete files are deliberately not marked as applied.
		// The next poll will retry after an editor finishes writing the file.
		return
	}

	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])

	a.mu.Lock()
	defer a.mu.Unlock()
	if checksum == a.configDiskChecksum {
		return
	}

	// Run the exact same normalization rules as the GUI save path, but do not
	// mutate the live configuration until the new runtime state is accepted.
	tmp := &App{cfg: cfg}
	tmp.normalizeConfigLocked()
	cfg = tmp.cfg

	if reflect.DeepEqual(cfg, a.cfg) {
		// The bytes changed but the effective configuration did not.
		a.configDiskChecksum = checksum
		a.err = nil
		return
	}

	oldCfg := a.cfg
	running := engine.Running()

	if err := setStartWithWindows(cfg.StartWithWindows); err != nil {
		a.err = err
		// Do not advance configDiskChecksum. A later poll will retry the same
		// configuration automatically after the transient error is gone.
		return
	}

	a.cfg = cfg
	if running {
		if err := a.applyRunningConfigLocked(oldCfg); err != nil {
			_ = setStartWithWindows(oldCfg.StartWithWindows)
			a.cfg = oldCfg
			a.err = err
			// Keep the checksum pointing at the last successfully applied file so
			// the watcher retries this exact external edit.
			return
		}
	}

	a.configDiskChecksum = checksum
	a.err = nil
}
