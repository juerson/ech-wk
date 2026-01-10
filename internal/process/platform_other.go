//go:build !windows

package process

import "os/exec"

func workersBinaryName() string { return "ech-workers" }

func applyPlatformCmdTweaks(cmd *exec.Cmd) {}
