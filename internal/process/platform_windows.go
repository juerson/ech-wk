//go:build windows

package process

import (
	"os/exec"
	"syscall"
)

func workersBinaryName() string { return "ech-workers.exe" }

func applyPlatformCmdTweaks(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
