//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// A unified client runs both UU roles on one machine: the controlled side
// (serve) and the controller side (controller-daemon). The two roles keep
// separate credentials and separate child processes, so runtimeConfig and
// linuxBearer are only ever mutated inside a child, never concurrently here.
const clientStateDir = "/run/uulink-client"
const clientLogPath = "/var/log/uulink-client/client.log"

// agentStateDir is the canonical lock directory for the controlled role.
// The serve child holds its per-DeviceID identity lock here, so a legacy
// `uulink serve` and the unified client cannot run the same identity twice.
const agentStateDir = "/run/uulink"

const (
	clientRoleControlled = "controlled"
	clientRoleController = "controller"
)

type clientOptions struct {
	config          string
	controller      string
	rules           string
	state           string
	controllerState string
	log             string
}

func parseClientOptions(args []string) (clientOptions, error) {
	var o clientOptions
	f := flag.NewFlagSet("client-daemon", flag.ContinueOnError)
	f.StringVar(&o.config, "config", configDefault, "controlled-side configuration file")
	f.StringVar(&o.controller, "controller-config", controllerConfigPath, "controller-side configuration file")
	f.StringVar(&o.rules, "rules", controllerRulesPath, "controller forwarding rules file")
	f.StringVar(&o.state, "state-dir", clientStateDir, "unified client runtime status directory")
	f.StringVar(&o.controllerState, "controller-state-dir", controllerStateDir, "controller daemon status and lock directory")
	f.StringVar(&o.log, "log-file", "", "bounded log file (1 MiB x 2)")
	err := f.Parse(args)
	return o, err
}

