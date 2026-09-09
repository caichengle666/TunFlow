//go:build !windows

package main

import "errors"

func currentNetworkSignature(_ string) (string, error) { return "", nil }

func resolveProxyIPs(_ string) ([]string, error) { return nil, nil }

func (a *App) selectRuntimeInterfaceLocked() error { return nil }

type routeState struct {
	active bool
}

func (a *App) setupRoutesLocked() error {
	return errors.New("自动系统路由目前仅实现 Windows；可通过 TUN 参数自行配置其他系统")
}

func (a *App) teardownRoutesLocked() error {
	a.up = routeState{}
	return nil
}
