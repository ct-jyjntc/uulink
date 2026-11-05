package main

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows currently refreshes the routing snapshot every two seconds. The
// Windows network GUID distinguishes many network changes with the same IP.
func startPhysicalChangeMonitor() {}

func detectPhysicalEgress(ctx context.Context) (physicalEgress, error) {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return physicalEgress{}, err
	}
	if table == nil {
		return physicalEgress{}, errors.New("Windows 未返回 IPv4 路由表")
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	type route struct {
		metric uint32
		next   net.IP
	}
	defaults := map[uint32]route{}
	for _, row := range table.Rows() {
		if row.Loopback != 0 || row.DestinationPrefix.PrefixLength != 0 || row.DestinationPrefix.Prefix.Family != windows.AF_INET || row.NextHop.Family != windows.AF_INET || row.ValidLifetime == 0 {
			continue
		}
		raw := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.NextHop))
		next := net.IPv4(raw.Addr[0], raw.Addr[1], raw.Addr[2], raw.Addr[3])
		previous, ok := defaults[row.InterfaceIndex]
		if !ok || row.Metric < previous.metric || (row.Metric == previous.metric && next.String() < previous.next.String()) {
			defaults[row.InterfaceIndex] = route{row.Metric, next}
		}
	}
	size := uint32(15000)
	var buf []byte
	var head *windows.IpAdapterAddresses
	for tries := 0; tries < 3; tries++ {
		if size == 0 || size > 4<<20 {
			return physicalEgress{}, errors.New("Windows 网卡列表大小无效")
		}
		buf = make([]byte, size)
		head = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER, 0, head, &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) || tries == 2 {
			return physicalEgress{}, err
		}
	}
	var best physicalEgress
	minMetric := ^uint64(0)
	for a := head; a != nil; a = a.Next {
		if err := ctx.Err(); err != nil {
			return physicalEgress{}, err
		}
		r, ok := defaults[a.IfIndex]
		if !ok || a.OperStatus != windows.IfOperStatusUp || a.IfIndex == 0 {
			continue
		}
		if a.IfType != windows.IF_TYPE_ETHERNET_CSMACD && a.IfType != windows.IF_TYPE_IEEE80211 {
			continue
		}
		info := windows.MibIfRow2{InterfaceLuid: a.Luid}
		if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &info); err != nil || info.InterfaceAndOperStatusFlags&1 == 0 || info.InterfaceAndOperStatusFlags&2 != 0 {
			continue
		}
		name := windows.UTF16PtrToString(a.FriendlyName)
		if physicalInterfaceOverride != "" && !strings.EqualFold(physicalInterfaceOverride, name) {
			continue
		}
		var source net.IP
		var mask net.IPMask
		for v := a.FirstUnicastAddress; v != nil; v = v.Next {
			ip := v.Address.IP().To4()
			if !usablePhysicalIP(ip) || v.DadState != 4 || v.OnLinkPrefixLength > 32 {
				continue
			}
			source, mask = append(net.IP(nil), ip...), net.CIDRMask(int(v.OnLinkPrefixLength), 32)
			if (&net.IPNet{IP: source, Mask: mask}).Contains(r.next) {
				break
			}
		}
		metric := uint64(r.metric) + uint64(a.Ipv4Metric)
		if source == nil || metric > minMetric || (metric == minMetric && int(a.IfIndex) >= best.iface.Index) {
			continue
		}
		minMetric = metric
		label := "以太网"
		if a.IfType == windows.IF_TYPE_IEEE80211 {
			label = "Wi-Fi"
		}
		var mac net.HardwareAddr
		if a.PhysicalAddressLength <= uint32(len(a.PhysicalAddress)) {
			mac = append(net.HardwareAddr(nil), a.PhysicalAddress[:a.PhysicalAddressLength]...)
		}
		best = physicalEgress{iface: net.Interface{HardwareAddr: mac, Index: int(a.IfIndex), MTU: int(a.Mtu), Name: name, Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast}, ip: source, mask: mask, label: label, gateway: r.next.String() + "/" + a.NetworkGuid.String()}
	}
	if best.iface.Index == 0 {
		return physicalEgress{}, errors.New("未找到可用的物理 IPv4 网卡与默认路由；不会回退到虚拟网卡或代理")
	}
	return best, nil
}

const ipUnicastIf = 31 // Winsock IP_UNICAST_IF

func physicalSocketIndex(fd uintptr, expected int) (int, error) {
	value, err := windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIf)
	if err != nil {
		return 0, err
	}
	// Verify the exact adapter, allowing the provider to return host or network order.
	if value == expected || bits.ReverseBytes32(uint32(value)) == uint32(expected) {
		return expected, nil
	}
	return value, nil
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
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			socketErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIf, int(bits.ReverseBytes32(uint32(e.iface.Index))))
			if socketErr == nil {
				var index int
				index, socketErr = physicalSocketIndex(fd, e.iface.Index)
				if socketErr == nil && index != e.iface.Index {
					socketErr = errors.New("网卡绑定未生效")
				}
			}
		}); err != nil {
			return err
		}
		if socketErr != nil {
			return fmt.Errorf("绑定物理网卡 %s 失败：%w", e.iface.Name, socketErr)
		}
		return nil
	}
}
