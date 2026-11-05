//go:build darwin || windows

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"golang.org/x/net/dns/dnsmessage"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// These sockets cover the provider control plane and ICE transports only.
// Local API/listeners and forwarded target services retain their own routing.
// IPv4 is deliberate: never silently create an unbound IPv6 socket.
type physicalEgress struct {
	iface   net.Interface
	ip      net.IP
	mask    net.IPMask
	label   string
	gateway string
	epoch   uint64
}

type providerNetworkInfo struct {
	Enforced                        bool
	Interface, Address, Name, Error string
	EventMonitor                    bool
}

var physicalCache struct {
	sync.Mutex
	until   time.Time
	egress  physicalEgress
	err     error
	epoch   uint64
	pending chan struct{}
}

// Set only by the diagnostic command, before any networking starts.
var physicalInterfaceOverride string
var physicalWatchOnce sync.Once
var physicalEpoch atomic.Uint64
var physicalMonitorOnline atomic.Bool

func usablePhysicalIP(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4.IsGlobalUnicast() && !v4.IsLinkLocalUnicast() && !(v4[0] == 198 && v4[1]&254 == 18)
}

func currentPhysicalEgress() (physicalEgress, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return currentPhysicalEgressContext(ctx)
}

// One bounded discovery runs outside the lock; cancelled callers stop waiting
// immediately, without cancelling discovery needed by other requests.
func currentPhysicalEgressContext(ctx context.Context) (physicalEgress, error) {
	physicalWatchOnce.Do(startPhysicalChangeMonitor)
	for {
		if err := ctx.Err(); err != nil {
			return physicalEgress{}, err
		}
		physicalCache.Lock()
		if time.Now().Before(physicalCache.until) && physicalCache.epoch == physicalEpoch.Load() {
			e, err := physicalCache.egress, physicalCache.err
			physicalCache.Unlock()
			return e, err
		}
		if physicalCache.pending == nil {
			physicalCache.pending = make(chan struct{})
			go func() {
				epoch := physicalEpoch.Load()
				work, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				e, err := detectPhysicalEgress(work)
				cancel()
				if err == nil {
					epoch = e.epoch
				}
				physicalCache.Lock()
				physicalCache.egress = e
				physicalCache.err = err
				physicalCache.epoch = epoch
				physicalCache.until = time.Now().Add(2 * time.Second)
				done := physicalCache.pending
				physicalCache.pending = nil
				close(done)
				physicalCache.Unlock()
			}()
		}
		done := physicalCache.pending
		physicalCache.Unlock()
		select {
		case <-ctx.Done():
			return physicalEgress{}, ctx.Err()
		case <-done:
		}
	}
}

func (e physicalEgress) stamp() string {
	return fmt.Sprintf("%s/%d/%s/%s/%d", e.iface.Name, e.iface.Index, e.ip, e.gateway, e.epoch)
}

func providerNetworkStatus() providerNetworkInfo {
	e, err := currentPhysicalEgress()
	if err != nil {
		return providerNetworkInfo{Enforced: true, Error: err.Error(), EventMonitor: physicalMonitorOnline.Load()}
	}
	return providerNetworkInfo{Enforced: true, Interface: e.iface.Name, Address: e.ip.String(), Name: e.label, EventMonitor: physicalMonitorOnline.Load()}
}

func providerNetworkStamp(ctx context.Context) (string, error) {
	e, err := currentPhysicalEgressContext(ctx)
	if err != nil {
		return "", err
	}
	return e.stamp(), nil
}

func watchProviderNetwork(ctx context.Context, stamp string, changed func()) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := providerNetworkStamp(ctx)
				if err != nil {
					failures++
					if failures < 2 {
						continue
					}
				} else {
					failures = 0
				}
				if err != nil || next != stamp {
					changed()
					return
				}
			}
		}
	}()
}

func physicalNetwork(network string) (string, error) {
	switch network {
	case "tcp", "tcp4":
		return "tcp4", nil
	case "udp", "udp4":
		return "udp4", nil
	}
	return "", fmt.Errorf("物理出口不支持 %s", network)
}

func (e physicalEgress) rawDial(ctx context.Context, network, address string, template *net.Dialer) (net.Conn, error) {
	network, err := physicalNetwork(network)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !usablePhysicalIP(net.ParseIP(host)) {
		return nil, fmt.Errorf("拒绝非物理 IPv4 目标或 Fake-IP：%s", host)
	}
	d := net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	if template != nil {
		d = *template
	}
	if d.Timeout == 0 {
		d.Timeout = 8 * time.Second
	}
	port := 0
	if d.LocalAddr != nil {
		var ip net.IP
		switch local := d.LocalAddr.(type) {
		case *net.TCPAddr:
			ip, port = local.IP, local.Port
		case *net.UDPAddr:
			ip, port = local.IP, local.Port
		default:
			return nil, fmt.Errorf("无效物理源地址")
		}
		if len(ip) > 0 && !ip.IsUnspecified() && !ip.Equal(e.ip) {
			return nil, fmt.Errorf("源地址不属于选定物理网卡")
		}
	}
	if network == "tcp4" {
		d.LocalAddr = &net.TCPAddr{IP: e.ip, Port: port}
	} else {
		d.LocalAddr = &net.UDPAddr{IP: e.ip, Port: port}
	}
	previous, previousContext := d.Control, d.ControlContext
	d.Control = nil
	d.ControlContext = func(controlCtx context.Context, network, address string, raw syscall.RawConn) error {
		if previousContext != nil {
			if err := previousContext(controlCtx, network, address, raw); err != nil {
				return err
			}
		} else if previous != nil {
			if err := previous(network, address, raw); err != nil {
				return err
			}
		}
		return e.control(nil)(network, address, raw)
	}
	// Always resolve before reaching net.Dialer, so Go cannot use system DNS.
	return d.DialContext(ctx, network, address)
}

