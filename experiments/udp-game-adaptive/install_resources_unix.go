//go:build !windows

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func freeDisk(path string) (uint64, error) {
	for {
		var stat unix.Statfs_t
		if e := unix.Statfs(path, &stat); e == nil {
			return stat.Bavail * uint64(stat.Bsize), nil
		} else if !os.IsNotExist(e) {
			return 0, e
		}
		next := filepath.Dir(path)
		if next == path {
			return 0, fmt.Errorf("无法检查磁盘空间：%s", path)
		}
		path = next
	}
}
func checkInstallSpace(exe string, cfg agentConfig) error {
	info, e := os.Stat(exe)
	if e != nil {
		return e
	}
	// The old executable remains until the new copy is atomically renamed.
	required := uint64(info.Size()) + 16<<20
	for _, path := range []string{filepath.Dir(installedBinary), "/etc/uulink", "/var/lib/uulink"} {
		free, e := freeDisk(path)
		if e != nil {
			return e
		}
		if free < required {
			return fmt.Errorf("%s 可用 %d MiB，安装/升级至少需要 %d MiB（含新二进制与余量）", path, free>>20, required>>20)
		}
	}
	if cfg.features().desktop && recommendLowMemory(false, memoryTotal()) {
		fmt.Println("提示：当前内存不超过 512 MiB，桌面编码可能耗尽资源；建议选择精简或移除 desktop。")
	}
	return nil
}

// Bound persistent logs on tiny disks without requiring logrotate or syslog.
