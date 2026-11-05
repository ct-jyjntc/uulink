//go:build darwin || windows

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/net/dns/dnsmessage"
)

// Read-only, account-free diagnostics for the exact transport used by the app.
// It does not start saved mappings, alter OS network settings or load identity.
func physicalNetworkCLI(args []string) error {
	f := flag.NewFlagSet("network-check", flag.ContinueOnError)
	iface := f.String("interface", "", "physical interface to diagnose (default: automatic)")
	timeout := f.Duration("timeout", 30*time.Second, "overall diagnostic timeout")
	domain := f.String("dns", "api.nrd.nie.163.com", "name to resolve using bound DoH")
	url := f.String("url", "https://www.apple.com/library/test/success.html", "HTTPS endpoint (empty skips HTTP)")
	udp := f.Bool("udp", false, "also query DNS using a bound native UDP socket")
	ice := f.Bool("ice", false, "also gather physical ICE candidates with public STUN")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	physicalInterfaceOverride = *iface
	e, err := currentPhysicalEgressContext(ctx)
	if err != nil {
		return err
	}
	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)
	_ = out.Encode(map[string]any{"physical_interface": e.iface.Name, "index": e.iface.Index, "source": e.ip.String(), "name": e.label, "proxy_policy": "disabled", "dns_policy": "physical DoH only", "families": "IPv4", "route_event_monitor": physicalMonitorOnline.Load()})
	if *domain != "" {
		ips, err := e.lookup(ctx, *domain)
		if err != nil {
			return err
		}
		_ = out.Encode(map[string]any{"dns": *domain, "addresses": ips})
	}
	if *url != "" {
		if !strings.HasPrefix(*url, "https://") {
			return fmt.Errorf("诊断 URL 必须使用 HTTPS")
		}
		binding := map[string]any{}
		req, err := http.NewRequestWithContext(ctx, "GET", *url, nil)
		if err != nil {
			return err
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { binding = physicalSocketDetails(info.Conn, e.iface.Index) }}))
		client := &http.Client{Transport: providerHTTPTransport(), Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if binding["bound_interface_index"] != e.iface.Index {
			return fmt.Errorf("HTTPS socket binding verification failed: %v", binding)
		}
		_ = out.Encode(map[string]any{"https_status": resp.StatusCode, "tls_verified": resp.TLS != nil && len(resp.TLS.VerifiedChains) > 0, "socket": binding})
	}
	if *udp {
		if err := physicalUDPDiagnostic(ctx, e, out); err != nil {
			return err
		}
	}
	if *ice {
		if err := physicalICEDiagnostic(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

func physicalSocketDetails(conn net.Conn, expected int) map[string]any {
	result := map[string]any{"local": conn.LocalAddr().String(), "remote": conn.RemoteAddr().String()}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn = tlsConn.NetConn()
	}
	if socket, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); ok {
		raw, err := socket.SyscallConn()
		if err == nil {
			err = raw.Control(func(fd uintptr) {
				index, e := physicalSocketIndex(fd, expected)
				if e == nil {
					result["bound_interface_index"] = index
				} else {
					result["error"] = e.Error()
				}
			})
		}
		if err != nil {
			result["error"] = err.Error()
		}
	}
	return result
}

func physicalUDPDiagnostic(ctx context.Context, e physicalEgress, out *json.Encoder) error {
	name, _ := dnsmessage.NewName("example.com.")
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 53721, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	wire, err := query.Pack()
	if err != nil {
		return err
	}
	c, err := e.rawDial(ctx, "udp4", "223.5.5.5:53", nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	if _, err = c.Write(wire); err != nil {
		return err
	}
	body := make([]byte, 4096)
	count, err := c.Read(body)
	if err != nil {
		return err
	}
	var answer dnsmessage.Message
	if err = answer.Unpack(body[:count]); err != nil {
		return err
	}
	if answer.ID != query.ID || !answer.Response || answer.RCode != dnsmessage.RCodeSuccess {
		return fmt.Errorf("UDP DNS returned invalid response")
	}
	binding := physicalSocketDetails(c, e.iface.Index)
	if binding["bound_interface_index"] != e.iface.Index {
		return fmt.Errorf("UDP socket binding verification failed")
	}
	return out.Encode(map[string]any{"native_udp_dns_answers": len(answer.Answers), "socket": binding})
}

func physicalICEDiagnostic(ctx context.Context, out *json.Encoder) error {
	pc, networkCancel, err := newProviderPeerConnection(ctx, webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		return err
	}
	defer func() { networkCancel(); pc.Close() }()
	if _, err = pc.CreateDataChannel("network-diagnostic", nil); err != nil {
		return err
	}
	done := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		return err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("ICE gathering timed out")
	case <-done:
	}
	candidates := []string{}
	for _, line := range strings.Split(pc.LocalDescription().SDP, "\r\n") {
		if strings.HasPrefix(line, "a=candidate:") {
			candidates = append(candidates, line)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("没有物理 ICE 候选")
	}
	return out.Encode(map[string]any{"physical_ice_candidates": candidates})
}
