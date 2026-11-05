package main

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

func memoryTotal() int64 {
	b, _ := os.ReadFile("/proc/meminfo")
	var total int64
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[0] == "MemTotal:" {
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			total = n * 1024
			break
		}
	}
	// Small VPSes are often containers; /proc/meminfo may report the host RAM.
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, _ := os.ReadFile(path)
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 && (total == 0 || n < total) {
			total = n
		}
	}
	return total
}
func recommendLowMemory(alpine bool, memory int64) bool {
	return alpine || (memory > 0 && memory <= 512<<20)
}
func configureResources(cfg agentConfig) {
	if !cfg.LowMemory {
		return
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(64 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
}

type rotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

const maxLogBytes = 1 << 20

func openRotatingLog(path string) (*rotatingLog, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	return &rotatingLog{path: path, file: f, size: info.Size()}, nil
}
func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogBytes {
		if e := l.file.Close(); e != nil {
			return 0, e
		}
		if e := os.Rename(l.path, l.path+".1"); e != nil {
			return 0, e
		}
		f, e := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			return 0, e
		}
		l.file = f
		l.size = 0
	}
	original := len(p)
	if len(p) > maxLogBytes {
		p = p[len(p)-maxLogBytes:]
	}
	n, e := l.file.Write(p)
	l.size += int64(n)
	if e != nil {
		return n, e
	}
	return original, nil
}
func (l *rotatingLog) Close() error { l.mu.Lock(); defer l.mu.Unlock(); return l.file.Close() }
