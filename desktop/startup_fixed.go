package main

import (
	"context"
	"os"
)

// startupFixed initializes application state while holding a.mu, then
// initializes the system tray only after releasing the lock. This avoids
// the lock re-entry path startup -> startSystemTray -> refreshTrayMenu ->
// GetStatus -> a.mu.Lock().
func (a *App) startupFixed(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx

	if err := a.load(); err != nil {
		if os.IsNotExist(err) {
			a.normalizeConfigLocked()
			if saveErr := a.saveLocked(); saveErr != nil {
				a.err = saveErr
				a.configLoaded = false
			} else {
				a.configLoaded = true
				a.err = nil
			}
		} else {
			// Never replace an existing user config with defaults just because
			// it is temporarily invalid or incomplete. Keep the file intact and
			// expose the parsing error through GetStatus().
			a.err = err
			a.configLoaded = false
		}
	} else {
		a.configLoaded = true
		a.err = nil
	}

	a.normalizeConfigLocked()
	a.refreshConfigDiskChecksumLocked()
	a.startConfigWatcherLocked()
	a.mu.Unlock()

	// Tray initialization must happen outside a.mu. refreshTrayMenu() calls
	// GetStatus(), which acquires a.mu itself.
	a.startSystemTray()
}
