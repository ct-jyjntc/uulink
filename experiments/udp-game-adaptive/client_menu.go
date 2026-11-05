//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// clientMenu is the unified client menu. One interactive session manages both
// UU roles on this machine: the controlled identity (this device can be
// reached by peers) and the outbound controller identity (this device maps
// peers' ports). Rules are stored in the same file the controller daemon uses,
// so the reused controllerMenu list/create/change helpers stay authoritative.
type clientMenu struct {
	in    *bufio.Reader
	out   io.Writer
	opts  clientOptions
	rules *controllerMenu
}

func clientMenuCLI(args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("请以 root 运行：sudo uulink client")
	}
	o, e := parseClientOptions(args)
	if e != nil {
		return e
	}
	// Share one reader with the reused controller menu and the login prompts so
	// piped input is never consumed by read-ahead.
	reader := bufio.NewReader(os.Stdin)
	m := &clientMenu{
		in:   reader,
		out:  os.Stdout,
		opts: o,
		rules: &controllerMenu{
			in:   reader,
			out:  os.Stdout,
			opts: controllerOptions{config: o.controller, rules: o.rules, state: o.controllerState},
		},
	}
	return m.run()
}

func readClientStatusFile(path string) (clientStatus, bool) {
	var s clientStatus
	b, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(b, &s) != nil || time.Since(s.Updated) > 5*time.Second {
		return s, false
	}
	return s, true
}

// roleSummary renders one identity for the status screen. It deliberately
// separates "logged in" (credentials present and usable) from "online", which
// the menu cannot confirm from local state alone.
func roleSummary(out io.Writer, label, path string) {
	cfg, e := readConfig(path)
	if e != nil {
		if os.IsNotExist(e) {
			fmt.Fprintf(out, "%s：未登录（%s）\n", label, path)
		} else {
			fmt.Fprintf(out, "%s：配置读取失败：%s\n", label, menuText(e.Error()))
		}
		return
	}
	if cfg.DeviceID == "" || cfg.ClientID == "" || cfg.UserID == "" || cfg.Bearer == "" {
		fmt.Fprintf(out, "%s：尚未完成登录（%s）\n", label, path)
		return
	}
	name := menuText(cfg.Name)
	if name == "" {
		name = "(未命名)"
	}
	fmt.Fprintf(out, "%s：%s 设备ID %s\n", label, name, cfg.DeviceID)
}

func (m *clientMenu) status() error {
	fmt.Fprintf(m.out, "\n统一客户端 %s\n", version)
	// The controlled device ID is the one a peer must map; the controller ID is
	// this machine's outbound identity. Both are independent UU identities.
	roleSummary(m.out, "本机被控身份（对端请用它做映射）", m.opts.config)
	roleSummary(m.out, "本机出站控制身份", m.opts.controller)

	st, fresh := readClientStatusFile(filepath.Join(m.opts.state, "client.json"))
	if !fresh {
		fmt.Fprintln(m.out, "统一服务状态：未运行或状态未更新（超过 5 秒未刷新）")
	} else {
		fmt.Fprintf(m.out, "统一服务：%s\n", menuText(st.Supervisor))
		if st.Error != "" {
			fmt.Fprintln(m.out, menuText(st.Error))
		}
		for _, name := range []string{clientRoleControlled, clientRoleController} {
			r, ok := st.Roles[name]
			label := name
			if name == clientRoleControlled {
				label = "被控角色"
			} else {
				label = "控制角色"
			}
			if !ok {
				fmt.Fprintf(m.out, "  %s：状态缺失\n", label)
				continue
			}
			line := fmt.Sprintf("  %s：%s", label, menuText(r.Process))
			if r.PID != 0 {
				line += fmt.Sprintf("（PID %d）", r.PID)
			}
			if r.Detail != "" {
				line += " - " + menuText(r.Detail)
			}
			fmt.Fprintln(m.out, line)
		}
		fmt.Fprintln(m.out, "注意：进程运行不等于已连通；本地无法确认 UU 可达性与对端在线。")
	}
	return nil
}

