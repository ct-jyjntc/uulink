//go:build !windows

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

func windowsGUIOptions() (guiOptions, error) { return guiOptions{}, errors.New("unsupported platform") }
func writableGUIDirectory(p string) bool     { return unix.Access(p, unix.W_OK|unix.X_OK) == nil }

func guiOpen(link string) {
	var c *exec.Cmd
	if runtime.GOOS == "darwin" {
		c = exec.Command("open", link)
	} else {
		c = exec.Command("xdg-open", link)
	}
	if c.Start() == nil {
		go c.Wait()
	}
}

func platformPreferences(http.ResponseWriter, *http.Request) (any, error) {
	return nil, errors.New("当前平台未使用此接口")
}

func guiRegistrationMAC() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("无法读取本机网络设备地址：%w", err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 && len(iface.HardwareAddr) > 0 {
			return iface.HardwareAddr.String(), nil
		}
	}
	return "", errors.New("无法读取本机网络设备地址，请检查网络设备后重试")
}
