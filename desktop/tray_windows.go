//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const trayIconBase64 = "AAABAAIAEBAAAAAAIABjAgAAJgAAACAgAAAAACAA8gAAAIkCAACJUE5HDQoaCgAAAA1JSERSAAAAEAAA" +
	"AAABAAEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

var trayOnce sync.Once

func (a *App) startSystemTray() {
	trayOnce.Do(func() {
		go systray.Run(func() {
			icon, err := base64.StdEncoding.DecodeString(trayIconBase64)
			if err == nil {
				systray.SetIcon(icon)
			}
			systray.SetTooltip("TunFlow")

			openItem := systray.AddMenuItem("打开 TunFlow", "打开控制台")
			startItem := systray.AddMenuItem("启动 TUN", "启动 TunFlow")
			stopItem := systray.AddMenuItem("停止 TUN", "停止 TunFlow")
			systray.AddSeparator()
			quitItem := systray.AddMenuItem("退出 TunFlow", "退出程序")

			go func() {
				for {
					select {
					case <-openItem.ClickedCh:
						a.ShowWindow()
					case <-startItem.ClickedCh:
						_ = a.Start()
					case <-stopItem.ClickedCh:
						_ = a.Stop()
					case <-quitItem.ClickedCh:
						runtime.Quit(a.ctx)
						return
					}
				}
			}()

			go a.updateTrayLoop(startItem, stopItem)
		}, func() {})
	})
}

func (a *App) updateTrayLoop(startItem, stopItem *systray.MenuItem) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		if a.ctx == nil {
			continue
		}
		stats := a.GetTrafficStats()
		s := a.GetStatus()
		title := "TunFlow · 已停止"
		if s.Running {
			title = "TunFlow · 运行中"
		}
		systray.SetTooltip(fmt.Sprintf("%s\n↓ %s/s   ↑ %s/s\n累计 ↓ %s   ↑ %s", title, formatBytes(stats.DownloadSpeed), formatBytes(stats.UploadSpeed), formatBytes(stats.Download), formatBytes(stats.Upload)))
		startItem.Check(!s.Running)
		stopItem.Check(s.Running)
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

func (a *App) QuitApp() {
	if a.ctx != nil {
		runtime.Quit(context.Background())
	}
}

func formatBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	value := float64(v)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
