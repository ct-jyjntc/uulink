//go:build !darwin && !windows

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"

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
func newProviderPeerConnection(_ context.Context, config webrtc.Configuration) (*webrtc.PeerConnection, context.CancelFunc, error) {
	pc, err := webrtc.NewPeerConnection(config)
	return pc, func() {}, err
}
func physicalNetworkCLI([]string) error {
	return fmt.Errorf("物理网卡直连诊断目前支持 macOS 和 Windows")
}
