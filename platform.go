//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const openRCPath = "/etc/init.d/uulink"
const agentLogPath = "/var/log/uulink/agent.log"
const unifiedMarkerPath = "/etc/uulink/unified"
const controllerOpenRCPath = "/etc/init.d/uulink-controller"
const controllerUnitPath = "/etc/systemd/system/uulink-controller.service"

type serviceManager string

const (
	systemd serviceManager = "systemd"
	openRC  serviceManager = "openrc"
)

func detectServiceManager() (serviceManager, error) {
	// Prefer the running init system over tools left behind by installed packages.
	if _, e := os.Stat("/run/systemd/system"); e == nil {
		return systemd, nil
	}
	if _, e := exec.LookPath("openrc-run"); e == nil {
		if _, e = exec.LookPath("rc-service"); e == nil {
			return openRC, nil
		}
	}
	if _, e := exec.LookPath("systemctl"); e == nil {
		return systemd, nil
	}
	return "", fmt.Errorf("需要 systemd 或 OpenRC；Alpine 请先运行 apk add --no-cache openrc")
}
func isAlpine() bool { _, e := os.Stat("/etc/alpine-release"); return e == nil }
func (s serviceManager) path() string {
	if s == openRC {
		return openRCPath
	}
	return unitPath
}
func (s serviceManager) control(action string) error {
	if s == openRC {
		return command("rc-service", "uulink", action)
	}
	args := []string{action, "uulink.service"}
	if action == "status" {
		args = append(args, "--no-pager")
	}
	return command("systemctl", args...)
}
func (s serviceManager) enable() error {
	if s == openRC {
		return command("rc-update", "add", "uulink", "default")
	}
	if e := command("systemctl", "daemon-reload"); e != nil {
		return e
	}
	return command("systemctl", "enable", "uulink.service")
}
func (s serviceManager) disable() error {
	if s == openRC {
		return command("rc-update", "del", "uulink", "default")
	}
	return command("systemctl", "disable", "uulink.service")
}
func (s serviceManager) writeService() error {
	mode := os.FileMode(0644)
	content := serviceUnit
	if s == openRC {
		mode = 0755
		content = openRCService
	}
	return os.WriteFile(s.path(), []byte(content), mode)
}

// writeUnifiedService installs the unified client under the same service name
// (`uulink`) so status/logs/doctor and existing firewall assumptions keep
// working. ExecStart now runs client-daemon, which supervises both roles.
func (s serviceManager) writeUnifiedService() error {
	mode := os.FileMode(0644)
	content := unifiedServiceUnit
	if s == openRC {
		mode = 0755
		content = unifiedOpenRCService
	}
	return os.WriteFile(s.path(), []byte(content), mode)
}

// serviceState captures the independent existence, enable, active and mask
// flags so a failed upgrade can restore the exact previous combination (for
// example active but disabled) and never mistake a service manager failure for
// a stopped service. exists is false for a unit that was never installed;
// masked units are refused by the installer rather than silently rewritten.
// active is true for active/activating/reloading (a stop must wait it out);
// ready is true only for a fully "active" unit, which is what a start check
// must require so a unit stuck in "activating" is not reported as started.
type serviceState struct {
	exists  bool
	enabled bool
	active  bool
	ready   bool
	masked  bool
}

// checkAvailable confirms the service manager itself answers. Without this a
// bus failure (dbus down, rc-update missing) would look like "unit inactive"
// and the installer could rewrite units on a host whose init system is broken.
func (s serviceManager) checkAvailable() error {
	if s == openRC {
		if r := commandQuiet("rc-update", "show", "default"); !r.ok() {
			return fmt.Errorf("rc-update 不可用：%v", r.err)
		}
		return nil
	}
	// `systemctl show` with only manager properties still requires a working
	// manager connection; a bus error makes it exit non-zero.
	if r := commandQuiet("systemctl", "show", "--property=Version"); !r.ok() {
		return fmt.Errorf("systemctl 不可用：%v", r.err)
	}
	return nil
}

