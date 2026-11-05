//go:build !windows

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// The unified client keeps the controlled installer's public entry points
// (the `uulink` symlink, the manifest) so status/doctor/logs and existing
// firewall assumptions are unchanged; only the service payload differs.
const unifiedLinkPath = "/usr/local/bin/uulink"
const unifiedManifestPath = "/usr/local/lib/uulink/manifest.json"
const unifiedBackupDir = "/etc/uulink/unified-backup"

// unifiedInstalled reports the marker written by a successful unified install.
// It is the single source of truth for "this device runs both roles", so the
// CLI, the menu and uninstall all branch on the same file.
func unifiedInstalled() bool {
	_, err := os.Stat(unifiedMarkerPath)
	return err == nil
}

// hash16 shortens a digest for use in backup file names. It is not a security
// boundary: it only keeps per-path backup names distinct and readable.
func hash16(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// syncDir flushes a directory entry so a rename survives a crash. Directory
// fsync is unsupported on some filesystems (EINVAL on darwin); there the
// rename is still atomic, only the extra durability barrier is skipped.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// writeFileDurable replaces path with data through a temp file plus rename,
// then fsyncs the parent directory. A concurrent reader therefore sees either
// the old bytes or the complete new bytes, never a partial write.
func writeFileDurable(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".uulink-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeFileFrom(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileDurable(dst, b, mode)
}

// writeConfigIfChanged persists one role's configuration only when a field
// would actually change. A repeat install must not rewrite an identical config
// (it would churn mtime and, worse, race a running child's atomic re-read);
// when a write is needed it goes through the same durable path as the rest of
// the installer. agentConfig is all scalar fields, so a struct comparison is a
// faithful "same content" test that ignores cosmetic differences (key order,
// unknown legacy keys) a byte comparison would treat as a change.
func writeConfigIfChanged(path string, cfg agentConfig) error {
	old, err := readConfig(path)
	if err == nil && old == cfg {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeConfig(path, cfg)
}

// savedPath records one filesystem entry before the installer mutates it, so a
// failed upgrade can put back the exact previous state: absent, a regular file
// with its bytes and mode, or a symlink with its target. The JSON tags make the
// whole transaction describable in one backup manifest, so an operator can see
// exactly what a failed upgrade restored (or would restore) without reading the
// binary's source.
type savedPath struct {
	Path       string      `json:"path"`
	Backup     string      `json:"backup,omitempty"`
	Existed    bool        `json:"existed"`
	IsLink     bool        `json:"is_link,omitempty"`
	LinkTarget string      `json:"link_target,omitempty"`
	Mode       os.FileMode `json:"mode,omitempty"`
}

// savePath copies path (a regular file or symlink) into dir before mutation.
// dir is unique per transaction so two concurrent installs cannot overwrite
// each other's backups.
func savePath(dir, path string) (*savedPath, error) {
	s := &savedPath{Path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.Existed = true
	s.Mode = info.Mode().Perm()
	if info.Mode()&os.ModeSymlink != 0 {
		s.IsLink = true
		if s.LinkTarget, err = os.Readlink(path); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("无法备份非常规文件：%s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s.Backup = filepath.Join(dir, filepath.Base(path)+"."+hash16([]byte(path))+".bak")
	// writeFileDurable fsyncs both the backup file and the backup directory,
	// so the copy is on disk before any service is stopped.
	if err = writeFileDurable(s.Backup, b, 0600); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *savedPath) restore() error {
	if !s.Existed {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if s.IsLink {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(s.LinkTarget, s.Path)
	}
	// Restore with the recorded mode so an executable (the installed binary,
	// the OpenRC init script) keeps its execute bits; the generic 0600 default
	// would leave a restored unit unreadable by the service manager.
	if err := writeFileFrom(s.Backup, s.Path, s.Mode); err != nil {
		return err
	}
	// Durability: make the restored name survive a crash, matching the
	// directory-sync that atomicControllerJSON already does.
	return syncDir(filepath.Dir(s.Path))
}

// unifiedTx is the two-phase upgrade. Everything that will be touched is saved
// first (unit files, marker, link, manifest) and the previous service states
// are captured before anything is stopped, so rollback can return the host to
// exactly the pre-upgrade layout.
type unifiedTx struct {
	manager     serviceManager
	prevService serviceState
	prevCtrl    serviceState
	backupDir   string
	saved       []*savedPath
	// hadUulink / hadController record whether each service unit existed before
	// the upgrade. Rollback must only restart a unit that was really there; a
	// fresh install must not leave a unit file behind and then start it.
	hadUulink      bool
	hadController  bool
	unitWritten    bool
	markerAdded    bool
	startAttempted bool
}

func newUnifiedTx(manager serviceManager) (*unifiedTx, error) {
	if err := manager.checkAvailable(); err != nil {
		return nil, err
	}
	prevService, err := manager.state()
	if err != nil {
		return nil, err
	}
	prevCtrl, err := manager.controllerState()
	if err != nil {
		return nil, err
	}
	if prevService.masked || prevCtrl.masked {
		return nil, fmt.Errorf("检测到已 mask 的服务单元，拒绝自动迁移；请先手动 unmask 后重试")
	}
	t := &unifiedTx{manager: manager, prevService: prevService, prevCtrl: prevCtrl}
	// A unique per-run directory keeps two installers from clobbering each
	// other's backups and leaves an operator-visible trail of what was saved.
	dir, err := os.MkdirTemp(unifiedBackupDir, "upgrade-")
	if err != nil {
		if err = os.MkdirAll(unifiedBackupDir, 0700); err != nil {
			return nil, err
		}
		if dir, err = os.MkdirTemp(unifiedBackupDir, "upgrade-"); err != nil {
			return nil, err
		}
	}
	t.backupDir = dir
	// "Was it installed?" is a filesystem question: rollback restores only
	// units that really existed, so a fresh install cannot be left enabled and
	// running after a failed upgrade.
	if _, err := os.Lstat(manager.path()); err == nil {
		t.hadUulink = true
	}
	if _, err := os.Lstat(manager.controllerPath()); err == nil {
		t.hadController = true
	}
	// installedBinary is included so a failed upgrade restores the exact
	// previous executable (and its mode), not just the unit files.
	for _, p := range []string{manager.path(), manager.controllerPath(), unifiedMarkerPath, unifiedLinkPath, unifiedManifestPath, installedBinary} {
		s, err := savePath(t.backupDir, p)
		if err != nil {
			return nil, err
		}
		t.saved = append(t.saved, s)
	}
	if err := t.writeBackupManifest(); err != nil {
		return nil, err
	}
	return t, nil
}

// backupManifest is the operator-facing record of one upgrade transaction.
// It lists every saved entry plus the service states captured before the
// upgrade, so a rollback can be audited (or repeated by hand) without the
// installer's internal structs.
type backupManifest struct {
	Version       string        `json:"version"`
	CreatedAt     string        `json:"created_at"`
	Manager       string        `json:"service_manager"`
	HadUulink     bool          `json:"had_uulink"`
	HadController bool          `json:"had_controller"`
	Service       manifestState `json:"uulink_state"`
	Controller    manifestState `json:"controller_state"`
	Entries       []*savedPath  `json:"entries"`
}

// manifestState is the exported, JSON-serializable mirror of serviceState so
// the backup manifest is readable without exporting the internal type.
type manifestState struct {
	Exists  bool `json:"exists"`
	Enabled bool `json:"enabled"`
	Active  bool `json:"active"`
	Masked  bool `json:"masked,omitempty"`
}

func toManifestState(st serviceState) manifestState {
	return manifestState{Exists: st.exists, Enabled: st.enabled, Active: st.active, Masked: st.masked}
}

func (t *unifiedTx) writeBackupManifest() error {
	m := backupManifest{
		Version:       version,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Manager:       string(t.manager),
		HadUulink:     t.hadUulink,
		HadController: t.hadController,
		Service:       toManifestState(t.prevService),
		Controller:    toManifestState(t.prevCtrl),
		Entries:       t.saved,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(t.backupDir, "manifest.json"), append(b, '\n'), 0600)
}

// enableControllerService stops and disables the legacy standalone controller
// service so it cannot race the unified client for the same identity. Any unit
// that exists is stopped even if the earlier probe thought it inactive, so a
// stale state read cannot leave a live daemon holding the identity. The caller
// recorded the previous state; rollback restores it.
func enableControllerService(manager serviceManager, prev serviceState) error {
	if prev.exists {
		if err := manager.controllerControl("stop"); err != nil {
			return fmt.Errorf("stop uulink-controller: %w", err)
		}
		// A stop that returns success must actually leave the unit inactive;
		// otherwise the legacy daemon keeps holding the controller identity
		// while the unified supervisor starts its own controller child.
		if err := waitControllerStopped(manager); err != nil {
			return err
		}
	}
	if prev.enabled {
		if err := manager.controllerDisable(); err != nil {
			return fmt.Errorf("disable uulink-controller: %w", err)
		}
	}
	return nil
}

func controllerEnable(manager serviceManager) error {
	if manager == openRC {
		return command("rc-update", "add", "uulink-controller", "default")
	}
	return command("systemctl", "enable", "uulink-controller.service")
}

// restoreServiceState re-applies the recorded enable/active combination for
// the `uulink` service name. Both directions are explicit: a previously
// disabled unit is disabled again (removing the symlink/runlevel the upgrade
// added), and a previously inactive one is stopped. hadUnit gates the whole
// restore: a fresh install that had no `uulink` unit must not be left running
// one after a rollback.
func restoreServiceState(manager serviceManager, st serviceState, hadUnit bool) error {
	if !hadUnit {
		return nil
	}
	if st.enabled {
		if err := manager.enable(); err != nil {
			return fmt.Errorf("enable uulink: %w", err)
		}
	} else if err := manager.disable(); err != nil {
		return fmt.Errorf("disable uulink: %w", err)
	}
	if st.active {
		if err := manager.control("start"); err != nil {
			return fmt.Errorf("start uulink: %w", err)
		}
	} else if err := manager.control("stop"); err != nil {
		return fmt.Errorf("stop uulink: %w", err)
	}
	return nil
}

func restoreControllerState(manager serviceManager, st serviceState, hadUnit bool) error {
	if !hadUnit {
		return nil
	}
	if st.enabled {
		if err := controllerEnable(manager); err != nil {
			return fmt.Errorf("enable uulink-controller: %w", err)
		}
	} else if err := manager.controllerDisable(); err != nil {
		return fmt.Errorf("disable uulink-controller: %w", err)
	}
	if st.active {
		if err := manager.controllerControl("start"); err != nil {
			return fmt.Errorf("start uulink-controller: %w", err)
		}
	} else if err := manager.controllerControl("stop"); err != nil {
		return fmt.Errorf("stop uulink-controller: %w", err)
	}
	return nil
}

// waitServiceActive confirms the supervisor actually came up before the
// installer claims success. A unit that is written but crash-loops must fail
// the install so rollback can run.
func waitServiceActive(manager serviceManager) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := manager.state()
		if err != nil {
			return err
		}
		// ready (ActiveState=active) is required, not merely active: a unit
		// stuck in "activating" must not be reported as a successful start.
		if st.ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("统一服务未在 20 秒内进入运行状态")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitServiceStopped confirms a stop took effect before the binary and unit
// files are replaced. A stale probe must not let the upgrade proceed while the
// old daemon is still running.
func waitServiceStopped(manager serviceManager) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := manager.state()
		if err != nil {
			return err
		}
		if !st.active {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("统一服务未在 10 秒内停止")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitControllerStopped confirms the legacy controller unit actually stopped
// before the unified supervisor takes over its identity. A successful stop
// command is not proof: a unit still shutting down (or a manager that reports
// stale state) would otherwise overlap the new controller child.
func waitControllerStopped(manager serviceManager) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := manager.controllerState()
		if err != nil {
			return err
		}
		if !st.active {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("旧控制端服务未在 10 秒内停止")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (t *unifiedTx) ensureLink() error {
	if info, err := os.Lstat(unifiedLinkPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(unifiedLinkPath); err == nil && target == installedBinary {
				return nil
			}
		}
		if err := os.Remove(unifiedLinkPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(installedBinary, unifiedLinkPath)
}

func (t *unifiedTx) commit(exe string, noStart bool) error {
	// Stop the racers first: the legacy controller service and the
	// controlled-only service must not overlap the unified supervisor.
	if err := enableControllerService(t.manager, t.prevCtrl); err != nil {
		return err
	}
	// A unit that exists is stopped even if the earlier probe reported it
	// inactive; only a unit that was never installed is skipped.
	if t.prevService.exists {
		if err := t.manager.control("stop"); err != nil {
			return fmt.Errorf("stop uulink: %w", err)
		}
		if err := waitServiceStopped(t.manager); err != nil {
			return err
		}
	}
	if err := copyExecutable(exe, installedBinary); err != nil {
		return err
	}
	if err := os.MkdirAll("/var/lib/uulink", 0700); err != nil {
		return err
	}
	if err := t.manager.writeUnifiedService(); err != nil {
		return err
	}
	t.unitWritten = true
	// The service is enabled even with --no-start: that flag means "write and
	// enable now, start on next boot", matching the controlled-only installer.
	if err := t.manager.enable(); err != nil {
		return fmt.Errorf("enable uulink: %w", err)
	}
	if !noStart {
		// Record the attempt before issuing start so a start that never reaches
		// active is still stopped by rollback.
		t.startAttempted = true
		if err := t.manager.control("start"); err != nil {
			return fmt.Errorf("start uulink: %w", err)
		}
		if err := waitServiceActive(t.manager); err != nil {
			return err
		}
	}
	if err := t.ensureLink(); err != nil {
		return err
	}
	// The marker goes last among the filesystem changes: it is what makes
	// `uulink` open the unified menu, so it must not exist until the unit and
	// link are in place.
	if err := writeFileDurable(unifiedMarkerPath, []byte(version+"\n"), 0600); err != nil {
		return err
	}
	t.markerAdded = true
	return writeUnifiedManifest()
}

func (t *unifiedTx) rollback() error {
	var firstErr error
	fail := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Any started or newly-written unit must be stopped, even if activation
	// never completed; otherwise a crash-looping supervisor keeps the identity
	// lock while the original service is restored underneath it.
	if t.startAttempted || t.unitWritten {
		fail(t.manager.control("stop"))
	}
	if t.markerAdded {
		if err := os.Remove(unifiedMarkerPath); err != nil && !os.IsNotExist(err) {
			fail(err)
		}
	}
	// Remove the enable symlink/runlevel the upgrade added before the unit file
	// is restored, so a fresh install cannot leave an enabled but missing unit.
	if t.unitWritten {
		fail(t.manager.disable())
	}
	// Reverse order restores the unit files before any service is restarted.
	for i := len(t.saved) - 1; i >= 0; i-- {
		fail(t.saved[i].restore())
	}
	if t.manager == systemd {
		fail(command("systemctl", "daemon-reload"))
	}
	fmt.Printf("恢复服务状态：%s\n", t.manager)
	fail(restoreServiceState(t.manager, t.prevService, t.hadUulink))
	fail(restoreControllerState(t.manager, t.prevCtrl, t.hadController))
	return firstErr
}

// writeUnifiedManifest records the installed binary's digest so `doctor` and
// operators can confirm what is on disk without re-reading the executable.
func writeUnifiedManifest() error {
	f, err := os.Open(installedBinary)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(map[string]string{
		"version":      version,
		"sha256":       hex.EncodeToString(h.Sum(nil)),
		"installed_at": time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(unifiedManifestPath, append(manifest, '\n'), 0644)
}

// installUnifiedCLI upgrades a device to the unified client: one supervisor
// running both the controlled and the controller role, so A can map B's ports
// and B can map A's ports on the same host. It preserves existing
// configuration and forwarding rules and is safe to re-run.
func installUnifiedCLI(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("安装器仅用于 Linux；当前系统是 %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		// Re-run the whole `install` verb with the original arguments. The
		// argument list (including --unified) is forwarded verbatim, so the
		// privileged child re-enters the unified path instead of silently
		// falling back to the controlled-only installer.
		if _, err = exec.LookPath("sudo"); err == nil {
			return command("sudo", append([]string{"--", exe, "install"}, args...)...)
		}
		if _, err = exec.LookPath("doas"); err == nil {
			return command("doas", append([]string{exe, "install"}, args...)...)
		}
		return fmt.Errorf("请切换到 root 后运行安装器（Alpine 可使用 su -），不需要额外安装 sudo")
	}
	f := flag.NewFlagSet("install", flag.ContinueOnError)
	importPath := f.String("import-config", "", "导入本机已有配置")
	phone := f.String("phone", "", "登录手机号")
	name := f.String("name", "", "设备名称")
	relay := f.Bool("relay", false, "仅使用UU中继")
	skip := f.Bool("skip-deps", false, "仅检查已安装依赖")
	noStart := f.Bool("no-start", false, "安装后暂不启动")
	noPower := f.Bool("disable-power", false, "禁用远程重启和关机")
	profile := f.String("profile", "", "安装方案：minimal 或 full")
	selected := f.String("components", "", "组件：mapping,terminal,files,desktop；mapping 始终保留")
	lowMemory := f.Bool("low-memory", false, "低内存模式；首次安装 Alpine 或内存不超过 512 MiB 时默认启用")
	yes := f.Bool("yes", false, "首次安装使用默认精简方案，跳过组件询问（登录仍需验证码）")
	// Accepted for symmetry with `install`; this function already is the
	// unified path, so the value is not consulted.
	f.Bool("unified", false, "升级为统一客户端（同时运行被控端与控制端，用于双向互控）")
	if err := f.Parse(args); err != nil {
		return err
	}
	// One installer per host: the transaction saves and restores global unit
	// files and the installed binary, so two concurrent upgrades would corrupt
	// each other's rollback state.
	lock, err := controllerLock(filepath.Join(agentStateDir, "install.lock"), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Preflight the link before touching anything: an unrelated program under
	// our name must not be silently replaced.
	if info, lerr := os.Lstat(unifiedLinkPath); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(unifiedLinkPath)
			if rerr != nil || target != installedBinary {
				return fmt.Errorf("%s 指向其他程序，未替换", unifiedLinkPath)
			}
		} else {
			src, serr := os.Stat(exe)
			if serr != nil || !os.SameFile(src, info) {
				return fmt.Errorf("%s 已存在且不属于此安装器", unifiedLinkPath)
			}
		}
	} else if !os.IsNotExist(lerr) {
		return lerr
	}

	fmt.Println("UU Linux 统一客户端安装器", version)
	fmt.Println("统一服务将以 root 运行；被控身份与出站控制身份各自独立登录、独立配置。")

	// A corrupt config is reported, never overwritten: silently resetting it
	// would deregister the device identity the operator is still using.
	controlled, err := readConfig(configDefault)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("被控端配置损坏，未覆盖：%w", err)
	}
	if *importPath != "" {
		if controlled, err = readConfig(*importPath); err != nil {
			return err
		}
	}
	if err = ensureMachineID(); err != nil {
		return err
	}
	if controlled.HostID != "" && controlled.HostID != machineID() {
		return fmt.Errorf("配置属于另一台机器；请在本机使用独立设备身份重新登录")
	}
	controlled.HostID = machineID()
	if *name != "" {
		controlled.Name = *name
	}
	if *relay {
		controlled.ICEPolicy = "relay"
	}
	// configureInstall normalizes an empty Components to the full set, but an
	// empty field is the legacy spelling of "full" for a pre-0.2.0 install. If
	// the operator did not ask to change components, keep the original empty
	// value so a pure upgrade does not rewrite the config byte for byte.
	provided := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { provided[fl.Name] = true })
	preserveEmptyComponents := controlled.Components == "" &&
		(controlled.DeviceID != "" || controlled.ClientID != "") &&
		!provided["profile"] && !provided["components"]

	controller, err := readConfig(controllerConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("控制端配置损坏，未覆盖：%w", err)
	}
	if controller.HostID != "" && controller.HostID != machineID() {
		return fmt.Errorf("配置属于另一台机器；请在本机使用独立设备身份重新登录")
	}
	// Only a genuinely new controller identity gets the machine binding and a
	// default name. An existing controller config must not be rewritten just
	// because it predates machine_id, or a pure upgrade would change its bytes.
	if controller.DeviceID == "" && controller.ClientID == "" {
		controller.HostID = machineID()
		if controller.Name == "" {
			host, _ := os.Hostname()
			controller.Name = "linux-" + host + "-controller"
		}
	}

	// Rules are validated, never rewritten: a live 0.0.0.0:11333 mapping must
	// survive the upgrade byte for byte.
	if _, err = readRules(controllerRulesPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("转发规则不可用，未修改：%w", err)
	}

	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(os.Stdin)
	if err = configureInstall(&controlled, f, *profile, *selected, *lowMemory, *noPower, *yes, reader); err != nil {
		return err
	}
	if preserveEmptyComponents {
		controlled.Components = ""
	}
	fmt.Printf("服务管理：%s；组件：%s；低内存模式：%t；远程电源：%t\n", manager, controlled.features(), controlled.LowMemory, controlled.AllowPower)
	if err = checkInstallSpace(exe, controlled); err != nil {
		return err
	}
	if err = ensureDependencies(controlled, manager, *skip); err != nil {
		return err
	}
	// Each role needs its own valid identity; complete whichever is missing
	// before the conflict check, which can only be evaluated once both exist.
	if controlled.Bearer == "" || controlled.UserID == "" || controlled.DeviceID == "" || controlled.ClientID == "" {
		if err = loginWithReader(configDefault, &controlled, *phone, reader); err != nil {
			return err
		}
	}
	if controller.Bearer == "" || controller.UserID == "" || controller.DeviceID == "" || controller.ClientID == "" {
		if err = loginWithReader(controllerConfigPath, &controller, *phone, reader); err != nil {
			return err
		}
	}
	// Fail closed before touching any file or service: both identities must be
	// complete and their component sets valid. A half-written pair would leave
	// the supervisor refusing one role after the old services are gone.
	for _, role := range []struct {
		label string
		cfg   agentConfig
	}{{"被控端", controlled}, {"控制端", controller}} {
		if role.cfg.DeviceID == "" || role.cfg.ClientID == "" || role.cfg.UserID == "" || role.cfg.Bearer == "" {
			return fmt.Errorf("%s身份不完整，未修改任何配置与服务", role.label)
		}
		if _, err = parseComponents(role.cfg.Components); err != nil {
			return fmt.Errorf("%s组件无效：%w", role.label, err)
		}
	}
	if controlled.DeviceID == controller.DeviceID || controlled.ClientID == controller.ClientID {
		return fmt.Errorf("被控端与控制端使用相同设备身份，无法同时运行；请为其中一个身份重新登录")
	}
	// Persist both configs before any service change, so a failure here leaves
	// the old services untouched with usable credentials on disk.
	if err = writeConfigIfChanged(configDefault, controlled); err != nil {
		return err
	}
	if err = writeConfigIfChanged(controllerConfigPath, controller); err != nil {
		return err
	}

	tx, err := newUnifiedTx(manager)
	if err != nil {
		return err
	}
	legacyController := tx.hadController
	fmt.Printf("升级备份目录：%s\n", tx.backupDir)
	if err = tx.commit(exe, *noStart); err != nil {
		if rerr := tx.rollback(); rerr != nil {
			return fmt.Errorf("%w；回滚也失败：%v", err, rerr)
		}
		return fmt.Errorf("%w；已回滚到迁移前状态", err)
	}
	if *noStart {
		fmt.Printf("安装完成（未启动）。设备：%s\n启用并启动：systemctl enable --now uulink.service（OpenRC：rc-update add uulink default && rc-service uulink start）\n", controlled.Name)
	} else {
		fmt.Printf("安装完成。设备：%s\n统一服务已启用并启动；被控身份与控制身份各自运行。\n查看状态：uulink status\n诊断：uulink doctor\n日志：uulink logs\n", controlled.Name)
	}
	if legacyController {
		fmt.Printf("旧控制端服务未自动恢复；其原 unit 副本保留在 %s，如仍需独立控制端请重新安装对应版本。\n", unifiedBackupDir)
	}
	return nil
}

// unifiedUninstallCLI tears down both the unified service and any legacy
// controller service it replaced. Leaving the controller unit enabled would
// resurrect the old single-role daemon on the next boot.
func unifiedUninstallCLI(args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("请以 root 运行 uulink uninstall")
	}
	f := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := f.Bool("purge", false, "同时删除登录配置")
	if err := f.Parse(args); err != nil {
		return err
	}
	// Share the installer lock so an uninstall cannot race an in-flight
	// upgrade's rollback over the same unit files and binary.
	lock, err := controllerLock(filepath.Join(agentStateDir, "install.lock"), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	if err = manager.checkAvailable(); err != nil {
		return err
	}
	if st, e := manager.state(); e != nil {
		return e
	} else if st.active {
		if err = manager.control("stop"); err != nil {
			return fmt.Errorf("stop uulink: %w", err)
		}
	}
	if st, e := manager.controllerState(); e != nil {
		return e
	} else if st.active {
		if err = manager.controllerControl("stop"); err != nil {
			return fmt.Errorf("stop uulink-controller: %w", err)
		}
	}
	if st, e := manager.state(); e != nil {
		return e
	} else if st.enabled {
		if err = manager.disable(); err != nil {
			return fmt.Errorf("disable uulink: %w", err)
		}
	}
	if st, e := manager.controllerState(); e != nil {
		return e
	} else if st.enabled {
		if err = manager.controllerDisable(); err != nil {
			return fmt.Errorf("disable uulink-controller: %w", err)
		}
	}
	// Both rotated log generations are removed so a reinstall starts clean.
	for _, p := range []string{
		manager.path(),
		manager.controllerPath(),
		unifiedMarkerPath,
		installedBinary,
		unifiedManifestPath,
		clientLogPath, clientLogPath + ".1",
		agentLogPath, agentLogPath + ".1",
	} {
		if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if target, err := os.Readlink(unifiedLinkPath); err == nil && target == installedBinary {
		os.Remove(unifiedLinkPath)
	}
	if manager == systemd {
		if err = command("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	if *purge {
		for _, p := range []string{configDefault, controllerConfigPath, controllerRulesPath} {
			if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		fmt.Println("服务与登录配置已移除。")
		return nil
	}
	fmt.Println("统一服务已卸载，登录配置与转发规则已保留，重新安装无需重复注册。")
	return nil
}