// loginRole reuses the standard login flow against one config path. A corrupt
// config is reported and never overwritten; an empty config is seeded with a
// local name and the minimal component set so mapping-only hosts stay lean.
func (m *clientMenu) loginRole(path, label, nameSuffix string) error {
	cfg, e := readConfig(path)
	if e != nil {
		if !os.IsNotExist(e) {
			return fmt.Errorf("%s 配置损坏，未覆盖：%w", label, e)
		}
		cfg = agentConfig{}
	}
	// Only seed defaults for a genuinely new identity. An existing config with
	// an empty Components field is a pre-0.2.0 full-feature install, and
	// re-login must not silently downgrade it to mapping-only.
	newIdentity := cfg.DeviceID == "" && cfg.ClientID == ""
	if cfg.Name == "" {
		host, _ := os.Hostname()
		cfg.Name = "linux-" + host + nameSuffix
	}
	if newIdentity && cfg.Components == "" {
		cfg.Components = minimalComponents
	}
	fmt.Fprintf(m.out, "\n登录%s（%s）\n", label, path)
	if e := loginWithReader(path, &cfg, "", m.in); e != nil {
		return e
	}
	fmt.Fprintf(m.out, "%s 登录完成。\n", label)
	return nil
}

// serviceAction starts or stops the unified service. Both actions require the
// unified marker: without it the device is still in the split
// controlled/controller layout and the menu must not silently rewrite it.
func (m *clientMenu) serviceAction(start bool) error {
	if !unifiedInstalled() {
		return fmt.Errorf("本机尚未安装统一客户端，请先运行 uulink install --unified")
	}
	// The service unit is written with the production paths. If this menu was
	// opened with custom --config/--state-dir (for tests or a side install),
	// toggling the production service would act on a different installation
	// than the one being inspected, so refuse rather than mislead.
	if m.opts.config != configDefault || m.opts.controller != controllerConfigPath ||
		m.opts.rules != controllerRulesPath || m.opts.state != clientStateDir ||
		m.opts.controllerState != controllerStateDir {
		return fmt.Errorf("当前菜单使用了自定义路径，仅默认安装可管理统一服务；请在默认安装上运行 uulink client")
	}
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	if start {
		if e = manager.enable(); e != nil {
			return e
		}
		return manager.control("start")
	}
	if e = manager.control("stop"); e != nil {
		return e
	}
	return manager.disable()
}

func (m *clientMenu) run() error {
	for {
		fmt.Fprintf(m.out, "\nUU Linux 统一客户端 %s\n1 查看状态与转发\n2 创建端口转发\n3 启用转发\n4 停用转发（保留规则）\n5 删除转发\n6 登录被控端身份\n7 登录出站控制身份\n8 启动统一服务并设置开机自启\n9 停止统一服务\n0 退出菜单（不停止服务）\n", version)
		s, e := m.in.ReadString('\n')
		if e != nil && e != io.EOF {
			return e
		}
		s = strings.TrimSpace(s)
		changed := false
		switch s {
		case "0", "q":
			return nil
		case "1":
			e = m.status()
			if e == nil {
				_, e = m.rules.list()
			}
		case "2":
			e = m.rules.create()
			changed = e == nil
		case "3":
			e = m.rules.change("enable")
			changed = e == nil
		case "4":
			e = m.rules.change("disable")
			changed = e == nil
		case "5":
			e = m.rules.change("delete")
			changed = e == nil
		case "6":
			e = m.loginRole(m.opts.config, "被控端身份", "")
		case "7":
			e = m.loginRole(m.opts.controller, "出站控制身份", "-controller")
		case "8":
			e = m.serviceAction(true)
		case "9":
			e = m.serviceAction(false)
		default:
			if s == "" && e == io.EOF {
				return nil
			}
			e = fmt.Errorf("请输入菜单中的编号")
		}
		if e == io.EOF {
			return nil
		}
		if e == errMenuCancel {
			e = nil
		}
		if e != nil {
			fmt.Fprintln(m.out, menuText(e.Error()))
		}
		if changed {
			fmt.Fprintln(m.out, "已保存。统一服务每秒检查规则，连接就绪后开始转发。")
		}
		if e == nil && s != "" && (s == "6" || s == "7") {
			fmt.Fprintln(m.out, "提示：身份变更后统一服务会自动重启对应角色，无需手工重启。")
		}
	}
}

