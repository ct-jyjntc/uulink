package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type tunnelLatency struct {
	Milliseconds float64   `json:"ms"`
	Updated      time.Time `json:"updated"`
}

// Echo over the authenticated peer's actual data channel. No external ICMP,
// signaling API duration or remote wall clock participates in the measurement.
func replyTunnelProbe(sender pmSender, raw []byte) bool {
	if len(raw) != 12 || string(raw[:4]) != "UPM1" {
		return false
	}
	reply := append([]byte(nil), raw...)
	copy(reply, "UQM1")
	_ = sender.Send(reply)
	return true
}
func (m *controllerMappings) handleProbe(raw []byte) bool {
	if len(raw) != 12 || string(raw[:4]) != "UQM1" {
		return false
	}
	m.mu.Lock()
	id := binary.BigEndian.Uint64(raw[4:])
	valid := id == m.probeID && !m.probeStarted.IsZero() && !m.closed
	elapsed := time.Since(m.probeStarted)
	callback := m.onLatency
	if valid {
		m.probeStarted = time.Time{}
	}
	m.mu.Unlock()
	if valid && callback != nil {
		callback(elapsed)
	}
	return true
}
func (m *controllerMappings) startProbes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				return
			}
			if !m.probeStarted.IsZero() && time.Since(m.probeStarted) < 10*time.Second {
				m.mu.Unlock()
				select {
				case <-m.ctx.Done():
					return
				case <-ticker.C:
				}
				continue
			}
			m.probeID++
			id := m.probeID
			m.probeStarted = time.Now()
			m.mu.Unlock()
			raw := make([]byte, 12)
			copy(raw, "UPM1")
			binary.BigEndian.PutUint64(raw[4:], id)
			_ = m.sender.Send(raw)
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func readProbeRequests(path string) map[string]time.Time {
	requests := map[string]time.Time{}
	b, e := os.ReadFile(path)
	if e == nil {
		_ = json.Unmarshal(b, &requests)
	}
	return requests
}
func activeProbeRequests(path string) map[string]time.Time {
	out := map[string]time.Time{}
	now := time.Now()
	for device, until := range readProbeRequests(path) {
		if safeControllerID.MatchString(device) && until.After(now) && until.Before(now.Add(time.Minute)) {
			out[device] = until
		}
	}
	return out
}
func (r *statusReporter) latency(elapsed time.Duration) {
	if elapsed < 0 || elapsed > 30*time.Second {
		return
	}
	_ = atomicControllerJSON(strings.TrimSuffix(r.path, ".json")+".latency.json", tunnelLatency{float64(elapsed) / float64(time.Millisecond), time.Now().UTC()})
}
func readTunnelLatencies(dir string) map[string]tunnelLatency {
	out := map[string]tunnelLatency{}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if len(out) >= 256 {
			break
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".latency.json") {
			continue
		}
		device := strings.TrimSuffix(name, ".latency.json")
		if !safeControllerID.MatchString(device) {
			continue
		}
		var point tunnelLatency
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e == nil && json.Unmarshal(b, &point) == nil && point.Milliseconds >= 0 && point.Milliseconds <= 30000 && time.Since(point.Updated) >= 0 && time.Since(point.Updated) < 10*time.Minute {
			out[device] = point
		}
	}
	return out
}
