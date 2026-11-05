//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func requestControllerStop(c *exec.Cmd)  { _ = c.Process.Signal(syscall.SIGTERM) }
func configureControllerChild(*exec.Cmd) {}