// rcUpdateHas matches the service name exactly against the first field of each
// `rc-update show` line. A substring match would confuse `uulink` with
// `uulink-controller`, so a host running only the controller would be reported
// as having the unified service enabled (and vice versa).
func rcUpdateHas(showOutput, name string) bool {
	for _, line := range strings.Split(showOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func parseSystemdProps(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return props
}

func (s serviceManager) state() (serviceState, error) { return s.unitState("uulink") }

func (s serviceManager) controllerState() (serviceState, error) {
	return s.unitState("uulink-controller")
}

func (s serviceManager) unitState(name string) (serviceState, error) {
	if s == openRC {
		return s.openRCState(name)
	}
	return s.systemdState(name + ".service")
}

// systemdState reads LoadState/UnitFileState/ActiveState from one `systemctl
// show`. A non-zero exit means the manager is unreachable, which is an error,
// not "inactive"; a unit that does not exist reports LoadState=not-found with
// exit 0, which is a normal absent state.
//
// Masked units report LoadState=masked (with UnitFileState=masked), so the
// mask flag must be read before the existence test: treating "not loaded" as
// absent would let the installer rewrite a unit an operator deliberately
// masked. Only not-found is a real absence; any other unrecognized or empty
// LoadState means the probe did not answer as expected and must fail loud
// rather than be silently downgraded to "no unit".
func (s serviceManager) systemdState(unit string) (serviceState, error) {
	var st serviceState
	r := commandQuiet("systemctl", "show", "--property=LoadState", "--property=UnitFileState", "--property=ActiveState", unit)
	if !r.ok() {
		return st, fmt.Errorf("systemctl show %s 失败：%v", unit, r.err)
	}
	props := parseSystemdProps(r.output)
	load := props["LoadState"]
	fileState := props["UnitFileState"]
	if load == "masked" || fileState == "masked" || fileState == "masked-runtime" {
		st.exists = true
		st.masked = true
		return st, nil
	}
	switch load {
	case "not-found":
		return st, nil
	case "loaded":
		st.exists = true
	default:
		return st, fmt.Errorf("无法识别 %s 的 LoadState=%q", unit, load)
	}
	switch fileState {
	case "enabled", "enabled-runtime":
		st.enabled = true
	}
	switch props["ActiveState"] {
	case "active":
		st.active = true
		st.ready = true
	case "activating", "reloading":
		st.active = true
	}
	return st, nil
}

// openRCState treats a failed `rc-update show` as a manager error. `rc-service
// status` reports started with exit 0; anything else is treated as inactive,
// which is safe because the subsequent stop/enable calls are idempotent.
func (s serviceManager) openRCState(name string) (serviceState, error) {
	var st serviceState
	if _, err := os.Stat("/etc/init.d/" + name); err == nil {
		st.exists = true
	}
	r := commandQuiet("rc-update", "show", "default")
	if !r.ok() {
		return st, fmt.Errorf("rc-update show 失败：%v", r.err)
	}
	st.enabled = rcUpdateHas(r.output, name)
	if st.exists && commandQuiet("rc-service", name, "status").ok() {
		st.active = true
		st.ready = true
	}
	return st, nil
}

func (s serviceManager) controllerControl(action string) error {
	if s == openRC {
		return command("rc-service", "uulink-controller", action)
	}
	return command("systemctl", action, "uulink-controller.service")
}

func (s serviceManager) controllerDisable() error {
	if s == openRC {
		return command("rc-update", "del", "uulink-controller", "default")
	}
	return command("systemctl", "disable", "uulink-controller.service")
}

func (s serviceManager) controllerPath() string {
	if s == openRC {
		return controllerOpenRCPath
	}
	return controllerUnitPath
}

// commandQuiet runs a command capturing combined output for state probes.
func commandQuiet(name string, args ...string) commandResult {
	c := exec.Command(name, args...)
	out, err := c.CombinedOutput()
	return commandResult{output: string(out), err: err}
}

type commandResult struct {
	output string
	err    error
}

func (r commandResult) ok() bool { return r.err == nil }

const openRCService = `#!/sbin/openrc-run
description="UU Remote Linux agent"
supervisor="supervise-daemon"
command="/usr/local/lib/uulink/uulink"
command_args="serve --config /etc/uulink/config.json --log-file /var/log/uulink/agent.log"
directory="/var/lib/uulink"
pidfile="/run/uulink.pid"
respawn_delay=5
respawn_max=0
stopsig=TERM
retry="TERM/20/KILL/5"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath --directory --mode 0700 /var/lib/uulink /var/log/uulink
    umask 077
}
`

// unifiedServiceUnit runs one supervisor for both roles. KillMode=control-group
// ensures the serve child and the controller-daemon's worker tree exit with the
// service instead of being orphaned. TimeoutStopSec leaves room for the
// supervisor's 5s graceful stop plus its 2s SIGKILL grace.
const unifiedServiceUnit = `[Unit]
Description=UU Remote unified client (controlled + controller)
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=/var/lib/uulink
ExecStart=/usr/local/lib/uulink/uulink client-daemon --config /etc/uulink/config.json --controller-config /etc/uulink-controller/config.json --rules /etc/uulink-controller/rules.json --state-dir /run/uulink-client --controller-state-dir /run/uulink-controller --log-file /var/log/uulink-client/client.log
Restart=always
RestartSec=5
TimeoutStopSec=25
KillMode=control-group
NoNewPrivileges=true
UMask=0077
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

const unifiedOpenRCService = `#!/sbin/openrc-run
description="UU Remote unified client (controlled + controller)"
supervisor="supervise-daemon"
command="/usr/local/lib/uulink/uulink"
command_args="client-daemon --config /etc/uulink/config.json --controller-config /etc/uulink-controller/config.json --rules /etc/uulink-controller/rules.json --state-dir /run/uulink-client --controller-state-dir /run/uulink-controller --log-file /var/log/uulink-client/client.log"
directory="/var/lib/uulink"
pidfile="/run/uulink-client/client.pid"
respawn_delay=5
respawn_max=0
stopsig=TERM
retry="TERM/20/KILL/5"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath --directory --mode 0700 /var/lib/uulink /var/log/uulink /run/uulink-client /var/log/uulink-client
    umask 077
}
`

func requiredCommands(cfg agentConfig, manager serviceManager) []string {
	names := []string{}
	if cfg.features().terminal {
		names = append(names, "sh")
	}
	if manager == openRC {
		names = append(names, "openrc-run", "rc-service", "rc-update", "supervise-daemon")
	} else {
		names = append(names, "systemctl")
	}
	if cfg.AllowPower {
		if manager == openRC {
			names = append(names, "sh", "sleep", "reboot", "poweroff")
		} else {
			names = append(names, "systemd-run")
		}
	}
	if cfg.features().desktop {
		names = append(names, "ffmpeg", "xdpyinfo", "xdotool", "xauth")
	}
	return names
}
func missingDependencies(cfg agentConfig, manager serviceManager) []string {
	var missing []string
	for _, name := range requiredCommands(cfg, manager) {
		if _, e := exec.LookPath(name); e != nil {
			missing = append(missing, name)
		}
	}
	if !hasCACertificates() {
		missing = append(missing, "ca-certificates")
	}
	return missing
}
func hasCACertificates() bool {
	for _, path := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt"} {
		if info, e := os.Stat(path); e == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}
func dependencyPackages(cfg agentConfig, alpine bool) []string {
	packages := []string{"ca-certificates"}
	if cfg.features().desktop {
		x11 := "x11-utils"
		if alpine {
			x11 = "xdpyinfo"
		}
		packages = append(packages, "ffmpeg", x11, "xdotool", "xauth")
	}
	return packages
}
func ensureDependencies(cfg agentConfig, manager serviceManager, skip bool) error {
	missing := missingDependencies(cfg, manager)
	if len(missing) == 0 {
		return nil
	}
	if skip {
		return fmt.Errorf("所选组件缺少依赖：%s", strings.Join(missing, ", "))
	}
	var name string
	var steps [][]string
	if isAlpine() {
		name = "apk"
		steps = [][]string{append([]string{"add", "--no-cache"}, dependencyPackages(cfg, true)...)}
	} else if _, e := exec.LookPath("apt-get"); e == nil {
		name = "apt-get"
		steps = [][]string{{"update"}, append([]string{"install", "-y", "--no-install-recommends"}, dependencyPackages(cfg, false)...)}
	} else {
		return fmt.Errorf("请手动安装所选组件依赖：%s（自动安装支持 Alpine、Debian、Ubuntu）", strings.Join(missing, ", "))
	}
	fmt.Printf("将通过 %s 安装：%s\n", name, strings.Join(dependencyPackages(cfg, isAlpine()), ", "))
	for _, args := range steps {
		c := exec.Command(name, args...)
		c.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if e := c.Run(); e != nil {
			return fmt.Errorf("安装依赖失败：%w", e)
		}
	}
	if missing = missingDependencies(cfg, manager); len(missing) > 0 {
		return fmt.Errorf("仍缺少依赖：%v", missing)
	}
	return nil
}
