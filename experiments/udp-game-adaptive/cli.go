//go:build !windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const version = "0.5.0-portmap-exp1"
const configDefault = "/etc/uulink/config.json"
const installedBinary = "/usr/local/lib/uulink/uulink"
const unitPath = "/etc/systemd/system/uulink.service"

func main() {
	if err := runCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "uulink:", err)
		os.Exit(1)
	}
}
func runCLI(args []string) error {
	mode := "install"
	switch filepath.Base(os.Args[0]) {
	case "uulink-controller":
		mode = "menu"
	case "uulink":
		// Only a device that has already switched to the unified client opens
		// the unified menu on a bare `uulink`; first-time packages keep the
		// controlled-side installer so existing deployments are unchanged.
		if unifiedInstalled() {
			mode = "client"
		}
	}
	if len(args) > 0 {
		mode = args[0]
		args = args[1:]
	}
	switch mode {
	case "network-check":
		return physicalNetworkCLI(args)
	case "app-run":
		return appOwnedCLI(args)
	case "client":
		return clientMenuCLI(args)
	case "client-daemon":
		return clientDaemonCLI(args)
	case "menu":
		return controllerMenuCLI(args)
	case "controller-daemon":
		return controllerDaemonCLI(args)
	case "controller-worker":
		if e := watchAppWorker(); e != nil {
			return e
		}
		return controllerWorkerCLI(args)
	case "udp-connect":
		return gameUDPConnectCLI(args)
	case "connect":
		return connectCLI(args)
	case "serve":
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		path := f.String("config", configPathDefault(), "configuration file")
		state := f.String("state-dir", agentStateDir, "runtime lock directory")
		logPath := f.String("log-file", "", "bounded log file (1 MiB x 2)")
		if err := f.Parse(args); err != nil {
			return err
		}
		if *logPath != "" {
			writer, err := openRotatingLog(*logPath)
			if err != nil {
				return err
			}
			defer writer.Close()
			log.SetOutput(writer)
		}
		// The controlled-side identity lock lives in the serve child so a
		// legacy `uulink serve` and the unified client cannot run the same
		// device twice. It is non-blocking: a second holder reports the conflict.
		// The identity is read before locking and re-checked after, so a login
		// that rewrites the config in between cannot leave this process running
		// under a lock that names a different identity.
		before, _ := readConfig(*path)
		identity := before.DeviceID + "\x00" + before.ClientID
		lock, err := agentIdentityLock(*state, *path, before.DeviceID)
		if err != nil {
			return err
		}
		defer lock.Close()
		if after, _ := readConfig(*path); after.DeviceID+"\x00"+after.ClientID != identity {
			return fmt.Errorf("设备身份在获取锁期间发生变化，请重试")
		}
		serve(*path, identity)
		return nil
	case "install":
		return installCLI(args)
	case "login":
		if os.Geteuid() != 0 && runtime.GOOS != "darwin" {
			return fmt.Errorf("请以 root 运行 uulink login")
		}
		f := flag.NewFlagSet("login", flag.ContinueOnError)
		path := f.String("config", configPathDefault(), "configuration file")
		phone := f.String("phone", "", "mobile phone")
		if err := f.Parse(args); err != nil {
			return err
		}
		if os.Geteuid() != 0 {
			home, err := os.UserHomeDir()
			rel, e := filepath.Rel(home, *path)
			if err != nil || e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("非 root 登录仅允许写入用户目录内的显式 --config 路径")
			}
		}
		cfg, _ := readConfig(*path)
		return loginInteractive(*path, &cfg, *phone)
	case "status":
		if unifiedInstalled() {
			return unifiedStatus()
		}
		manager, err := detectServiceManager()
		if err != nil {
			return err
		}
		return manager.control("status")
	case "logs":
		if unifiedInstalled() {
			return command("tail", "-n", "100", "-F", clientLogPath)
		}
		return command("tail", "-n", "100", "-F", agentLogPath)
	case "doctor":
		f := flag.NewFlagSet("doctor", flag.ContinueOnError)
		path := f.String("config", configPathDefault(), "configuration file")
		if err := f.Parse(args); err != nil {
			return err
		}
		if unifiedInstalled() {
			return unifiedDoctor(*path)
		}
		return doctor(*path)
	case "uninstall":
		return uninstallCLI(args)
	case "version", "--version":
		fmt.Printf("uulink %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "--help", "-h":
		fmt.Println("uulink [install|client|client-daemon|serve|menu|controller-daemon|connect|login|status|logs|doctor|uninstall|version]\nuulink-controller 无参数打开旧控制端端口转发菜单；uulink 无参数在已安装统一客户端的设备上打开统一菜单，否则启动被控端交互安装。安装和系统服务需要 root；凭据保存在 /etc/uulink/config.json。\nclient: 统一客户端菜单（同时管理本机被控身份与出站控制身份），需 root。\nclient-daemon: --config /etc/uulink/config.json --controller-config /etc/uulink-controller/config.json --rules /etc/uulink-controller/rules.json --state-dir /run/uulink-client --controller-state-dir /run/uulink-controller [--log-file PATH]\nconnect: --device DEVICE_ID --listen 127.0.0.1:28006 --target 127.0.0.1:22 [--relay] [--config /etc/uulink-controller/config.json] [--log-file PATH]\ninstall: --profile minimal|full 或 --components mapping,terminal,files,desktop --low-memory[=false] --yes --import-config PATH --phone NUMBER --name NAME --relay --skip-deps --no-start --disable-power\ninstall --unified: 显式升级为统一客户端（双角色），保留现有被控/控制配置与规则。\nuninstall: 默认保留设备身份；--purge 同时移除登录配置。")
		return nil
	default:
		return fmt.Errorf("unknown command %q; use uulink help", mode)
	}
}
func configPathDefault() string {
	if s := os.Getenv("UU_CONFIG_FILE"); s != "" {
		return s
	}
	return configDefault
}
func command(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
func machineID() string {
	if runtime.GOOS == "darwin" {
		b, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "\"IOPlatformUUID\"") {
					parts := strings.Split(line, "\"")
					if len(parts) >= 4 {
						return parts[3]
					}
				}
			}
		}
		return ""
	}

	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id", "/etc/uulink/machine-id"} {
		b, _ := os.ReadFile(path)
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	return ""
}
func ensureMachineID() error {
	if machineID() != "" {
		return nil
	}
	if err := os.MkdirAll("/etc/uulink", 0700); err != nil {
		return err
	}
	f, err := os.OpenFile("/etc/uulink/machine-id", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, uuid.NewString())
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func loginInteractive(path string, cfg *agentConfig, phone string) error {
	return loginWithReader(path, cfg, phone, bufio.NewReader(os.Stdin))
}
func loginWithReader(path string, cfg *agentConfig, phone string, reader *bufio.Reader) error {
	if err := ensureMachineID(); err != nil {
		return err
	}
	read := func(prompt string) (string, error) {
		fmt.Print(prompt)
		s, e := reader.ReadString('\n')
		if e != nil && e != io.EOF {
			return "", e
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return "", fmt.Errorf("输入不能为空")
		}
		return s, nil
	}
	if cfg.ClientID == "" {
		cfg.ClientID = strings.ToUpper(uuid.NewString())
	}
	if cfg.Name == "" {
		host, _ := os.Hostname()
		cfg.Name = "linux-" + host
	}
	if cfg.HostID != "" && cfg.HostID != machineID() {
		return fmt.Errorf("配置属于另一台机器；请使用本机新配置注册独立设备")
	}
	cfg.HostID = machineID()
	if err := writeConfig(path, *cfg); err != nil {
		return err
	}
	runtimeConfig = *cfg
	linuxBearer = ""
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if cfg.DeviceID == "" {
		var mac string
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			if i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) > 0 {
				mac = i.HardwareAddr.String()
				break
			}
		}
		body, _ := json.Marshal(map[string]any{"system_id": cfg.ClientID, "client_id": cfg.ClientID, "system_name": "macOS", "system_version": "27.2.0", "model_number": "UULINK-LINUX", "model_identifier": "Linux,1", "model": "Linux", "name": cfg.Name, "mac": mac, "cpu": runtime.GOARCH, "resolution": "1280x720", "dpi": 96, "video": []string{"Linux desktop"}, "controllable": true, "memory": "4096", "os": "Linux"})
		var result struct {
			DeviceID string `json:"device_id"`
		}
		if e := api(ctx, "/api/v1/device/macos/init", body, &result); e != nil {
			return e
		}
		if result.DeviceID == "" {
			return fmt.Errorf("设备注册未返回设备 ID")
		}
		cfg.DeviceID = result.DeviceID
		runtimeConfig = *cfg
		if e := writeConfig(path, *cfg); e != nil {
			return e
		}
	}
	var err error
	if phone == "" {
		phone, err = read("UU 登录手机号：")
		if err != nil {
			return err
		}
	}
	if len(phone) < 6 || len(phone) > 20 || strings.IndexFunc(phone, func(r rune) bool { return (r < '0' || r > '9') && r != '+' }) >= 0 {
		return fmt.Errorf("手机号格式不正确")
	}
	body, _ := json.Marshal(map[string]string{"mobile": phone, "type": "login"})
	if e := api(ctx, "/api/v1/security/mobile/code", body, nil); e != nil {
		return e
	}
	fmt.Println("验证码已发送。验证码只用于本次登录，不会保存。")
	code, e := read("短信验证码：")
	if e != nil {
		return e
	}
	body, _ = json.Marshal(map[string]string{"mobile": phone, "code": code})
	var result struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
	}
	if e = api(context.Background(), "/api/v1/login/by_mobile", body, &result); e != nil {
		return e
	}
	if result.UserID == "" || result.Token == "" {
		return fmt.Errorf("登录未返回有效身份")
	}
	cfg.UserID = result.UserID
	cfg.Bearer = result.Token
	runtimeConfig = *cfg
	linuxBearer = cfg.Bearer
	if e = writeConfig(path, *cfg); e != nil {
		return e
	}
	fmt.Println("登录成功。设备：", cfg.Name)
	return nil
}
func copyExecutable(src, dst string) error {
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
		return e
	}
	out, e := os.CreateTemp(filepath.Dir(dst), ".uulink-*")
	if e != nil {
		return e
	}
	defer os.Remove(out.Name())
	if _, e = io.Copy(out, in); e == nil {
		e = out.Chmod(0755)
	}
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(out.Name(), dst)
}

