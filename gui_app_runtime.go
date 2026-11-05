package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type guiConnectionConfig struct {
	Version       int    `json:"version"`
	Mode          string `json:"mode,omitempty"`
	Device        string `json:"device,omitempty"`
	Listen        string `json:"listen,omitempty"`
	AllowClient   string `json:"allow_client,omitempty"`
	Enabled       bool   `json:"enabled,omitempty"`
	ConnectOnOpen bool   `json:"connect_on_open"`
}

func (g *guiServer) connectionPath() string {
	return filepath.Join(g.opts.sessionDir, "connection.json")
}
func (g *guiServer) connectionConfig() (guiConnectionConfig, error) {
	c := guiConnectionConfig{Version: 2, Mode: "ports", Enabled: true, ConnectOnOpen: true}
	b, e := os.ReadFile(g.connectionPath())
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, fmt.Errorf("连接配置损坏，未覆盖")
	}
	if (c.Version != 1 && c.Version != 2) || (c.Version == 1 && c.Mode != "tcp" && c.Mode != "game") {
		return c, fmt.Errorf("连接配置版本或类型无效")
	}
	return c, nil
}

type guiAppRuntime struct {
	mu         sync.Mutex
	ctx        context.Context
	cmd        *exec.Cmd
	engineStop func(bool)
	done       chan struct{}
	wanted     bool
	started    time.Time
	lastError  string
	probeOnly  bool
}

func (g *guiServer) startConnection() error {
	g.connection.mu.Lock()
	probeOnly := g.connection.cmd != nil && g.connection.probeOnly
	g.connection.mu.Unlock()
	if probeOnly {
		if e := g.stopConnection(); e != nil {
			return e
		}
	}
	return g.startConnectionMode(false)
}
func (g *guiServer) startLatencyProbe() error { return g.startConnectionMode(true) }
func (g *guiServer) startConnectionMode(probeOnly bool) error {
	r := g.connection
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return fmt.Errorf("应用正在退出")
	}
	if r.cmd != nil {
		if !r.probeOnly {
			r.wanted = true
		}
		return nil
	}
	_, e := g.connectionConfig()
	if e != nil {
		return e
	}
	doc, e := readRules(g.opts.rules)
	if e != nil {
		return e
	}
	enabled := false
	for _, rule := range doc.Rules {
		enabled = enabled || rule.Enabled
	}
	if !enabled && !probeOnly {
		return fmt.Errorf("请先添加并启用映射")
	}
	identity, e := readConfig(g.opts.controller)
	if e != nil || identity.Bearer == "" || identity.DeviceID == "" || identity.UserID == "" || identity.ClientID == "" {
		return fmt.Errorf("请先登录账户")
	}
	mid, e := g.machineID()
	if e != nil {
		return e
	}
	if identity.HostID != "" && identity.HostID != mid {
		return fmt.Errorf("账户身份属于其他机器")
	}
	self, e := os.Executable()
	if e != nil {
		return e
	}
	binary := filepath.Join(filepath.Dir(self), appEngineName)
	if _, e = os.Stat(binary); e != nil {
		return fmt.Errorf("连接引擎未找到，请重新解压或安装完整的 UU Link")
	}
	args := []string{"app-run", strconv.Itoa(os.Getpid()), "controller-daemon", "--config", g.opts.controller, "--rules", g.opts.rules, "--state-dir", g.opts.controllerState, "--log-file", g.opts.gatewayLog}
	if probeOnly {
		args = append(args, "--probe-only")
	}
	if e = os.MkdirAll(filepath.Dir(g.opts.gatewayLog), 0700); e != nil {
		return e
	}
	output, e := openRotatingLog(filepath.Join(filepath.Dir(g.opts.gatewayLog), "app-engine.log"))
	if e != nil {
		return e
	}
	cmd := exec.Command(binary, args...)
	cmd.WaitDelay = time.Second
	cmd.Stdout = output
	cmd.Stderr = output
	stopEngine, e := startAppEngine(cmd)
	if e != nil {
		output.Close()
		r.lastError = e.Error()
		return e
	}
	r.cmd = cmd
	r.engineStop = stopEngine
	r.done = make(chan struct{})
	r.wanted = !probeOnly
	r.probeOnly = probeOnly
	r.started = time.Now().UTC()
	r.lastError = ""
	done := r.done
	go func() {
		err := cmd.Wait()
		// The leader may have crashed while TCP workers still own the group's
		// pipes. Finish the entire old group before publishing it as stopped.
		stopEngine(true)
		output.Close()
		r.mu.Lock()
		r.cmd = nil
		r.engineStop = nil
		retry := r.wanted && r.ctx.Err() == nil
		if err != nil && retry {
			r.lastError = "连接引擎已退出，正在重试"
		}
		close(done)
		r.mu.Unlock()
		if retry {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			// A stop or configuration change during the retry delay cancels it.
			g.opsMu.Lock()
			defer g.opsMu.Unlock()
			r.mu.Lock()
			retry = r.wanted && r.cmd == nil
			r.mu.Unlock()
			if retry {
				if e := g.startConnection(); e != nil {
					r.mu.Lock()
					r.lastError = e.Error()
					r.mu.Unlock()
				}
			}
		}
	}()
	return nil
}
func (g *guiServer) stopConnection() error {
	r := g.connection
	r.mu.Lock()
	r.wanted = false
	stopEngine, done := r.engineStop, r.done
	r.mu.Unlock()
	if stopEngine == nil {
		return nil
	}
	stopEngine(false)
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
	}
	stopEngine(true)
	select {
	case <-done:
		return nil
	case <-time.After(time.Second):
		return fmt.Errorf("连接引擎未能退出")
	}
}
func (g *guiServer) service() guiService {
	c, e := g.connectionConfig()
	s := guiService{Mode: "ports", State: "stopped", Manager: "application", Name: "UU Link", Installed: e == nil, AutoStart: c.ConnectOnOpen}
	if e != nil {
		if os.IsNotExist(e) {
			s.State = "not-installed"
		} else {
			s.Error = e.Error()
		}
		return s
	}
	r := g.connection
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		s.State = "running"
		if r.probeOnly {
			s.State = "measuring"
		}
		s.PID = r.cmd.Process.Pid
	}
	s.Error = r.lastError
	return s
}
func (g *guiServer) control(action string) error {
	switch action {
	case "stop":
		return g.stopConnection()
	case "restart":
		if e := g.stopConnection(); e != nil {
			return e
		}
		return g.startConnection()
	case "start":
		return g.startConnection()
	}
	return fmt.Errorf("未知连接操作")
}
func (g *guiServer) setConnectOnOpen(on bool) error {
	c, e := g.connectionConfig()
	if os.IsNotExist(e) {
		e = nil
		c.Enabled = false
	}
	if e != nil {
		return e
	}
	c.ConnectOnOpen = on
	return atomicControllerJSON(g.connectionPath(), c)
}
