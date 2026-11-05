package main

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

type controllerDevice struct {
	ID           string `json:"device_id"`
	Name         string `json:"alias"`
	Status       string `json:"status"`
	Availability string `json:"publisher_availability_status"`
}

func controllerDevices(ctx context.Context, config string) ([]controllerDevice, error) {
	if e := loadAgentConfig(config); e != nil {
		return nil, e
	}
	var data struct {
		Mine   []controllerDevice `json:"my_binded_devices"`
		Shared []controllerDevice `json:"others_shared_devices"`
	}
	if e := apiRequest(ctx, "GET", "/api/v1/device/list", nil, &data); e != nil {
		return nil, e
	}
	out := []controllerDevice{}
	seen := map[string]bool{}
	for _, d := range append(data.Mine, data.Shared...) {
		if d.ID != runtimeConfig.DeviceID && safeControllerID.MatchString(d.ID) && !seen[d.ID] {
			out = append(out, d)
			seen[d.ID] = true
		}
	}
	return out, nil
}

func readControllerStatus(path string) controllerStatus {
	var s controllerStatus
	b, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(b, &s) != nil || time.Since(s.Updated) > 5*time.Second {
		return controllerStatus{State: "状态未更新"}
	}
	return s
}