const serviceUnit = `[Unit]
Description=UU Remote Linux agent
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=/var/lib/uulink
ExecStart=/usr/local/lib/uulink/uulink serve --config /etc/uulink/config.json --log-file /var/log/uulink/agent.log
Restart=always
RestartSec=5
TimeoutStopSec=20
NoNewPrivileges=true
UMask=0077
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

func installCLI(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("安装器仅用于 Linux；当前系统是 %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		if _, e = exec.LookPath("sudo"); e == nil {
			return command("sudo", append([]string{"--", exe, "install"}, args...)...)
		}
		if _, e = exec.LookPath("doas"); e == nil {
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
	unified := f.Bool("unified", false, "升级为统一客户端（同时运行被控端与控制端，用于双向互控）")
	if e := f.Parse(args); e != nil {
		return e
	}
	// `install --unified` opts into the unified client explicitly. Once the
	// marker exists, a plain `install` must not silently downgrade the device
	// back to the controlled-only layout, so both paths converge here.
	if *unified || unifiedInstalled() {
		return installUnifiedCLI(args)
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	link := "/usr/local/bin/uulink"
	replaceOwn := false
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(link)
			if err != nil || target != installedBinary {
				return fmt.Errorf("%s 指向其他程序，未替换", link)
			}
		} else {
			src, err := os.Stat(exe)
			if err != nil || !os.SameFile(src, info) {
				return fmt.Errorf("%s 已存在且不属于此安装器", link)
			}
			replaceOwn = true
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	fmt.Println("UU Linux 安装器", version)
	fmt.Println("服务将以 root 运行，用于终端、文件系统与电源管理；设备连接仅使用出站信令和 WebRTC。")
	cfg, e := readConfig(configDefault)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if *importPath != "" {
		cfg, e = readConfig(*importPath)
		if e != nil {
			return e
		}
	}
	if e = ensureMachineID(); e != nil {
		return e
	}
	if cfg.HostID != "" && cfg.HostID != machineID() {
		return fmt.Errorf("不能复用其他机器的设备身份")
	}
	cfg.HostID = machineID()
	if *name != "" {
		cfg.Name = *name
	}
	if *relay {
		cfg.ICEPolicy = "relay"
	}
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	reader := bufio.NewReader(os.Stdin)
	if e = configureInstall(&cfg, f, *profile, *selected, *lowMemory, *noPower, *yes, reader); e != nil {
		return e
	}
	fmt.Printf("服务管理：%s；组件：%s；低内存模式：%t；远程电源：%t\n", manager, cfg.features(), cfg.LowMemory, cfg.AllowPower)
	if e = checkInstallSpace(exe, cfg); e != nil {
		return e
	}
	if e = ensureDependencies(cfg, manager, *skip); e != nil {
		return e
	}
	if cfg.Bearer == "" || cfg.UserID == "" || cfg.DeviceID == "" {
		if e = loginWithReader(configDefault, &cfg, *phone, reader); e != nil {
			return e
		}
	}
	if e = writeConfig(configDefault, cfg); e != nil {
		return e
	}
	// Record a recoverable copy of the previous unit without exposing credentials.
	if old, e := os.ReadFile(manager.path()); e == nil {
		backup := "/etc/uulink/service-before-upgrade"
		if e = os.WriteFile(backup, old, 0600); e != nil {
			return e
		}
	}
	if e = copyExecutable(exe, installedBinary); e != nil {
		return e
	}
	if e = os.MkdirAll("/var/lib/uulink", 0700); e != nil {
		return e
	}
	if _, err := os.Stat(manager.path()); err == nil {
		if e = manager.control("stop"); e != nil {
			return e
		}
	}
	if e = manager.writeService(); e != nil {
		return e
	}
	if replaceOwn {
		if e = os.Remove(link); e != nil {
			return e
		}
	}
	if _, e = os.Lstat(link); os.IsNotExist(e) {
		if e = os.Symlink(installedBinary, link); e != nil {
			return e
		}
	}

	if e = manager.enable(); e != nil {
		return e
	}
	if !*noStart {
		if e = manager.control("start"); e != nil {
			return e
		}
	}
	binary, e := os.Open(installedBinary)
	if e != nil {
		return e
	}
	hash := sha256.New()
	_, e = io.Copy(hash, binary)
	binary.Close()
	if e != nil {
		return e
	}
	manifest, _ := json.MarshalIndent(map[string]string{"version": version, "sha256": hex.EncodeToString(hash.Sum(nil)), "installed_at": time.Now().UTC().Format(time.RFC3339)}, "", "  ")
	if e = os.WriteFile("/usr/local/lib/uulink/manifest.json", manifest, 0644); e != nil {
		return e
	}
	fmt.Printf("安装完成。设备：%s\n服务已启用开机启动。\n查看状态：uulink status\n诊断：uulink doctor\n日志：uulink logs\n", cfg.Name)
	return nil
}
func uninstallCLI(args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("请以 root 运行 uulink uninstall")
	}
	// In unified mode a single service supervises both roles, so the generic
	// controlled-only teardown would leave the controller unit and the unified
	// marker behind. Delegate to the unified teardown instead.
	if unifiedInstalled() {
		return unifiedUninstallCLI(args)
	}
	f := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := f.Bool("purge", false, "同时删除登录配置")
	if e := f.Parse(args); e != nil {
		return e
	}
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	if _, err := os.Stat(manager.path()); err == nil {
		if e = manager.control("stop"); e != nil {
			return e
		}
		if e = manager.disable(); e != nil {
			return e
		}
	}
	for _, p := range []string{manager.path(), installedBinary, "/usr/local/lib/uulink/manifest.json", agentLogPath, agentLogPath + ".1"} {
		if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if target, e := os.Readlink("/usr/local/bin/uulink"); e == nil && target == installedBinary {
		os.Remove("/usr/local/bin/uulink")
	}
	if manager == systemd {
		_ = command("systemctl", "daemon-reload")
	}
	if *purge {
		if e := os.Remove(configDefault); e != nil && !os.IsNotExist(e) {
			return e
		}
		fmt.Println("服务与登录配置已移除。")
	} else {
		fmt.Println("服务已卸载，登录配置已保留，重新安装无需重复注册。")
	}
	return nil
}
func doctor(path string) error {
	fmt.Printf("uulink %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	cfg, e := readConfig(path)
	if e != nil {
		return fmt.Errorf("配置不可用：%w", e)
	}
	if _, e = parseComponents(cfg.Components); e != nil {
		return e
	}
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	missing := missingDependencies(cfg, manager)
	fmt.Printf("服务管理：%s；组件：%s；低内存模式：%t\n", manager, cfg.features(), cfg.LowMemory)
	fmt.Println("所选组件缺失依赖：", missing)
	fmt.Printf("设备：%s (%s)，登录状态：%t，电源操作：%t\n", cfg.Name, cfg.DeviceID, cfg.Bearer != "", cfg.AllowPower)
	if cfg.features().desktop {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d := detectDesktop(ctx)
		if d.display == "" {
			fmt.Println("桌面：无可访问的 X11 会话，将显示无桌面提示图")
		} else {
			fmt.Printf("桌面：X11 %s，%dx%d\n", d.display, d.width, d.height)
		}
	} else {
		fmt.Println("桌面：未安装，连接时仅发送内置提示帧，不运行桌面编码器")
	}
	fmt.Printf("日志：%s（每份最多 1 MiB，保留 2 份）\n", agentLogPath)
	if len(missing) > 0 {
		return errors.New("所选组件运行依赖不完整")
	}
	if cfg.DeviceID == "" || cfg.ClientID == "" || cfg.UserID == "" || cfg.Bearer == "" {
		return errors.New("设备尚未完成登录，请运行 uulink login")
	}
	return nil
}
