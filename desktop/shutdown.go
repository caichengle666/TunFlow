package main

import (
	"context"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

// shutdownNoPersist stops runtime resources without rewriting config.json.
// config.json is the persistent source of truth and may have been edited
// externally while the application was running.
func (a *App) shutdownNoPersist(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	watchStop := a.configWatchStop
	a.configWatchStop = nil
	if watchStop != nil {
		close(watchStop)
	}
	trayEnd := a.trayEnd
	a.trayEnd = nil
	if engine.Running() || a.up.active {
		_ = a.stopLocked()
	}
	a.mu.Unlock()

	if watchStop != nil {
		a.configWatchWG.Wait()
	}
	if trayEnd != nil {
		trayEnd()
	}
}
