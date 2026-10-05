//go:build !windows

package server

import (
	"os/exec"
	"runtime"
)

// openInSystemBrowser 用系统默认浏览器打开链接（macOS 用 open，其余用 xdg-open）。
func openInSystemBrowserOS(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