// unifiedStatus backs `uulink status` once the device is in unified mode. It
// reports the service manager state and the supervisor's own status file, and
// never claims connectivity it cannot verify.
func unifiedStatus() error {
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	if e = manager.control("status"); e != nil {
		// A stopped service is a normal state for status; report and continue.
		fmt.Println("服务未运行或状态不可用。")
	}
	st, fresh := readClientStatusFile(filepath.Join(clientStateDir, "client.json"))
	if !fresh {
		fmt.Println("统一服务状态：未运行或状态未更新（超过 5 秒未刷新）")
		return nil
	}
	fmt.Printf("统一服务：%s\n", menuText(st.Supervisor))
	if st.Error != "" {
		fmt.Println(menuText(st.Error))
	}
	for _, name := range []string{clientRoleControlled, clientRoleController} {
		if r, ok := st.Roles[name]; ok {
			line := fmt.Sprintf("%s：%s", name, menuText(r.Process))
			if r.PID != 0 {
				line += fmt.Sprintf("（PID %d）", r.PID)
			}
			if r.Detail != "" {
				line += " - " + menuText(r.Detail)
			}
			fmt.Println(line)
		}
	}
	fmt.Println("注意：进程运行不等于已连通。")
	return nil
}

// unifiedDoctor is the read-only diagnostic for unified mode. It validates
// both independent configs and the selected components' dependencies, and
// reports the unified marker so an operator can tell the two layouts apart.
func unifiedDoctor(path string) error {
	fmt.Printf("uulink %s %s/%s（统一客户端）\n", version, runtime.GOOS, runtime.GOARCH)
	manager, e := detectServiceManager()
	if e != nil {
		return e
	}
	fmt.Printf("服务管理：%s；统一标记：%s\n", manager, unifiedMarkerPath)

	var problems []string
	controlled, e := readConfig(path)
	if e != nil {
		problems = append(problems, fmt.Sprintf("被控配置不可用（%s）：%v", path, e))
	} else {
		if _, e = parseComponents(controlled.Components); e != nil {
			problems = append(problems, e.Error())
		} else {
			missing := missingDependencies(controlled, manager)
			fmt.Printf("被控身份：%s (%s)，组件：%s；缺失依赖：%v\n", controlled.Name, controlled.DeviceID, controlled.features(), missing)
			if len(missing) > 0 {
				problems = append(problems, "被控端所选组件运行依赖不完整")
			}
		}
		if controlled.DeviceID == "" || controlled.ClientID == "" || controlled.UserID == "" || controlled.Bearer == "" {
			problems = append(problems, "被控身份尚未完成登录，请运行 uulink login 或菜单 6")
		}
	}

	controller, e := readConfig(controllerConfigPath)
	if e != nil {
		problems = append(problems, fmt.Sprintf("控制配置不可用（%s）：%v", controllerConfigPath, e))
	} else {
		fmt.Printf("出站控制身份：%s (%s)\n", controller.Name, controller.DeviceID)
		if controller.DeviceID == "" || controller.ClientID == "" || controller.UserID == "" || controller.Bearer == "" {
			problems = append(problems, "出站控制身份尚未完成登录，请运行 uulink login --config "+controllerConfigPath+" 或菜单 7")
		}
	}
	// loadClientRole is the same validator the supervisor uses, so doctor and
	// the runtime agree on what "usable" means (login, components, HostID).
	for _, r := range []struct {
		label string
		path  string
	}{{"被控角色", path}, {"控制角色", controllerConfigPath}} {
		if role := loadClientRole(r.path); !role.ready() {
			problems = append(problems, fmt.Sprintf("%s不可运行：%s", r.label, role.detail))
		}
	}
	// Two roles on one host cannot run the same UU identity; the supervisor
	// refuses to start the controller in that case, so doctor must fail too.
	if controlled.DeviceID != "" && controller.DeviceID != "" &&
		(controlled.DeviceID == controller.DeviceID || (controlled.ClientID != "" && controlled.ClientID == controller.ClientID)) {
		problems = append(problems, "被控端与控制端使用相同设备身份，无法同时运行；请为其中一个身份重新登录")
	}
	if _, e := readRules(controllerRulesPath); e != nil && !os.IsNotExist(e) {
		problems = append(problems, fmt.Sprintf("转发规则不可用（%s）：%v", controllerRulesPath, e))
	}

	fmt.Printf("日志：%s（每份最多 1 MiB，保留 2 份）\n", clientLogPath)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("- " + p)
		}
		return fmt.Errorf("统一客户端诊断发现 %d 个问题", len(problems))
	}
	return nil
}