// clientRoleStatus reports one role's process state. Connected stays
// "未确认": a live child process is never proof that UU is reachable.
type clientRoleStatus struct {
	Process   string `json:"process"`
	Connected string `json:"connected"`
	PID       int    `json:"pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type clientStatus struct {
	Updated    time.Time                   `json:"updated"`
	Supervisor string                      `json:"supervisor"`
	Error      string                      `json:"error,omitempty"`
	Roles      map[string]clientRoleStatus `json:"roles"`
}

// clientDaemonCLI supervises both roles under a single instance lock. Each
// role is a separate child process group so a crash or a stop in one role
// never tears down the other, and stopping the parent cannot leave the
// controller daemon's workers behind.
func clientDaemonCLI(args []string) error {
	o, err := parseClientOptions(args)
	if err != nil {
		return err
	}
	if o.log != "" {
		w, err := openRotatingLog(o.log)
		if err != nil {
			return err
		}
		defer w.Close()
		log.SetOutput(w)
	}
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	lock, err := controllerLock(filepath.Join(o.state, "client.lock"), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	// The supervisor only coordinates children, so keep it as small as the
	// controller daemon.
	configureResources(agentConfig{LowMemory: true})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runClientSupervisor(ctx, o)
}

type clientChild struct {
	name           string
	cmd            *exec.Cmd
	pgid           int
	done           chan error
	running        bool
	stopping       bool
	started        time.Time
	nextStart      time.Time
	backoff        time.Duration
	deadline       time.Time
	identity       string
	unhealthySince time.Time
	status         clientRoleStatus
}

// clientStopGrace lets a transient config read (for example an atomic login
// rewrite) recover before a healthy role is torn down. A persistent change
// still stops the role once the grace window passes.
const clientStopGrace = 3 * time.Second

type clientSupervisor struct {
	exe      string
	opts     clientOptions
	children map[string]*clientChild
}

func runClientSupervisor(ctx context.Context, o clientOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	s := &clientSupervisor{exe: exe, opts: o, children: map[string]*clientChild{}}
	if o.state != clientStateDir {
		log.Printf("client warning: 非默认 state-dir=%s 仅用于隔离测试，不保护默认 /run/uulink 身份", o.state)
	}
	for _, name := range []string{clientRoleControlled, clientRoleController} {
		s.children[name] = &clientChild{name: name, status: clientRoleStatus{Process: "未运行", Connected: "未确认"}}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.tick()
		select {
		case <-ctx.Done():
			// Clear both process groups first so the final status cannot
			// claim a role is still running.
			s.stopAll()
			s.writeStatus("已停止", "")
			return nil
		case <-ticker.C:
		}
	}
}

func (s *clientSupervisor) tick() {
	controlled := loadClientRole(s.opts.config)
	controller := loadClientRole(s.opts.controller)

	conflict := ""
	if controlled.ready() && controller.ready() {
		if controlled.cfg.DeviceID == controller.cfg.DeviceID || controlled.cfg.ClientID == controller.cfg.ClientID {
			conflict = "控制端配置与被控端使用相同设备身份，已拒绝控制端"
		}
	}

	for name, c := range s.children {
		s.reap(c)
		role := controlled
		roleConflict := ""
		if name == clientRoleController {
			role = controller
			roleConflict = conflict
		}
		want := role.ready() && roleConflict == ""
		// A token refresh rewrites the config atomically and keeps the same
		// identity, so it must not disturb a healthy child. A changed
		// DeviceID/ClientID/HostID means the running child holds stale
		// credentials, so that role is restarted with the new identity.
		restart := c.running && !c.stopping && role.ready() && role.identity() != c.identity
		if want || restart {
			c.unhealthySince = time.Time{}
		} else if c.unhealthySince.IsZero() {
			c.unhealthySince = time.Now()
		}
		switch {
		case c.stopping:
			// SIGTERM was already delivered, so the stop cannot be undone; wait
			// for reap and only escalate to SIGKILL after the deadline.
		case restart:
			// Ordering matters for the controlled role: the old child holds the
			// lock for the previous DeviceID, and a new-identity serve would use
			// a different lock name. Stopping first and starting only after reap
			// (a later tick) keeps the old publisher from overlapping the new one.
			log.Printf("client %s identity changed; restarting", c.name)
			s.stop(c)
		case !want:
			if roleConflict != "" {
				c.status.Process = "配置无效"
				c.status.Detail = roleConflict
			} else {
				c.status.Process = role.state
				c.status.Detail = role.detail
			}
			if c.running && time.Since(c.unhealthySince) >= clientStopGrace {
				s.stop(c)
			}
		case c.running:
			if c.status.Process != "运行中" {
				c.status.Process = "运行中"
				c.status.Detail = ""
			}
		case !time.Now().Before(c.nextStart):
			s.start(c, role)
		}
		if c.stopping && c.running && time.Now().After(c.deadline) {
			signalClientGroup(c.pgid, syscall.SIGKILL)
		}
	}
	s.writeStatus("运行中", conflict)
}

func (s *clientSupervisor) start(c *clientChild, role clientRole) {
	var args []string
	if c.name == clientRoleControlled {
		args = []string{"serve", "--config", s.opts.config, "--state-dir", s.controlledStateDir()}
	} else {
		args = []string{"controller-daemon", "--config", s.opts.controller, "--rules", s.opts.rules, "--state-dir", s.opts.controllerState}
	}
	cmd := exec.Command(s.exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		c.status.Process = "启动失败"
		c.status.Detail = err.Error()
		s.scheduleRetry(c)
		return
	}
	c.cmd = cmd
	c.pgid = cmd.Process.Pid
	c.done = make(chan error, 1)
	c.running = true
	c.stopping = false
	c.started = time.Now()
	c.identity = role.identity()
	c.status = clientRoleStatus{Process: "运行中", Connected: "未确认", PID: cmd.Process.Pid}
	go func() { c.done <- cmd.Wait() }()
	log.Printf("client %s started pid=%d", c.name, cmd.Process.Pid)
}

func (s *clientSupervisor) reap(c *clientChild) {
	if !c.running {
		return
	}
	select {
	case err := <-c.done:
		// The leader exited; any grandchild (for example a controller
		// worker) may still be alive in the group and holding ports or the
		// identity. Clear the group we own before dropping its PGID.
		clearClientGroup(c.pgid)
		c.running = false
		c.cmd = nil
		c.pgid = 0
		c.status.PID = 0
		if c.stopping {
			c.stopping = false
			c.status.Process = "未运行"
			c.status.Detail = "已停止"
			return
		}
		detail := "进程退出"
		if err != nil {
			detail = err.Error()
		}
		log.Printf("client %s exited: %v", c.name, err)
		c.status.Process = "重启退避"
		c.status.Detail = detail
		s.scheduleRetry(c)
	default:
	}
}

func (s *clientSupervisor) scheduleRetry(c *clientChild) {
	if time.Since(c.started) > time.Minute {
		c.backoff = 0
	}
	if c.backoff == 0 {
		c.backoff = 2 * time.Second
	} else if c.backoff < 30*time.Second {
		c.backoff *= 2
		if c.backoff > 30*time.Second {
			c.backoff = 30 * time.Second
		}
	}
	c.nextStart = time.Now().Add(c.backoff)
}

func (s *clientSupervisor) stop(c *clientChild) {
	c.stopping = true
	c.status.Process = "正在停止"
	c.status.Detail = "正在停止"
	c.deadline = time.Now().Add(5 * time.Second)
	signalClientGroup(c.pgid, syscall.SIGTERM)
}

func (s *clientSupervisor) stopAll() {
	deadline := time.Now().Add(5 * time.Second)
	for _, c := range s.children {
		if c.running {
			c.stopping = true
			signalClientGroup(c.pgid, syscall.SIGTERM)
		}
	}
	for _, c := range s.children {
		if !c.running {
			continue
		}
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case <-c.done:
		case <-time.After(remaining):
			// A group member that ignores TERM (or an orphaned grandchild)
			// must not outlive the supervisor.
			signalClientGroup(c.pgid, syscall.SIGKILL)
			select {
			case <-c.done:
			case <-time.After(2 * time.Second):
			}
		}
		clearClientGroup(c.pgid)
		c.running = false
		c.pgid = 0
		c.cmd = nil
		c.status.PID = 0
		c.status.Process = "未运行"
		c.status.Detail = "已停止"
	}
}

// signalClientGroup targets the whole child process group so
// controller-daemon's workers are not orphaned. Children always start with
// Setpgid, so the recorded pgid equals the leader's pid.
func signalClientGroup(pgid int, sig syscall.Signal) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

// clearClientGroup force-kills any surviving members of a group we still own.
// It is only called for a pgid recorded at start and after that leader has
// been reaped or killed, so it cannot hit an unrelated reused group.
func clearClientGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func (s *clientSupervisor) writeStatus(supervisor, errText string) {
	st := clientStatus{Updated: time.Now(), Supervisor: supervisor, Error: errText, Roles: map[string]clientRoleStatus{}}
	for name, c := range s.children {
		st.Roles[name] = c.status
	}
	if err := atomicControllerJSON(filepath.Join(s.opts.state, "client.json"), st); err != nil {
		log.Printf("client status: %v", err)
	}
}

// controlledStateDir keeps the controlled identity lock canonical by default.
// A non-default parent state dir derives its own lock directory, which is an
// explicit isolation for tests only: it does not guard against a legacy
// `uulink serve` running against the default /run/uulink identity.
func (s *clientSupervisor) controlledStateDir() string {
	if s.opts.state == clientStateDir {
		return agentStateDir
	}
	return filepath.Join(s.opts.state, "controlled")
}

// agentIdentityLock serializes one controlled-side identity so a legacy
// `uulink serve` and the unified client cannot run the same device at once.
// The lock name is the configured DeviceID; before login there is none, so the
// config path is hashed instead and concurrent pre-login serves still exclude
// each other. It is non-blocking: a second holder reports the conflict.
//
// The caller may pin the identity it already read. Without pinning the helper
// re-reads the config, so a config rewrite between the caller's read and the
// lock (DeviceID1 -> DeviceID2 -> DeviceID1) could lock one identity while the
// caller serves another. A pinned, non-empty DeviceID is used verbatim and the
// config is not re-read; a pinned empty DeviceID hashes the config path.
func agentIdentityLock(stateDir, configPath string, pinnedDeviceID ...string) (*os.File, error) {
	key := ""
	if len(pinnedDeviceID) > 0 {
		key = pinnedDeviceID[0]
	} else {
		cfg, _ := readConfig(configPath)
		key = cfg.DeviceID
	}
	if key == "" {
		key = "path:" + configPath
	}
	sum := sha256.Sum256([]byte(key))
	return controllerLock(filepath.Join(stateDir, hex.EncodeToString(sum[:16])+".lock"), true)
}

// clientRole describes whether one role's configuration is usable. state is
// empty only when the role may run.
type clientRole struct {
	cfg    agentConfig
	state  string
	detail string
}

func (r clientRole) ready() bool { return r.state == "" }

// identity is the credential binding a running child depends on. A change
// here (but not a token/bearer refresh) means the child must be restarted.
func (r clientRole) identity() string {
	return r.cfg.DeviceID + "\x00" + r.cfg.ClientID + "\x00" + r.cfg.HostID
}

// loadClientRole reads and validates one config without touching the shared
// runtimeConfig/linuxBearer globals; those are only set inside child processes.
func loadClientRole(path string) clientRole {
	cfg, err := readConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			return clientRole{state: "等待登录", detail: "配置文件不存在：" + path}
		}
		return clientRole{state: "配置无效", detail: err.Error()}
	}
	if cfg.DeviceID == "" || cfg.ClientID == "" || cfg.UserID == "" || cfg.Bearer == "" {
		return clientRole{cfg: cfg, state: "等待登录", detail: "设备尚未完成登录：" + path}
	}
	if _, err := parseComponents(cfg.Components); err != nil {
		return clientRole{cfg: cfg, state: "配置无效", detail: err.Error()}
	}
	if cfg.HostID != "" {
		if local := machineID(); local != "" && cfg.HostID != local {
			return clientRole{cfg: cfg, state: "配置无效", detail: "配置属于另一台机器：" + path}
		}
	}
	return clientRole{cfg: cfg}
}
