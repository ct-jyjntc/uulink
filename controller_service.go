package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type controllerOptions struct {
	config, rules, state, log, device string
	probeOnly                         bool
}

func parseControllerOptions(mode string, args []string) (controllerOptions, error) {
	var o controllerOptions
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	f.StringVar(&o.config, "config", controllerConfigPath, "controller login configuration")
	f.StringVar(&o.rules, "rules", controllerRulesPath, "persistent rules file")
	f.StringVar(&o.state, "state-dir", controllerStateDir, "runtime status directory")
	f.StringVar(&o.log, "log-file", "", "bounded log file (1 MiB x 2)")
	f.StringVar(&o.device, "device", "", "worker device")
	f.BoolVar(&o.probeOnly, "probe-only", false, "measure peer latency without starting port listeners")
	err := f.Parse(args)
	return o, err
}

type controllerStatus struct {
	Updated time.Time         `json:"updated"`
	State   string            `json:"state"`
	Error   string            `json:"error,omitempty"`
	Rules   map[string]string `json:"rules,omitempty"`
}
type statusReporter struct {
	mu     sync.Mutex
	status controllerStatus
	path   string
}

func (r *statusReporter) set(state, err string, rules map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = controllerStatus{State: state, Error: err, Rules: rules}
}
func (r *statusReporter) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Updated = time.Now()
	if e := atomicControllerJSON(r.path, r.status); e != nil {
		log.Printf("controller status: %v", e)
	}
}
func (r *statusReporter) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		r.flush()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func controllerWorkerCLI(args []string) error {
	o, e := parseControllerOptions("controller-worker", args)
	if e != nil {
		return e
	}
	if !safeControllerID.MatchString(o.device) {
		return fmt.Errorf("valid --device required")
	}
	lock, e := controllerLock(filepath.Join(o.state, o.device+".lock"), true)
	if e != nil {
		return e
	}
	defer lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.SetPrefix("[" + o.device + "] ")
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	report := &statusReporter{path: filepath.Join(o.state, o.device+".json")}
	report.set("正在连接", "", nil)
	reportDone := make(chan struct{})
	go func() { defer close(reportDone); report.run(ctx) }()
	defer func() { stop(); <-reportDone; report.set("已停止", "", nil); report.flush() }()
	configured := false
	source := func() ([]forwardRule, error) {
		if o.probeOnly {
			return []forwardRule{}, nil
		}
		return enabledDeviceRules(o.rules, o.device)
	}
	for ctx.Err() == nil {
		report.set("正在连接", "", nil)
		e = loadAgentConfig(o.config)
		if e == nil {
			if !configured {
				configureResources(runtimeConfig)
				configured = true
			}
			e = runControllerRules(ctx, o.device, true, source, func(states map[string]string) { report.set("已连接", "", states) }, report.latency)
		}
		if ctx.Err() != nil {
			return nil
		}
		detail := "连接结束"
		if e != nil {
			detail = e.Error()
		}
		report.set("等待重连", detail, nil)
		log.Printf("session ended: %s; retry in 5s", detail)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

type controllerProcess struct {
	cmd      *exec.Cmd
	done     chan error
	stopping bool
	deadline time.Time
}

func controllerDaemonCLI(args []string) error {
	o, e := parseControllerOptions("controller-daemon", args)
	if e != nil {
		return e
	}
	lock, e := controllerLock(filepath.Join(o.state, "daemon.lock"), true)
	if e != nil {
		return e
	}
	defer lock.Close()
	if o.log != "" {
		w, e := openRotatingLog(o.log)
		if e != nil {
			return e
		}
		defer w.Close()
		log.SetOutput(w)
	}
	configureResources(agentConfig{LowMemory: true})
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	// Create an empty document only at first setup; malformed files are never overwritten.
	if _, e = os.Stat(o.rules); os.IsNotExist(e) {
		if e = updateRules(o.rules, func(*ruleDocument) error { return nil }); e != nil {
			return e
		}
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	children := map[string]*controllerProcess{}
	stopChild := func(p *controllerProcess) {
		if !p.stopping {
			p.stopping = true
			p.deadline = time.Now().Add(5 * time.Second)
			requestControllerStop(p.cmd)
		}
	}
	defer func() {
		for _, p := range children {
			stopChild(p)
		}
		for _, p := range children {
			timer := time.NewTimer(time.Until(p.deadline))
			select {
			case <-p.done:
			case <-timer.C:
				_ = p.cmd.Process.Kill()
				<-p.done
			}
			timer.Stop()
		}
		_ = atomicControllerJSON(filepath.Join(o.state, "daemon.json"), controllerStatus{Updated: time.Now(), State: "已停止"})
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	wanted := map[string]bool{}
	for ctx.Err() == nil {
		doc, readErr := readRules(o.rules)
		if readErr == nil {
			wanted = map[string]bool{}
			for _, r := range doc.Rules {
				if r.Enabled && !o.probeOnly {
					wanted[r.Device] = true
				}
			}
		}
		if o.probeOnly {
			wanted = map[string]bool{}
		}
		for device := range activeProbeRequests(filepath.Join(o.state, "probes.json")) {
			wanted[device] = true
		}
		if o.probeOnly && len(wanted) == 0 {
			return nil
		}
		for device, p := range children {
			select {
			case err := <-p.done:
				log.Printf("worker %s exited: %v", device, err)
				delete(children, device)
				_ = os.Remove(filepath.Join(o.state, device+".json"))
				continue
			default:
			}
			if !wanted[device] {
				stopChild(p)
			}
			if p.stopping && time.Now().After(p.deadline) {
				_ = p.cmd.Process.Kill()
			}
		}
		for device := range wanted {
			if children[device] != nil {
				continue
			}
			cmd := exec.Command(exe, "controller-worker", "--config", o.config, "--rules", o.rules, "--state-dir", o.state, "--device", device)
			if o.probeOnly {
				cmd.Args = append(cmd.Args, "--probe-only")
			}
			configureControllerChild(cmd)
			cmd.Stdout = log.Writer()
			cmd.Stderr = log.Writer()
			cmd.WaitDelay = 5 * time.Second
			if e = cmd.Start(); e != nil {
				log.Printf("start worker %s: %v", device, e)
				continue
			}
			p := &controllerProcess{cmd: cmd, done: make(chan error, 1)}
			children[device] = p
			go func() { p.done <- cmd.Wait() }()
			log.Printf("started worker %s pid=%d", device, cmd.Process.Pid)
		}
		state := controllerStatus{Updated: time.Now(), State: fmt.Sprintf("运行中，%d 台被控端", len(children))}
		if readErr != nil {
			state.Error = "规则读取失败，保留上次配置: " + readErr.Error()
		}
		if e = atomicControllerJSON(filepath.Join(o.state, "daemon.json"), state); e != nil {
			log.Printf("daemon status: %v", e)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}
