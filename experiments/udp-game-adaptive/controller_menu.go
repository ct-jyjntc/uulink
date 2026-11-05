//go:build !windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errMenuCancel = errors.New("已取消")

type controllerMenu struct {
	in   *bufio.Reader
	out  io.Writer
	opts controllerOptions
}

func (m *controllerMenu) ask(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(m.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(m.out, "%s: ", prompt)
	}
	s, e := m.in.ReadString('\n')
	if e != nil {
		return "", e
	}
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "q") {
		return "", errMenuCancel
	}
	if s == "" {
		s = def
	}
	return s, nil
}
func menuText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}
func (m *controllerMenu) list() (ruleDocument, error) {
	doc, e := readRules(m.opts.rules)
	if os.IsNotExist(e) {
		e = nil
	}
	if e != nil {
		return doc, e
	}
	daemon := readControllerStatus(filepath.Join(m.opts.state, "daemon.json"))
	fmt.Fprintf(m.out, "\n后台服务：%s\n", menuText(daemon.State))
	if daemon.Error != "" {
		fmt.Fprintln(m.out, menuText(daemon.Error))
	}
	if len(doc.Rules) == 0 {
		fmt.Fprintln(m.out, "暂无端口转发。选择 2 创建。")
		return doc, nil
	}
	for i, r := range doc.Rules {
		state := "已停用"
		if r.Enabled {
			state = "已启用，等待后台处理"
			if daemon.State == "状态未更新" || daemon.State == "已停止" {
				state = "已启用，后台服务未运行或状态未更新"
			} else {
				s := readControllerStatus(filepath.Join(m.opts.state, r.Device+".json"))
				if st := s.Rules[r.ID]; st != "" {
					state = st
				} else {
					state = s.State
				}
				if s.Error != "" {
					state += " (" + s.Error + ")"
				}
			}
		}
		name := r.DeviceName
		if name == "" {
			name = r.Device
		}
		fmt.Fprintf(m.out, "%d. %s | %s\n   %s → %s / %s\n", i+1, menuText(r.Name), menuText(state), r.Listen, menuText(name), r.Target)
	}
	return doc, nil
}
func (m *controllerMenu) create() error {
	fmt.Fprintln(m.out, "\n创建 TCP 转发（任意步骤输入 q 取消）")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	devices, e := controllerDevices(ctx, m.opts.config)
	if e != nil {
		fmt.Fprintf(m.out, "设备列表暂不可用：%s\n可手动输入设备 ID。\n", menuText(e.Error()))
	}
	for i, d := range devices {
		fmt.Fprintf(m.out, "%d. %s (%s) [%s / %s]\n", i+1, menuText(d.Name), d.ID, menuText(d.Status), menuText(d.Availability))
	}
	fmt.Fprintln(m.out, "0. 手动输入设备 ID")
	choice, e := m.ask("选择被控端编号", "")
	if e != nil {
		return e
	}
	n, e := strconv.Atoi(choice)
	if e != nil || n < 0 || n > len(devices) {
		return fmt.Errorf("无效的设备编号")
	}
	var d controllerDevice
	if n == 0 {
		d.ID, e = m.ask("被控端 UU 设备 ID", "")
		if e != nil {
			return e
		}
		d.Name = d.ID
	} else {
		d = devices[n-1]
	}
	if !safeControllerID.MatchString(d.ID) {
		return fmt.Errorf("无效的设备 ID")
	}
	local, e := m.ask("本地监听端口", "")
	if e != nil {
		return e
	}
	fmt.Fprintln(m.out, "监听范围：1 仅本机（默认）  2 所有 IPv4 地址  3 指定本机 IP")
	scope, e := m.ask("选择监听范围", "1")
	if e != nil {
		return e
	}
	host := "127.0.0.1"
	switch scope {
	case "1":
	case "2":
		host = "0.0.0.0"
	case "3":
		host, e = m.ask("本机监听 IP", "")
		if e != nil {
			return e
		}
	default:
		return fmt.Errorf("无效的监听范围")
	}
	targetHost, e := m.ask("目标地址（从被控端访问）", "127.0.0.1")
	if e != nil {
		return e
	}
	targetPort, e := m.ask("目标端口", "22")
	if e != nil {
		return e
	}
	name, e := m.ask("规则名称", menuText(d.Name)+"-"+local)
	if e != nil {
		return e
	}
	r := forwardRule{ID: uuid.NewString(), Name: name, Device: d.ID, DeviceName: menuText(d.Name), Listen: net.JoinHostPort(host, local), Target: net.JoinHostPort(targetHost, targetPort), Enabled: true}
	if e = validateRules(ruleDocument{Version: 1, Rules: []forwardRule{r}}); e != nil {
		return e
	}
	fmt.Fprintf(m.out, "\n%s：%s → %s / %s\n启用后自动重连、开机恢复；使用 UU 中继。\n", r.Name, r.Listen, r.DeviceName, r.Target)
	if host != "127.0.0.1" && host != "::1" {
		fmt.Fprintln(m.out, "此监听范围允许其他主机访问；实际可达性取决于防火墙和安全组。")
	}
	yes, e := m.ask("创建并启用？y/N", "n")
	if e != nil {
		return e
	}
	if !strings.EqualFold(yes, "y") {
		return errMenuCancel
	}
	return updateRules(m.opts.rules, func(doc *ruleDocument) error {
		// Probe for unrelated services before persisting. The daemon also retries
		// binding, since availability can change between this check and activation.
		doc.Rules = append(doc.Rules, r)
		if e := validateRules(*doc); e != nil {
			return e
		}
		l, e := net.Listen("tcp", r.Listen)
		if e != nil {
			return fmt.Errorf("监听端口不可用: %w", e)
		}
		return l.Close()
	})
}
func (m *controllerMenu) change(action string) error {
	doc, e := m.list()
	if e != nil {
		return e
	}
	if len(doc.Rules) == 0 {
		return errMenuCancel
	}
	s, e := m.ask("选择规则编号（q 取消）", "")
	if e != nil {
		return e
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 || n > len(doc.Rules) {
		return fmt.Errorf("无效的规则编号")
	}
	selected := doc.Rules[n-1]
	if action == "delete" {
		yes, e := m.ask(fmt.Sprintf("删除 %s (%s → %s)，并关闭该规则现有连接？y/N", menuText(selected.Name), selected.Listen, selected.Target), "n")
		if e != nil {
			return e
		}
		if !strings.EqualFold(yes, "y") {
			return errMenuCancel
		}
	}
	return updateRules(m.opts.rules, func(current *ruleDocument) error {
		for i, r := range current.Rules {
			if r.ID == selected.ID {
				switch action {
				case "delete":
					current.Rules = append(current.Rules[:i], current.Rules[i+1:]...)
				case "enable":
					current.Rules[i].Enabled = true
				case "disable":
					current.Rules[i].Enabled = false
				}
				return nil
			}
		}
		return fmt.Errorf("这条规则已被其他管理员删除，请刷新列表")
	})
}
func (m *controllerMenu) run() error {
	for {
		fmt.Fprintf(m.out, "\nUU Linux 控制端 %s\n1 查看转发与状态\n2 创建端口转发\n3 启用转发\n4 停用转发（保留规则）\n5 删除转发\n6 启动后台服务并设置开机自启\n0 退出菜单（已启用转发继续运行）\n", version)
		s, e := m.ask("请选择", "")
		if e == io.EOF {
			return nil
		}
		if e == errMenuCancel {
			return nil
		}
		if e != nil {
			return e
		}
		changed := false
		switch s {
		case "0":
			return nil
		case "1":
			_, e = m.list()
		case "2":
			e = m.create()
			changed = e == nil
		case "3":
			e = m.change("enable")
			changed = e == nil
		case "4":
			e = m.change("disable")
			changed = e == nil
		case "5":
			e = m.change("delete")
			changed = e == nil
		case "6":
			e = command("systemctl", "enable", "--now", "uulink-controller.service")
		default:
			e = fmt.Errorf("请输入菜单中的编号")
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			fmt.Fprintln(m.out, menuText(e.Error()))
		}
		if changed {
			fmt.Fprintln(m.out, "已保存。后台服务每秒检查规则，连接就绪后开始转发。")
			state := readControllerStatus(filepath.Join(m.opts.state, "daemon.json")).State
			if state == "状态未更新" || state == "已停止" {
				fmt.Fprintln(m.out, "后台服务未运行或状态未更新；可选择 6 启动。")
			}
		}
	}
}
func controllerMenuCLI(args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("请以 root 运行：sudo uulink-controller")
	}
	o, e := parseControllerOptions("menu", args)
	if e != nil {
		return e
	}
	m := controllerMenu{in: bufio.NewReader(os.Stdin), out: os.Stdout, opts: o}
	return m.run()
}
