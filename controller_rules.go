package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const controllerConfigPath = "/etc/uulink-controller/config.json"
const controllerRulesPath = "/etc/uulink-controller/rules.json"
const controllerStateDir = "/run/uulink-controller"

type forwardRule struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Device     string `json:"device"`
	DeviceName string `json:"device_name,omitempty"`
	Listen     string `json:"listen"`
	Target     string `json:"target"`
	Enabled    bool   `json:"enabled"`
	Protocol   string `json:"protocol,omitempty"`
	AllowLAN   *bool  `json:"allow_lan,omitempty"`
}
type ruleDocument struct {
	Version int           `json:"version"`
	Rules   []forwardRule `json:"rules"`
}

var safeControllerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validateRules(doc ruleDocument) error {
	if doc.Version != 1 {
		return fmt.Errorf("不支持的规则版本 %d", doc.Version)
	}
	if doc.Rules == nil {
		return fmt.Errorf("规则文件缺少 rules 数组")
	}
	if len(doc.Rules) > 256 {
		return fmt.Errorf("最多支持 256 条规则")
	}
	ids, ports := map[string]bool{}, map[string]string{}
	for _, r := range doc.Rules {
		if !safeControllerID.MatchString(r.ID) || !safeControllerID.MatchString(r.Device) {
			return fmt.Errorf("无效的规则或设备 ID")
		}
		if r.transport() != "tcp" && r.transport() != "udp" && r.transport() != "tcp+udp" {
			return fmt.Errorf("无效的传输协议")
		}
		if ids[r.ID] {
			return fmt.Errorf("重复规则 ID: %s", r.ID)
		}
		ids[r.ID] = true
		if strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 || strings.ContainsAny(r.Name+r.DeviceName, "\r\n\t\x1b") {
			return fmt.Errorf("规则名称为空、过长或包含控制字符")
		}
		for i, addr := range []string{r.Listen, r.Target} {
			h, p, e := net.SplitHostPort(addr)
			n, pe := strconv.Atoi(p)
			if e != nil || h == "" || pe != nil || n < 1 || n > 65535 || strings.ContainsAny(h, " /\r\n\t") {
				return fmt.Errorf("无效的端口地址: %s", addr)
			}
			if i == 0 {
				if net.ParseIP(h) == nil {
					return fmt.Errorf("监听地址必须是 IP: %s", h)
				}
				if r.Enabled {
					// Reserve a port across all local interfaces, including IPv4/IPv6 wildcards.
					port := strconv.Itoa(n)
					if other, ok := ports[port]; ok {
						return fmt.Errorf("本地端口 %s 已用于规则 %s", port, other)
					}
					ports[port] = r.Name
				}
			}
		}
	}
	return nil
}
func readRules(path string) (ruleDocument, error) {
	doc := ruleDocument{Version: 1, Rules: []forwardRule{}}
	b, e := os.ReadFile(path)
	if e != nil {
		return doc, e
	}
	doc = ruleDocument{}
	if e = json.Unmarshal(b, &doc); e != nil {
		return doc, e
	}
	return doc, validateRules(doc)
}
func atomicControllerJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".uulink-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	// Windows does not support flushing a directory handle opened by os.Open.
	// The file contents were flushed before the atomic replacement above.
	if runtime.GOOS == "windows" {
		return nil
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func updateRules(path string, change func(*ruleDocument) error) error {
	lock, e := controllerLock(path+".lock", false)
	if e != nil {
		return e
	}
	defer lock.Close()
	doc, e := readRules(path)
	if os.IsNotExist(e) {
		e = nil
	}
	if e != nil {
		return e
	}
	if e = change(&doc); e != nil {
		return e
	}
	if e = validateRules(doc); e != nil {
		return e
	}
	return atomicControllerJSON(path, doc)
}
func enabledDeviceRules(path, device string) ([]forwardRule, error) {
	doc, e := readRules(path)
	if e != nil {
		return nil, e
	}
	out := []forwardRule{}
	for _, r := range doc.Rules {
		if r.Enabled && r.Device == device {
			out = append(out, r)
		}
	}
	return out, nil
}

type activeForward struct {
	rule       forwardRule
	forward    *pmForwarder
	udp        *udpForward
	udpAttempt time.Time
	udpError   string
}

// TCP rules share one reliable channel; UDP rules have independent channels.
// Each rule owns its listeners and streams, so another rule can change live.
type controllerMappings struct {
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	ctx          context.Context
	sender       pmSender
	mu           sync.Mutex
	active       map[string]activeForward
	closed       bool
	udpFactory   func(forwardRule) (*udpForward, error)
	onLatency    func(time.Duration)
	probeID      uint64
	probeStarted time.Time
}

func newControllerMappings(ctx context.Context, sender pmSender) *controllerMappings {
	ctx, cancel := context.WithCancel(ctx)
	return &controllerMappings{ctx: ctx, cancel: cancel, sender: sender, active: map[string]activeForward{}}
}
func closeActiveForward(a activeForward) {
	if a.forward != nil {
		a.forward.close()
	}
	if a.udp != nil {
		a.udp.close()
	}
}
func (m *controllerMappings) reconcile(rules []forwardRule) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := map[string]string{}
	if m.closed {
		return states
	}
	wanted := map[string]forwardRule{}
	for _, r := range rules {
		if r.Enabled {
			wanted[r.ID] = r
		}
	}
	for id, a := range m.active {
		r, ok := wanted[id]
		if !ok || r.Listen != a.rule.Listen || r.Target != a.rule.Target || r.transport() != a.rule.transport() || r.lanAllowed() != a.rule.lanAllowed() {
			delete(m.active, id)
			closeActiveForward(a)
		}
	}
	for id, r := range wanted {
		a := m.active[id]
		a.rule = r
		var parts []string
		if r.transport() != "udp" {
			if a.forward != nil {
				a.forward.mu.Lock()
				closed := a.forward.closed
				a.forward.mu.Unlock()
				if closed {
					a.forward = nil
				}
			}
			if a.forward == nil {
				f := newPMForwarder(m.ctx, m.sender, r.Target)
				f.allowSource = func(addr net.Addr) bool { return ruleAllowsSource(r, addr) }
				if e := f.bind(r.Listen); e != nil {
					f.close()
					parts = append(parts, "TCP 监听失败: "+e.Error())
				} else {
					a.forward = f
					go func() {
						if e := f.accept(); e != nil {
							f.close()
						}
					}()
				}
			}
			if a.forward != nil {
				parts = append(parts, "TCP 就绪")
			}
		}
		if r.transport() != "tcp" {
			closed := a.udp == nil
			if a.udp != nil {
				_, closed = a.udp.status()
			}
			if closed && time.Since(a.udpAttempt) > 5*time.Second {
				a.udpAttempt = time.Now()
				a.udpError = ""
				if m.udpFactory == nil {
					a.udpError = "UDP 引擎不可用"
				} else {
					u, e := m.udpFactory(r)
					if e != nil {
						a.udpError = "UDP 监听失败: " + e.Error()
					} else {
						a.udp = u
					}
				}
			}
			if a.udpError != "" {
				parts = append(parts, a.udpError)
			} else if a.udp != nil {
				st, _ := a.udp.status()
				parts = append(parts, st)
			} else {
				parts = append(parts, "UDP 等待重试")
			}
		}
		m.active[id] = a
		states[id] = strings.Join(parts, " · ")
	}
	return states
}
func (m *controllerMappings) handle(b []byte) {
	if m.handleProbe(b) {
		return
	}
	f, e := decodePM(b)
	if e != nil || f.kind == pmDATAACK {
		return
	}
	m.mu.Lock()
	var dst *pmForwarder
	for _, a := range m.active {
		if a.forward != nil && a.forward.session == f.key.session {
			dst = a.forward
			break
		}
	}
	m.mu.Unlock()
	if dst != nil {
		dst.handleFrame(f)
	}
}
func (m *controllerMappings) close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	all := m.active
	m.active = map[string]activeForward{}
	m.mu.Unlock()
	for _, a := range all {
		closeActiveForward(a)
	}
	m.workers.Wait()
}
func (m *controllerMappings) start(source func() ([]forwardRule, error), report func(map[string]string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.workers.Add(1)
	go func() { defer m.workers.Done(); m.run(source, report) }()
}
func (m *controllerMappings) run(source func() ([]forwardRule, error), report func(map[string]string)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for m.ctx.Err() == nil {
		rules, e := source()
		if e == nil {
			states := m.reconcile(rules)
			if report != nil {
				report(states)
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
