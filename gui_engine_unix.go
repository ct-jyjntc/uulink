//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

const appEngineName = "uulink-engine"

func startAppEngine(cmd *exec.Cmd) (func(bool), error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func(force bool) {
		sig := syscall.SIGTERM
		if force {
			sig = syscall.SIGKILL
		}
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}, nil
}
