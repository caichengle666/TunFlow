package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/xjasonlyu/tun2socks/v2/engine"
)

const configWatchInterval = 2 * time.Second

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
			a.refreshNetworkRoutes()
		}
	}
}

func (a *App) refreshNetworkRoutes() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !engine.Running() || !a.cfg.AutoRoute || !a.up.active {
		return
	}
	signature, err := currentNetworkSignature(a.cfg.Interface)
	if err == nil {
		if ips, resolveErr := resolveProxyIPs(a.cfg.Proxy); resolveErr == nil {
			sort.Strings(ips)
			signature += "|" + strings.Join(ips, ",")
		}
	}
	if err != nil || signature == "" || signature == a.networkSignature {
		return
	}
	if err := a.teardownRoutesLocked(); err != nil {
		a.err = fmt.Errorf("网络切换时清理旧路由失败: %w", err)
		return
	}
	if err := a.selectRuntimeInterfaceLocked(); err != nil {
		a.err = fmt.Errorf("网络切换后选择物理出口失败: %w", err)
		return
	}
	if err := a.setupRoutesLocked(); err != nil {
		a.err = fmt.Errorf("网络切换后重建路由失败: %w", err)
		return
	}
	if err := engine.Reload(a.buildEngineKeyLocked(a.cfg)); err != nil {
		_ = a.teardownRoutesLocked()
		a.err = fmt.Errorf("网络切换后刷新核心网卡绑定失败: %w", err)
		return
	}
	a.networkSignature = signature
	a.err = nil
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

	// Read a stable snapshot twice so an editor writing the file is not
	// treated as a finished change; the next tick will pick up the final data.
	path = a.configPath()
	cfg2, data2, err2 := readConfigFile(path)
	if err2 != nil {
		return
	}
	sum2 := sha256.Sum256(data2)
	checksum2 := hex.EncodeToString(sum2[:])
	if checksum2 != checksum {
		return
	}
	if !reflect.DeepEqual(cfg, cfg2) {
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
		a.emitConfigReload("unchanged", "")
		return
	}

	oldCfg := a.cfg
	running := engine.Running()

	if err := setStartWithWindows(cfg.StartWithWindows); err != nil {
		a.err = err
		// Do not advance configDiskChecksum. A later poll will retry the same
		// configuration automatically after the transient error is gone.
		a.emitConfigReload("failed", err.Error())
		return
	}

	a.cfg = cfg
	if running {
		if err := a.applyRunningConfigLocked(oldCfg); err != nil {
			a.err = err
			// The file is authoritative. Keep the saved settings even when the
			// running core cannot hot-reload them; the next start uses them.
			a.configDiskChecksum = checksum
			a.emitConfigReload("failed", err.Error())
			return
		}
	}

	a.configDiskChecksum = checksum
	a.err = nil
	a.emitConfigReload("applied", "")
}

func (a *App) emitConfigReload(status, message string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "config-reloaded", map[string]string{
		"status":  status,
		"message": message,
	})
}
