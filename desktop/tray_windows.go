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

const trayIconBase64 = "AAABAAIAEBAAAAAAIABjAgAAJgAAACAgAAAAACAA8gAAAIkCAACJUE5HDQoaCgAAAA1JSERSAAAAEAAAABAIBgAAAB/z/2EAAAIqSURBVHicfZO/axRBGIafb3Zn7/a8PU0uF0lOECux0MreLmCKgAEFSyurINgJsRX8C7TVv8BeC9OoRSBgkyIEwQMLSaJ3ZM/czux8FnuXcOZwYIr58T7z8b7fSNbqKLOGyPRaZ1+LZwnFGELhgLFIQWyMGEHL8B+ACIRA0R9Qv9xBjEFVkSjC/fqNHxXYVoaW5QyACKFwRInl+tPHdO+tYuIIDQFTSxjs7rH/+i2Hn7exrRYaqkoka3V08rJEhlsvn7O8tkJxcAQCUJ1FjZTgPTsbm/zc+kTUaEAIGACJDP74mGuPHrK8tsKf3g+0LFFfot6jqrj+AELg5otn1BfaqHMggkGEMCqoLXborq9SHBxhElv5MZmAxDHl8IRkYY7u+l18niORqSqoLBDExufjO41RQA1ChFg7w0TVKXf/FYv1YB2SujNvTgHjqKJa7dTdKXHN4XoLuN4FdDmlzEcoHhAMqoiNKfp9Brt7xI30rBIFrMd9bzP8cIPR16vkHxepDx9gmy3Ul+MUjKEcFey/ekPwnihNUedRF4AC18sII8U0PRoFsqU72MY8GooKoGXAZhmHX7bZ2djE50OS9hxJZ45kfp569yJxaiDEmMgQwgnBjwAzbqRJEsbg8yG1Tpsr66uItYgoZV5QG94nW7pNKB3f3j/haO8dJmlOAyaQ4Bw+zyc7KB7bbBGnl9CyxJ8cYuIGoOcB46ZAIjOOSiuI92jw499qQau0/gLFHPjk4UMw1AAAAABJRU5ErkJggolQTkcNChoKAAAADUlIRFIAAAAgAAAAIAgGAAAAc3p69AAAALlJREFUeJxj5OUT/c8wgIBpIC0fdcCoAxgYGBhYSNXgdHgDQTX7bAOINo+R2GxIjMXkOISoKCDHcmL1EXQAuZYTqx+vAyi1nBhzBjwX4HQAtXxPyLzBGwKjDhh1AL0AyZURMeDTcisMMb7IY1jVUj0EsFmOTxynA0ipUglZAgMmWa+IdwC9AF4HkBMKVHUAPRxBVBTQ0hFEN8lggFAtiS8hnpkmRrkDiAHYUjs2y2nmAFLA4M6GI8IBABYFMQA/6VKcAAAAAElFTkSuQmCC"

var trayOnce sync.Once

func (a *App) startSystemTray() {
	trayOnce.Do(func() {
		go systray.Run(func() {
			if icon, err := base64.StdEncoding.DecodeString(trayIconBase64); err == nil {
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
	t := time.NewTicker(time.Second)
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
		systray.SetTooltip(fmt.Sprintf("%s\n↓ %s/s   ↑ %s/s\n累计 ↓ %s   ↑ %s", title, formatBytes(stats.DownloadPerSecond), formatBytes(stats.UploadPerSecond), formatBytes(stats.DownloadTotal), formatBytes(stats.UploadTotal)))
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

func formatBytes(v int64) string {
	if v < 0 {
		v = 0
	}
	const unit = int64(1024)
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	value := float64(v)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		value /= float64(unit)
		if value < float64(unit) {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PB", value/float64(unit))
}
