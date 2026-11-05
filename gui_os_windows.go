package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func windowsDataDir() (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "UU Link"), nil
}

func windowsGUIOptions() (guiOptions, error) {
	d, err := windowsDataDir()
	if err != nil {
		return guiOptions{}, err
	}
	return guiOptions{
		controlled:      filepath.Join(d, "controlled.json"),
		controller:      filepath.Join(d, "controller.json"),
		rules:           filepath.Join(d, "rules.json"),
		state:           filepath.Join(d, "state"),
		controllerState: filepath.Join(d, "controller-state"),
		sessionDir:      d, gatewayLog: filepath.Join(d, "logs", "connection.log"),
	}, nil
}

func writableGUIDirectory(p string) bool {
	path, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(path, windows.FILE_WRITE_DATA|windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

func machineID() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("MachineGuid")
	return value
}

func guiOpen(link string) {
	verb, _ := windows.UTF16PtrFromString("open")
	path, err := windows.UTF16PtrFromString(link)
	if err == nil {
		_ = windows.ShellExecute(0, verb, path, nil, nil, 1)
	}
}

func guiRegistrationMAC() (string, error) {
	e, err := currentPhysicalEgress()
	if err != nil {
		return "", err
	}
	if len(e.iface.HardwareAddr) == 0 {
		return "", errors.New("无法读取物理网卡地址")
	}
	return e.iface.HardwareAddr.String(), nil
}
