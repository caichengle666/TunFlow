//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func setStartWithWindows(enabled bool) error {
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	name := "TunFlow"
	if !enabled {
		cmd := exec.Command("reg", "delete", key, "/v", name, "/f")
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if strings.Contains(strings.ToLower(msg), "unable to find") || strings.Contains(strings.ToLower(msg), "找不到") {
				return nil
			}
			return fmt.Errorf("关闭开机启动失败: %w: %s", err, msg)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("解析程序路径失败: %w", err)
	}
	value := `"` + exe + `"`
	cmd := exec.Command("reg", "add", key, "/v", name, "/t", "REG_SZ", "/d", value, "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("开启开机启动失败: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
