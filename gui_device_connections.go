package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type guiDeviceConnection struct {
	State      string     `json:"state"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

const occupiedAttemptVisibility = time.Minute

func readDeviceConnections(dir string, reports map[string]controllerStatus, now time.Time) map[string]guiDeviceConnection {
	out := map[string]guiDeviceConnection{}
	for device, report := range reports {
		if report.State == "已连接" {
			out[device] = guiDeviceConnection{State: "local_connected"}
		}
	}

	entries, _ := os.ReadDir(dir)
	busy := 0
	for _, entry := range entries {
		if busy >= 256 {
			break
		}
		device, ok := strings.CutSuffix(entry.Name(), occupiedAttemptSuffix)
		if !ok || !entry.Type().IsRegular() || !safeControllerID.MatchString(device) || out[device].State == "local_connected" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var attempt occupiedAttempt
		if json.Unmarshal(raw, &attempt) == nil {
			age := now.Sub(attempt.ObservedAt)
			if age >= 0 && age < occupiedAttemptVisibility {
				out[device] = guiDeviceConnection{State: "recently_busy", ObservedAt: &attempt.ObservedAt}
				busy++
			}
		}
	}
	return out
}
