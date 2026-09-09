//go:build !windows

package main

func (a *App) startSystemTray() {}
func (a *App) HideToTray() {}
func (a *App) ShowWindow() {}
func (a *App) MinimizeToTray() {}
func (a *App) CloseToTray() {}
func (a *App) QuitApp() {}
