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
	// An empty proxy is valid in the persisted configuration. The runtime
	// start path is responsible for requiring a reachable SOCKS5 endpoint.
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
		return
	}

	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])

	// Capture event data while holding the lock, but emit after releasing it.
	// The JS event handler may call GetConfig(), which also acquires a.mu.
	var emitStatus, emitMessage string
	var needEmit bool

	a.mu.Lock()
	if checksum == a.configDiskChecksum {
		a.mu.Unlock()
		return
	}

	// Read a stable snapshot twice so an editor writing the file is not
	// treated as a finished change; the next poll picks up the final data.
	path = a.configPath()
	cfg2, data2, err2 := readConfigFile(path)
	if err2 != nil {
		a.mu.Unlock()
		return
	}
	sum2 := sha256.Sum256(data2)
	checksum2 := hex.EncodeToString(sum2[:])
	if checksum2 != checksum || !reflect.DeepEqual(cfg, cfg2) {
		a.mu.Unlock()
		return
	}

	tmp := &App{cfg: cfg}
	tmp.normalizeConfigLocked()
	cfg = tmp.cfg

	if reflect.DeepEqual(cfg, a.cfg) {
		a.configDiskChecksum = checksum
		a.err = nil
		emitStatus = "unchanged"
		emitMessage = ""
		needEmit = true
		a.mu.Unlock()
		if needEmit {
			a.emitConfigReload(emitStatus, emitMessage)
		}
		return
	}

	oldCfg := a.cfg
	running := engine.Running()

	if err := setStartWithWindows(cfg.StartWithWindows); err != nil {
		a.err = err
		emitStatus = "failed"
		emitMessage = err.Error()
		needEmit = true
		a.mu.Unlock()
		if needEmit {
			a.emitConfigReload(emitStatus, emitMessage)
		}
		return
	}

	if running && strings.TrimSpace(cfg.Proxy) == "" {
		// Never replace a running core with an empty SOCKS5 endpoint. The
		// persisted file remains authoritative and will be used on restart.
		a.configDiskChecksum = checksum
		a.err = errors.New("当前运行中不能清空 SOCKS5 地址；请停止 TunFlow 后再清空")
		emitStatus = "failed"
		emitMessage = a.err.Error()
		needEmit = true
		a.mu.Unlock()
		if needEmit {
			a.emitConfigReload(emitStatus, emitMessage)
		}
		return
	}

	a.cfg = cfg
	if running {
		if err := a.applyRunningConfigLocked(oldCfg); err != nil {
			a.err = err
			// Keep the saved settings on disk even when the running core cannot
			// hot-reload them; the next start uses them.
			a.configDiskChecksum = checksum
			emitStatus = "failed"
			emitMessage = err.Error()
			needEmit = true
			a.mu.Unlock()
			if needEmit {
				a.emitConfigReload(emitStatus, emitMessage)
			}
			return
		}
	}

	a.configDiskChecksum = checksum
	a.err = nil
	emitStatus = "applied"
	emitMessage = ""
	needEmit = true
	a.mu.Unlock()
	if needEmit {
		a.emitConfigReload(emitStatus, emitMessage)
	}
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
