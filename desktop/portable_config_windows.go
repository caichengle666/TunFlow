//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// TunFlow keeps its configuration next to the portable executable.
// app.go already builds its config path as %APPDATA%\\TunFlow\\config.json;
// for the packaged portable layout (usually <parent>\\TunFlow\\TunFlow.exe),
// redirect the process-local APPDATA root to the executable's parent so that
// the resulting path is exactly <exe-dir>\\config.json.
func init() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exeDir := filepath.Dir(exe)
	if !strings.EqualFold(filepath.Base(exeDir), "TunFlow") {
		return
	}
	if err := os.Setenv("APPDATA", filepath.Dir(exeDir)); err != nil {
		return
	}
}
