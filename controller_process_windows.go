package main

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func requestControllerStop(c *exec.Cmd) { _ = c.Process.Kill() }
func configureControllerChild(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
