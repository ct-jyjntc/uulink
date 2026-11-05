//go:build !windows

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func controllerLock(path string, nonblock bool) (*os.File, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	flags := unix.LOCK_EX
	if nonblock {
		flags |= unix.LOCK_NB
	}
	if e = unix.Flock(int(f.Fd()), flags); e != nil {
		f.Close()
		return nil, fmt.Errorf("已有进程持有锁 %s: %w", path, e)
	}
	return f, nil
}
