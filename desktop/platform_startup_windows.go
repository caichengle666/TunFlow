//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const startupRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func setStartWithWindows(enabled bool) error {
	name := "TunFlow"
	if !enabled {
		// `reg delete` returns exit code 1 when the value does not exist.
		// That is already the desired state, so probe first and only delete
		// an existing value. This keeps saving normal settings independent of
		// whether the optional startup entry has ever been created.
		query := exec.Command("reg", "query", startupRunKey, "/v", name)
		if err := query.Run(); err != nil {
			return nil
		}
		cmd := exec.Command("reg", "delete", startupRunKey, "/v", name, "/f")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("关闭开机启动失败: %w", cleanCommandOutput(out))
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
	cmd := exec.Command("reg", "add", startupRunKey, "/v", name, "/t", "REG_SZ", "/d", value, "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("开启开机启动失败: %w", cleanCommandOutput(out))
	}
	return nil
}

func cleanCommandOutput(out []byte) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return "Windows 注册表操作失败"
	}
	return msg
}
