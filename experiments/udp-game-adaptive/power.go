//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var powerMu sync.Mutex
var powerPending bool

func requestPower(action string) error {
	if action != "reboot" && action != "poweroff" {
		return fmt.Errorf("unsupported power action")
	}
	if !runtimeConfig.AllowPower {
		return fmt.Errorf("remote power actions are disabled")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("power actions require root")
	}
	powerMu.Lock()
	defer powerMu.Unlock()
	if powerPending {
		return nil
	}
	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	program, args, err := powerCommand(manager, action, exec.LookPath)
	if err != nil {
		return err
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	marker, _ := json.Marshal(map[string]string{"action": action, "requested_at": time.Now().UTC().Format(time.RFC3339Nano), "boot_id": string(boot)})
	dir := "/var/lib/uulink"
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "last-power-action.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(marker)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return err
	}
	cmd := exec.Command(program, args...)
	if manager == openRC {
		// A separate session survives the agent stopping during shutdown.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return err
		}
		go func() {
			if e := cmd.Wait(); e != nil {
				log.Printf("POWER action=%s failed: %v", action, e)
				powerMu.Lock()
				powerPending = false
				powerMu.Unlock()
			}
		}()
	} else if out, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("schedule power action: %w (%s)", e, out)
	}
	powerPending = true
	log.Printf("POWER action=%s scheduled via=%s", action, manager)
	return nil
}

func powerCommand(manager serviceManager, action string, lookup func(string) (string, error)) (string, []string, error) {
	if action != "reboot" && action != "poweroff" {
		return "", nil, fmt.Errorf("unsupported power action")
	}
	if manager == openRC {
		power, e := lookup(action)
		if e != nil {
			return "", nil, e
		}
		sh, e := lookup("sh")
		if e != nil {
			return "", nil, e
		}
		return sh, []string{"-c", `sleep 2; exec "$1"`, "uulink-power", power}, nil
	}
	systemctl, e := lookup("systemctl")
	if e != nil {
		return "", nil, e
	}
	unit := "uulink-power-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	return "systemd-run", []string{"--quiet", "--unit=" + unit, "--on-active=2s", "--collect", systemctl, action}, nil
}
func signalPowerAction(raw []byte) string { return signalPowerActionDepth(raw, 0) }
func signalPowerActionDepth(raw []byte, depth int) string {
	if depth > 4 || len(raw) > 65536 {
		return ""
	}
	if len(raw) > 0 && raw[0] == '"' {
		var inner string
		if json.Unmarshal(raw, &inner) != nil {
			return ""
		}
		return signalPowerActionDepth([]byte(inner), depth+1)
	}
	var v struct {
		Type   string          `json:"type"`
		Action string          `json:"action"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if v.Type == "transport_rpc" {
		var rpc struct {
			Procedure string `json:"p"`
		}
		data := v.Data
		if len(data) > 0 && data[0] == '"' {
			var inner string
			if json.Unmarshal(data, &inner) != nil {
				return ""
			}
			data = []byte(inner)
		}
		if json.Unmarshal(data, &rpc) != nil {
			return ""
		}
		switch rpc.Procedure {
		case "shutdown":
			return "poweroff"
		case "reboot":
			return "reboot"
		default:
			return ""
		}
	}
	if v.Type != "device_reboot" && v.Type != "device_shutdown" {
		if len(v.Data) > 0 {
			return signalPowerActionDepth(v.Data, depth+1)
		}
		return ""
	}
	action := v.Action
	if len(v.Data) > 0 && v.Data[0] == '"' {
		var inner string
		if json.Unmarshal(v.Data, &inner) == nil {
			v.Data = []byte(inner)
		}
	}
	var d struct {
		Action string `json:"action"`
	}
	json.Unmarshal(v.Data, &d)
	if d.Action != "" {
		action = d.Action
	}
	if v.Type == "device_shutdown" || action == "shutdown" || action == "poweroff" {
		return "poweroff"
	}
	if action == "" || action == "reboot" || action == "restart" {
		return "reboot"
	}
	return ""
}
