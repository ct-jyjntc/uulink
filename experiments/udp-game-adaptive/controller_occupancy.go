package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const controllerJoinPrefix = "/api/v1/room/join/by_device/"
const occupiedAttemptSuffix = ".room-busy.json"

type occupiedAttempt struct {
	ObservedAt time.Time `json:"observed_at"`
}

func (r *statusReporter) occupiedPath() string {
	return strings.TrimSuffix(r.path, ".json") + occupiedAttemptSuffix
}

func (r *statusReporter) clearOccupiedAttempt() {
	if err := os.Remove(r.occupiedPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("clear previous room occupancy: %v", err)
	}
}

// Keep the last confirmed refusal separate from the worker heartbeat so it can
// be shown briefly even after an on-demand latency worker exits.
func (r *statusReporter) recordAttemptEnd(device string, err error) {
	var reply *providerAPIError
	if !errors.As(err, &reply) || reply.path != controllerJoinPrefix+device ||
		reply.status != http.StatusBadRequest || reply.code != 2002 {
		r.clearOccupiedAttempt()
		return
	}
	if err := atomicControllerJSON(r.occupiedPath(), occupiedAttempt{ObservedAt: time.Now().UTC()}); err != nil {
		log.Printf("record room occupancy: %v", err)
	}
}

func (r *statusReporter) connected(rules map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.State != "已连接" {
		r.clearOccupiedAttempt()
	}
	r.status = controllerStatus{State: "已连接", Rules: rules}
}
