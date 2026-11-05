//go:build darwin || windows

package main

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/pion/ice/v4"
	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
)

// Filtering ICE candidates alone does not constrain TURN sockets. Every Net
// entry point below uses the same source IP, kernel binding and DoH resolver.
type physicalPionNet struct {
	transport.Net
	e   physicalEgress
	ctx context.Context
}

var _ transport.Net = (*physicalPionNet)(nil)

func newProviderPeerConnection(parent context.Context, config webrtc.Configuration) (*webrtc.PeerConnection, context.CancelFunc, error) {
	e, err := currentPhysicalEgressContext(parent)
	if err != nil {
		return nil, nil, err
	}
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	settings := webrtc.SettingEngine{}
	settings.SetNet(&physicalPionNet{Net: base, e: e, ctx: ctx})
	// Pion active ICE-TCP uses a stdlib dialer outside transport.Net. TURN/TCP
	// still uses our bound DialTCP; disable only the unbound active candidate path.
	settings.DisableActiveTCP(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == e.iface.Name })
	settings.SetIPFilter(func(ip net.IP) bool { return ip.Equal(e.ip) })
	settings.SetRemoteIPFilter(usablePhysicalIP)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(config)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return pc, cancel, nil
}

func (n *physicalPionNet) Interfaces() ([]*transport.Interface, error) {
	iface := transport.NewInterface(n.e.iface)
	iface.AddAddress(&net.IPNet{IP: n.e.ip, Mask: n.e.mask})
	return []*transport.Interface{iface}, nil
}
func (n *physicalPionNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	if index != n.e.iface.Index {
		return nil, transport.ErrInterfaceNotFound
	}
	ifs, _ := n.Interfaces()
	return ifs[0], nil
}
func (n *physicalPionNet) InterfaceByName(name string) (*transport.Interface, error) {
	if name != n.e.iface.Name {
		return nil, transport.ErrInterfaceNotFound
	}
	ifs, _ := n.Interfaces()
	return ifs[0], nil
}

func (n *physicalPionNet) localAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsUnspecified() && !ip.Equal(n.e.ip) {
			return "", fmt.Errorf("监听地址不属于选定物理网卡")
		}
	}
	return net.JoinHostPort(n.e.ip.String(), port), nil
}

func (n *physicalPionNet) listenPacket(ctx context.Context, network, address string, base *net.ListenConfig) (net.PacketConn, error) {
	network, err := physicalNetwork(network)
	if err != nil || network != "udp4" {
		return nil, fmt.Errorf("不支持的物理 UDP 网络：%s", network)
	}
	address, err = n.localAddress(address)
	if err != nil {
		return nil, err
	}
	lc := net.ListenConfig{}
	if base != nil {
		lc = *base
	}
	lc.Control = n.e.control(lc.Control)
	return lc.ListenPacket(ctx, network, address)
}
func (n *physicalPionNet) ListenPacket(network, address string) (net.PacketConn, error) {
	return n.listenPacket(n.ctx, network, address, nil)
}
func (n *physicalPionNet) ListenUDP(network string, addr *net.UDPAddr) (transport.UDPConn, error) {
	address := ":0"
	if addr != nil {
		address = addr.String()
	}
	c, err := n.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}
func (n *physicalPionNet) listenTCP(ctx context.Context, network, address string, base *net.ListenConfig) (net.Listener, error) {
	network, err := physicalNetwork(network)
	if err != nil || network != "tcp4" {
		return nil, fmt.Errorf("不支持的物理 TCP 网络：%s", network)
	}
	address, err = n.localAddress(address)
	if err != nil {
		return nil, err
	}
	lc := net.ListenConfig{}
	if base != nil {
		lc = *base
	}
	lc.Control = n.e.control(lc.Control)
	return lc.Listen(ctx, network, address)
}

type physicalTCPListener struct{ *net.TCPListener }

func (l physicalTCPListener) AcceptTCP() (transport.TCPConn, error) { return l.TCPListener.AcceptTCP() }
func (n *physicalPionNet) ListenTCP(network string, addr *net.TCPAddr) (transport.TCPListener, error) {
	address := ":0"
	if addr != nil {
		address = addr.String()
	}
	l, err := n.listenTCP(n.ctx, network, address, nil)
	if err != nil {
		return nil, err
	}
	return physicalTCPListener{l.(*net.TCPListener)}, nil
}
func (n *physicalPionNet) Dial(network, address string) (net.Conn, error) {
	return n.e.dial(n.ctx, network, address, nil)
}
func (n *physicalPionNet) DialUDP(network string, local, remote *net.UDPAddr) (transport.UDPConn, error) {
	if remote == nil {
		return nil, fmt.Errorf("UDP 目标为空")
	}
	d := &net.Dialer{}
	if local != nil {
		d.LocalAddr = local
	}
	c, err := n.e.dial(n.ctx, network, remote.String(), d)
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}
func (n *physicalPionNet) DialTCP(network string, local, remote *net.TCPAddr) (transport.TCPConn, error) {
	if remote == nil {
		return nil, fmt.Errorf("TCP 目标为空")
	}
	d := &net.Dialer{}
	if local != nil {
		d.LocalAddr = local
	}
	c, err := n.e.dial(n.ctx, network, remote.String(), d)
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}
func (n *physicalPionNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	if network != "ip" && network != "ip4" {
		return nil, fmt.Errorf("物理出口仅支持 IPv4")
	}
	ips, err := n.e.lookup(n.ctx, address)
	if err != nil {
		return nil, err
	}
	return &net.IPAddr{IP: ips[0]}, nil
}
func (n *physicalPionNet) resolve(network, address, kind string) (net.IP, int, error) {
	network, err := physicalNetwork(network)
	if err != nil || network != kind+"4" {
		return nil, 0, fmt.Errorf("不支持的物理网络：%s", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return nil, 0, fmt.Errorf("无效目标端口")
	}
	ips, err := n.e.lookup(n.ctx, host)
	if err != nil {
		return nil, 0, err
	}
	return ips[0], p, nil
}
func (n *physicalPionNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	ip, p, err := n.resolve(network, address, "udp")
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: p}, nil
}
func (n *physicalPionNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	ip, p, err := n.resolve(network, address, "tcp")
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: ip, Port: p}, nil
}

type physicalPionDialer struct {
	n *physicalPionNet
	d net.Dialer
}

func (d physicalPionDialer) Dial(network, address string) (net.Conn, error) {
	ctx := d.n.ctx
	cancel := func() {}
	if d.d.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, d.d.Timeout)
	}
	defer cancel()
	return d.n.e.dial(ctx, network, address, &d.d)
}
func (n *physicalPionNet) CreateDialer(d *net.Dialer) transport.Dialer {
	copy := net.Dialer{}
	if d != nil {
		copy = *d
	}
	return physicalPionDialer{n, copy}
}

type physicalPionListenConfig struct {
	n  *physicalPionNet
	lc net.ListenConfig
}

func (l physicalPionListenConfig) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.n.ctx, cancel)
	defer stop()
	return l.n.listenTCP(ctx, network, address, &l.lc)
}
func (l physicalPionListenConfig) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.n.ctx, cancel)
	defer stop()
	return l.n.listenPacket(ctx, network, address, &l.lc)
}
func (n *physicalPionNet) CreateListenConfig(lc *net.ListenConfig) transport.ListenConfig {
	copy := net.ListenConfig{}
	if lc != nil {
		copy = *lc
	}
	return physicalPionListenConfig{n, copy}
}