type physicalDNSPoint struct{ host, ip string }

var physicalDNSPoints = []physicalDNSPoint{{"dns.alidns.com", "223.5.5.5"}, {"dns.alidns.com", "223.6.6.6"}, {"cloudflare-dns.com", "1.1.1.1"}}
var physicalDNSCache = struct {
	sync.Mutex
	entries map[string]physicalDNSResult
}{entries: map[string]physicalDNSResult{}}

type physicalDNSResult struct {
	addresses []net.IP
	until     time.Time
}

func (e physicalEgress) lookup(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !usablePhysicalIP(ip) {
			return nil, fmt.Errorf("拒绝非物理 IPv4 目标或 Fake-IP：%s", host)
		}
		return []net.IP{ip.To4()}, nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, err
	}
	key := e.stamp() + "/" + host
	physicalDNSCache.Lock()
	cached, ok := physicalDNSCache.entries[key]
	physicalDNSCache.Unlock()
	if ok && time.Now().Before(cached.until) {
		return cached.addresses, nil
	}
	var nonce [2]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(nonce[:]), RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	wire, err := query.Pack()
	if err != nil {
		return nil, err
	}
	var last error
	for _, point := range physicalDNSPoints {
		addresses, ttl, err := e.lookupAt(ctx, point, query, wire)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if ttl > 0 {
			physicalDNSCache.Lock()
			if len(physicalDNSCache.entries) >= 128 {
				physicalDNSCache.entries = map[string]physicalDNSResult{}
			}
			physicalDNSCache.entries[key] = physicalDNSResult{addresses, time.Now().Add(time.Duration(min(ttl, 300)) * time.Second)}
			physicalDNSCache.Unlock()
		}
		return addresses, nil
	}
	return nil, fmt.Errorf("物理网卡加密 DNS 解析失败（不回退系统 DNS）：%w", last)
}

func (e physicalEgress) lookupAt(parent context.Context, point physicalDNSPoint, query dnsmessage.Message, wire []byte) ([]net.IP, uint32, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, DisableKeepAlives: true, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return e.rawDial(ctx, "tcp4", net.JoinHostPort(point.ip, "443"), nil)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+point.host+"/dns-query?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-message")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("DoH HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, 0, err
	}
	var answer dnsmessage.Message
	if err = answer.Unpack(body); err != nil {
		return nil, 0, err
	}
	if !answer.Response || answer.ID != query.ID || answer.Truncated || answer.RCode != dnsmessage.RCodeSuccess || len(answer.Questions) != 1 || answer.Questions[0] != query.Questions[0] {
		return nil, 0, fmt.Errorf("DoH 返回无效 DNS 应答")
	}
	owners := map[string]bool{strings.ToLower(query.Questions[0].Name.String()): true}
	ttl := uint32(300)
	for round := 0; round < 16; round++ {
		changed := false
		for _, r := range answer.Answers {
			if c, ok := r.Body.(*dnsmessage.CNAMEResource); ok && owners[strings.ToLower(r.Header.Name.String())] {
				target := strings.ToLower(c.CNAME.String())
				if !owners[target] {
					owners[target] = true
					changed = true
				}
				ttl = min(ttl, r.Header.TTL)
			}
		}
		if !changed {
			break
		}
	}
	addresses := []net.IP{}
	for _, r := range answer.Answers {
		if a, ok := r.Body.(*dnsmessage.AResource); ok && r.Header.Class == dnsmessage.ClassINET && owners[strings.ToLower(r.Header.Name.String())] {
			ip := net.IP(a.A[:])
			if usablePhysicalIP(ip) {
				addresses = append(addresses, append(net.IP(nil), ip...))
				ttl = min(ttl, r.Header.TTL)
			}
		}
	}
	if len(addresses) == 0 {
		return nil, 0, fmt.Errorf("DoH 没有可用的真实 IPv4 地址")
	}
	return addresses, ttl, nil
}

func (e physicalEgress) dial(ctx context.Context, network, address string, template *net.Dialer) (net.Conn, error) {
	timeout := 8 * time.Second
	if template != nil && template.Timeout > 0 {
		timeout = template.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if template != nil && !template.Deadline.IsZero() {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, template.Deadline)
		defer deadlineCancel()
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := e.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		conn, err := e.rawDial(ctx, network, net.JoinHostPort(ip.String(), port), template)
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func providerDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	e, err := currentPhysicalEgressContext(ctx)
	if err != nil {
		return nil, err
	}
	return e.dial(ctx, network, address, nil)
}

// Rebuild the HTTP pool on an egress change; never reuse idle sockets belonging
// to the former physical network. Redirects retain the same strict dial path.
type physicalHTTPTransport struct {
	sync.Mutex
	stamp     string
	transport *http.Transport
}

func providerHTTPTransport() http.RoundTripper { return &physicalHTTPTransport{} }
func (p *physicalHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	e, err := currentPhysicalEgressContext(req.Context())
	if err != nil {
		return nil, err
	}
	p.Lock()
	if p.transport == nil || p.stamp != e.stamp() {
		if p.transport != nil {
			p.transport.CloseIdleConnections()
		}
		p.stamp = e.stamp()
		p.transport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 8 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return e.dial(ctx, network, address, nil)
		}}
	}
	tr := p.transport
	p.Unlock()
	return tr.RoundTrip(req)
}
