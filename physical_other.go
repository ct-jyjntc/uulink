//go:build !darwin && !windows

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
)

// The macOS desktop policy does not change existing Linux server routing.
type providerNetworkInfo struct {
	Enforced                        bool
	Interface, Address, Name, Error string
	EventMonitor                    bool
}

func providerNetworkStatus() providerNetworkInfo           { return providerNetworkInfo{} }
func providerNetworkStamp(context.Context) (string, error) { return "", nil }
func watchProviderNetwork(context.Context, string, func()) {}
func providerHTTPTransport() http.RoundTripper             { return http.DefaultTransport }
func providerDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

// bigBufferNet enlarges the kernel socket buffers behind every ICE socket:
// the OS default (208 KiB on Linux) silently throttles high-BDP relay paths.
type bigBufferNet struct {
	transport.Net
}

func (n *bigBufferNet) ListenPacket(network, address string) (net.PacketConn, error) {
	c, err := n.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	if u, ok := c.(*net.UDPConn); ok {
		_ = u.SetReadBuffer(8 << 20)
		_ = u.SetWriteBuffer(8 << 20)
	}
	return c, nil
}

func (n *bigBufferNet) ListenUDP(network string, addr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, addr)
	if err != nil {
		return nil, err
	}
	_ = c.SetReadBuffer(8 << 20)
	_ = c.SetWriteBuffer(8 << 20)
	return c, nil
}

func newProviderPeerConnection(_ context.Context, config webrtc.Configuration) (*webrtc.PeerConnection, context.CancelFunc, error) {
	// High-RTT relay paths (~250 ms) spend most of a transfer climbing the
	// slow-start ramp. Raise both the receive buffer and the cwnd floor so a
	// fresh association starts near its steady-state bandwidth-delay product.
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, nil, err
	}
	settings := webrtc.SettingEngine{}
	settings.SetSCTPMaxReceiveBufferSize(4 << 20)
	settings.SetSCTPMinCwnd(2 << 20)
	settings.SetNet(&bigBufferNet{base})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(config)
	return pc, func() {}, err
}
func physicalNetworkCLI([]string) error {
	return fmt.Errorf("物理网卡直连诊断目前支持 macOS 和 Windows")
}
