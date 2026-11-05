//go:build !windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// The desktop engine is tied to the UI helper's lifetime. A killed helper
// cannot leave an orphaned tunnel or controller worker behind.
func appOwnedCLI(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("app-run requires its owning PID and connection mode")
	}
	parent, e := strconv.Atoi(args[0])
	if e != nil || parent < 2 || os.Getppid() != parent {
		return fmt.Errorf("owning application is not running")
	}
	if args[1] != "udp-connect" && args[1] != "controller-daemon" {
		return fmt.Errorf("unsupported application connection mode")
	}
	if syscall.Getpgrp() != os.Getpid() {
		return fmt.Errorf("application engine requires its own process group")
	}
	os.Setenv("UULINK_APP_OWNER_PID", strconv.Itoa(parent))
	os.Setenv("UULINK_APP_ENGINE_PGID", strconv.Itoa(os.Getpid()))
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != parent {
				_ = syscall.Kill(-os.Getpid(), syscall.SIGTERM)
				time.Sleep(3 * time.Second)
				_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
				return
			}
		}
	}()
	return runCLI(args[1:])
}

// Every TCP worker also watches the owner and its engine leader. Its own
// watchdog survives a simultaneous failure of the helper and engine leader.
func watchAppWorker() error {
	ownerText, groupText := os.Getenv("UULINK_APP_OWNER_PID"), os.Getenv("UULINK_APP_ENGINE_PGID")
	if ownerText == "" && groupText == "" {
		return nil
	}
	owner, e := strconv.Atoi(ownerText)
	if e != nil || owner < 2 {
		return fmt.Errorf("invalid application owner")
	}
	group, e := strconv.Atoi(groupText)
	if e != nil || group < 2 || syscall.Getpgrp() != group {
		return fmt.Errorf("invalid application engine group")
	}
	if os.Getppid() != group || syscall.Kill(owner, 0) != nil {
		_ = syscall.Kill(-group, syscall.SIGKILL)
		return fmt.Errorf("application owner has exited")
	}
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != group || syscall.Kill(owner, 0) != nil {
				_ = syscall.Kill(-group, syscall.SIGTERM)
				time.Sleep(3 * time.Second)
				_ = syscall.Kill(-group, syscall.SIGKILL)
				return
			}
		}
	}()
	return nil
}
