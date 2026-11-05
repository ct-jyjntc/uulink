package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const minimalComponents = "mapping,terminal,files"
const fullComponents = minimalComponents + ",desktop"

type components struct{ terminal, files, desktop bool }

func parseComponents(value string) (components, error) {
	if value == "" {
		value = fullComponents
	}
	var c components
	for _, name := range strings.Split(value, ",") {
		switch strings.TrimSpace(name) {
		case "mapping":
		case "terminal":
			c.terminal = true
		case "files":
			c.files = true
		case "desktop":
			c.desktop = true
		default:
			return c, fmt.Errorf("未知组件 %q；支持 mapping,terminal,files,desktop", name)
		}
	}
	return c, nil
}
func (c components) String() string {
	names := []string{"mapping"}
	if c.terminal {
		names = append(names, "terminal")
	}
	if c.files {
		names = append(names, "files")
	}
	if c.desktop {
		names = append(names, "desktop")
	}
	return strings.Join(names, ",")
}
func (cfg agentConfig) features() components { c, _ := parseComponents(cfg.Components); return c }

func configureInstall(cfg *agentConfig, flags *flag.FlagSet, profile, selected string, lowMemory, noPower, yes bool, reader *bufio.Reader) error {
	provided := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["profile"] && provided["components"] {
		return fmt.Errorf("--profile 和 --components 不能同时使用")
	}
	first := cfg.Components == "" && cfg.DeviceID == ""
	interactive := first && !yes && !provided["profile"] && !provided["components"]
	if first {
		cfg.Components = minimalComponents
		cfg.LowMemory = recommendLowMemory(isAlpine(), memoryTotal())
		cfg.AllowPower = true
	}
	if provided["profile"] {
		switch profile {
		case "minimal":
			cfg.Components = minimalComponents
		case "full":
			cfg.Components = fullComponents
		default:
			return fmt.Errorf("--profile 必须为 minimal 或 full")
		}
	} else if provided["components"] {
		if strings.TrimSpace(selected) == "" {
			return fmt.Errorf("--components 不能为空；仅端口映射请指定 mapping")
		}
		cfg.Components = selected
	} else if interactive {
		if e := chooseComponents(reader, os.Stdout, cfg); e != nil {
			return e
		}
	}
	c, e := parseComponents(cfg.Components)
	if e != nil {
		return e
	}
	cfg.Components = c.String()
	if provided["low-memory"] {
		cfg.LowMemory = lowMemory
	}
	if provided["disable-power"] {
		cfg.AllowPower = !noPower
	} else if interactive {
		v, e := promptBool(reader, os.Stdout, "允许远程重启/关机", cfg.AllowPower)
		if e != nil {
			return e
		}
		cfg.AllowPower = v
	}
	return nil
}

// One reader is shared with login so piped input is not lost to read-ahead.
func chooseComponents(reader *bufio.Reader, out io.Writer, cfg *agentConfig) error {
	fmt.Fprintln(out, "请选择安装内容（不安装桌面环境）：\n  1. 精简（默认）：端口映射、终端、文件传输，无 ffmpeg/X11 依赖\n  2. 完整：精简功能 + X11 桌面控制（需要较多内存和磁盘）\n  3. 自定义：逐项选择可选功能")
	for {
		fmt.Fprint(out, "选择 [1/2/3，默认 1]：")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		switch strings.TrimSpace(line) {
		case "", "1":
			cfg.Components = minimalComponents
		case "2":
			cfg.Components = fullComponents
		case "3":
			c := components{}
			for _, item := range []struct {
				label   string
				target  *bool
				initial bool
			}{
				{"原生终端", &c.terminal, true}, {"文件传输", &c.files, true}, {"X11 桌面（安装 ffmpeg 和 X11 工具）", &c.desktop, false},
			} {
				v, e := promptBool(reader, out, item.label, item.initial)
				if e != nil {
					return e
				}
				*item.target = v
			}
			cfg.Components = c.String()
		default:
			if err == io.EOF {
				return fmt.Errorf("无效安装选项")
			}
			continue
		}
		return nil
	}
}
func promptBool(reader *bufio.Reader, out io.Writer, label string, initial bool) (bool, error) {
	options := "y/N"
	if initial {
		options = "Y/n"
	}
	for {
		fmt.Fprintf(out, "%s [%s]：", label, options)
		s, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "":
			return initial, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if err == io.EOF {
			return false, fmt.Errorf("%s：请输入 y 或 n", label)
		}
	}
}
