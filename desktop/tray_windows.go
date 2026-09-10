//go:build windows

package main

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed assets/tray.ico
var trayIconICO []byte

var (
	trayOnce      sync.Once
	trayActionMu  sync.Mutex
	trayBusy      atomic.Bool
	trayMenuMu    sync.Mutex
	trayOpenItem  *systray.MenuItem
	trayStartItem *systray.MenuItem
	trayStopItem  *systray.MenuItem
	trayQuitItem  *systray.MenuItem
)

func formatBytes(v int64) string {
	if v < 0 {
		v = 0
	}
	const unit = int64(1024)
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	value := float64(v)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		value /= float64(unit)
		if value < float64(unit) || u == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%d B", v)
}

func (a *App) startSystemTray() {
	trayOnce.Do(func() {
		startLoop, endLoop := systray.RunWithExternalLoop(func() {
			systray.SetIcon(trayIconICO)
			systray.SetTooltip("TunFlow")

			trayOpenItem = systray.AddMenuItem("打开 TunFlow", "打开控制台")
			trayStartItem = systray.AddMenuItem("启动 TUN", "启动 TunFlow")
			trayStopItem = systray.AddMenuItem("停止 TUN", "停止 TunFlow")
			systray.AddSeparator()
			trayQuitItem = systray.AddMenuItem("退出 TunFlow", "退出程序")
			a.refreshTrayMenu()

			systray.SetOnTapped(func() {
				a.ShowWindow()
			})
			go a.handleTrayMenu()
			a.startTrayTooltipLoop()
		}, func() {})
		a.trayEnd = endLoop
		startLoop()
	})
}

func (a *App) runTrayAction(action func()) {
	trayActionMu.Lock()
	defer trayActionMu.Unlock()
	if !trayBusy.CompareAndSwap(false, true) {
		return
	}
	defer trayBusy.Store(false)
	action()
	a.refreshTrayMenu()
}

func (a *App) refreshTrayMenu() {
	trayMenuMu.Lock()
	defer trayMenuMu.Unlock()

	if trayStartItem == nil || trayStopItem == nil {
		return
	}
	status := a.GetStatus()

	if status.Running {
		trayStartItem.Disable()
		trayStopItem.Enable()
	} else {
		trayStartItem.Enable()
		trayStopItem.Disable()
	}
}

func (a *App) handleTrayMenu() {
	go func() {
		for {
			select {
			case <-trayOpenItem.ClickedCh:
				a.ShowWindow()
			case <-trayStartItem.ClickedCh:
				a.runTrayAction(func() {
					if err := a.Start(); err != nil {
						a.showTrayError(err)
					}
				})
			case <-trayStopItem.ClickedCh:
				a.runTrayAction(func() {
					if err := a.Stop(); err != nil {
						a.showTrayError(err)
					}
				})
			case <-trayQuitItem.ClickedCh:
				a.QuitApp()
				return
			}
		}
	}()
}

func (a *App) startTrayTooltipLoop() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastTooltip := ""
		for range ticker.C {
			stats := a.GetTrafficStats()
			status := a.GetStatus()
			title := "TunFlow · 已停止"
			if status.Running {
				title = "TunFlow · 运行中"
			}
			tooltip := fmt.Sprintf("%s\n↓ %s/s   ↑ %s/s\n累计 ↓ %s   ↑ %s", title, formatBytes(stats.DownloadPerSecond), formatBytes(stats.UploadPerSecond), formatBytes(stats.DownloadTotal), formatBytes(stats.UploadTotal))
			if tooltip != lastTooltip {
				systray.SetTooltip(tooltip)
				lastTooltip = tooltip
			}
		}
	}()
}

func (a *App) showTrayError(err error) {
	if err == nil {
		return
	}
	if a.ctx != nil {
		runtime.WindowShow(a.ctx)
		runtime.WindowUnminimise(a.ctx)
		runtime.EventsEmit(a.ctx, "tray-error", err.Error())
	}
}

func (a *App) HideToTray() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

func (a *App) ShowWindow() {
	if a.ctx != nil {
		runtime.WindowShow(a.ctx)
		runtime.WindowUnminimise(a.ctx)
	}
}

func (a *App) MinimizeToTray() {
	a.HideToTray()
}

func (a *App) CloseToTray() {
	a.HideToTray()
}

func (a *App) ToggleMaximize() {
	if a.ctx == nil {
		return
	}
	if runtime.WindowIsMaximised(a.ctx) {
		runtime.WindowUnmaximise(a.ctx)
	} else {
		runtime.WindowMaximise(a.ctx)
	}
}

func (a *App) QuitApp() {
	if a.ctx != nil {
		runtime.Quit(context.Background())
	}
}
