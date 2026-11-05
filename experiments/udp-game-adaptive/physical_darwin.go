//go:build darwin

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

var physicalDevicePattern = regexp.MustCompile(`^en[0-9]+$`)
var serviceDevicePattern = regexp.MustCompile(`Device:\s*(en[0-9]+)\)`)
var physicalSelectedIndex atomic.Int64
var physicalLinkFlags atomic.Int64

// Link/address events also distinguish two networks which hand out the same
// local IP. Ignore virtual-interface and ARP events, which must not flap an
// otherwise healthy physical connection.
func startPhysicalChangeMonitor() {
	go func() {
		for {
			fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
			if err != nil {
				physicalMonitorOnline.Store(false)
				log.Printf("physical network monitor unavailable; retry in5s: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
			unix.CloseOnExec(fd)
			// Events missed while the monitor was offline cannot be replayed.
			physicalEpoch.Add(1)
			physicalMonitorOnline.Store(true)
			err = readPhysicalChanges(fd)
			physicalMonitorOnline.Store(false)
			unix.Close(fd)
			physicalEpoch.Add(1)
			log.Printf("physical network monitor stopped; retry in2s: %v", err)
			time.Sleep(2 * time.Second)
		}
	}()
}
func readPhysicalChanges(fd int) error {
	buf := make([]byte, 65536)
	for {
		count, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if count == 0 {
			return io.EOF
		}
		messages, err := route.ParseRIB(route.RIBTypeRoute, buf[:count])
		if err != nil {
			continue
		}
		for _, message := range messages {
			changed := false
			switch m := message.(type) {
			case *route.InterfaceMessage:
				if int64(m.Index) == physicalSelectedIndex.Load() {
					flags := int64(m.Flags & (unix.IFF_UP | unix.IFF_RUNNING))
					changed = physicalLinkFlags.Swap(flags) != flags
				}
			case *route.InterfaceAddrMessage:
				if int64(m.Index) == physicalSelectedIndex.Load() {
					for _, addr := range m.Addrs {
						if _, ok := addr.(*route.Inet4Addr); ok {
							changed = true
							break
						}
					}
				}
			}
			if changed {
				physicalEpoch.Add(1)
			}
		}
	}
}

func physicalCommand(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("读取物理网络失败：%s: %w", args[0], err)
	}
	return string(b), nil
}

func physicalRouteInterface(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "interface" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func detectPhysicalEgress(ctx context.Context) (physicalEgress, error) {
	ports, err := physicalCommand(ctx, "/usr/sbin/networksetup", "-listallhardwareports")
	if err != nil {
		return physicalEgress{}, err
	}
	hardware := map[string]string{}
	label := ""
	for _, line := range strings.Split(ports, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Hardware Port: ") {
			label = strings.TrimPrefix(line, "Hardware Port: ")
		}
		if strings.HasPrefix(line, "Device: ") {
			name := strings.TrimPrefix(line, "Device: ")
			if physicalDevicePattern.MatchString(name) {
				hardware[name] = label
			}
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return physicalEgress{}, err
	}
	candidates := map[string]physicalEgress{}
	for _, iface := range interfaces {
		if _, ok := hardware[iface.Name]; !ok || iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		if physicalInterfaceOverride != "" && iface.Name != physicalInterfaceOverride {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, subnet, err := net.ParseCIDR(addr.String())
			if err == nil && usablePhysicalIP(ip) {
				candidates[iface.Name] = physicalEgress{iface: iface, ip: append(net.IP(nil), ip.To4()...), mask: subnet.Mask, label: hardware[iface.Name]}
				break
			}
		}
	}
	order := []string{}
	if route, err := physicalCommand(ctx, "/sbin/route", "-n", "get", "default"); err == nil {
		order = append(order, physicalRouteInterface(route))
	}
	if services, err := physicalCommand(ctx, "/usr/sbin/networksetup", "-listnetworkserviceorder"); err == nil {
		for _, match := range serviceDevicePattern.FindAllStringSubmatch(services, -1) {
			order = append(order, match[1])
		}
	}
	// Include a newly attached hardware port before its service is configured.
	for _, iface := range interfaces {
		order = append(order, iface.Name)
	}
	seen := map[string]bool{}
	for _, name := range order {
		e, ok := candidates[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		route, err := physicalCommand(ctx, "/sbin/route", "-n", "get", "-ifscope", name, "1.1.1.1")
		if err == nil && physicalRouteInterface(route) == name && strings.Contains(route, "GATEWAY") {
			for _, line := range strings.Split(route, "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "gateway" {
					e.gateway = strings.TrimSpace(v)
				}
			}
			e.epoch = physicalEpoch.Load()
			if physicalSelectedIndex.Swap(int64(e.iface.Index)) != int64(e.iface.Index) {
				flags := int64(0)
				if e.iface.Flags&net.FlagUp != 0 {
					flags |= unix.IFF_UP
				}
				if e.iface.Flags&net.FlagRunning != 0 {
					flags |= unix.IFF_RUNNING
				}
				physicalLinkFlags.Store(flags)
			}
			return e, nil
		}
	}
	return physicalEgress{}, fmt.Errorf("未找到具有物理 IPv4 出口的网卡；不会回退到虚拟网卡或代理")
}

func (e physicalEgress) control(previous func(string, string, syscall.RawConn) error) func(string, string, syscall.RawConn) error {
	return func(network, address string, raw syscall.RawConn) error {
		if network != "tcp4" && network != "udp4" {
			return fmt.Errorf("物理出口不支持 %s", network)
		}
		if previous != nil {
			if err := previous(network, address, raw); err != nil {
				return err
			}
		}
		var bindErr error
		err := raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, e.iface.Index)
			if bindErr == nil {
				var index int
				index, bindErr = physicalSocketIndex(fd, e.iface.Index)
				if bindErr == nil && index != e.iface.Index {
					bindErr = fmt.Errorf("网卡绑定未生效")
				}
			}
		})
		if err != nil {
			return err
		}
		if bindErr != nil {
			return fmt.Errorf("绑定物理网卡 %s 失败：%w", e.iface.Name, bindErr)
		}
		return nil
	}
}

func physicalSocketIndex(fd uintptr, _ int) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF)
}
