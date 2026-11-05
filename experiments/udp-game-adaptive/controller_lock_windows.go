package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func controllerLock(path string, nonblock bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonblock {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var at windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &at); err != nil {
		f.Close()
		return nil, fmt.Errorf("无法获取运行锁 %s: %w", path, err)
	}
	return f, nil
}
