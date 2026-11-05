package main

// UDP mappings use an independent unordered channel per rule. TCP streams keep
// their existing reliable channel, so editing another rule does not replace it.
import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const udpMappingPrefix = "UULINK_UDP_MAP_V1:"
const udpMappingPayload = 65507

var udpMappingSockets = make(chan struct{}, 1024)

type udpMappingFlow struct {
	addr *net.UDPAddr
	conn *net.UDPConn
	last time.Time
	once sync.Once
}

func (f *udpMappingFlow) close() {
	f.once.Do(func() {
		if f.conn != nil {
			f.conn.Close()
			<-udpMappingSockets
		}
	})
}

type udpForward struct {
	ctx       context.Context
	cancel    context.CancelFunc
	dc        *webrtc.DataChannel
	local     *net.UDPConn
	target    *net.UDPAddr
	rule      forwardRule
	server    bool
	mu        sync.Mutex
	flows     map[uint64]*udpMappingFlow
	sources   map[string]uint64
	next      uint64
	ready     bool
	closed    bool
	lastAck   time.Time
	errorText string
	once      sync.Once
}

func newUDPForward(ctx context.Context, dc *webrtc.DataChannel) *udpForward {
	ctx, cancel := context.WithCancel(ctx)
	b := &udpForward{ctx: ctx, cancel: cancel, dc: dc, flows: map[uint64]*udpMappingFlow{}, sources: map[string]uint64{}}
	dc.OnClose(func() { b.close() })
	dc.OnError(func(e error) { b.fail("UDP 通道错误: " + e.Error()) })
	go func() { <-ctx.Done(); b.close() }()
	return b
}
func (b *udpForward) close() {
	b.once.Do(func() {
		b.cancel()
		b.mu.Lock()
		b.closed = true
		b.ready = false
		local, flows := b.local, b.flows
		b.flows = map[uint64]*udpMappingFlow{}
		b.sources = map[string]uint64{}
		b.mu.Unlock()
		if local != nil {
			local.Close()
		}
		for _, f := range flows {
			f.close()
		}
		go b.dc.Close()
	})
}
func (b *udpForward) fail(s string) { b.mu.Lock(); b.errorText = s; b.mu.Unlock(); b.close() }
func (b *udpForward) status() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.errorText != "" {
		return b.errorText, b.closed
	}
	if b.closed {
		return "UDP 通道已关闭，正在重试", true
	}
	if b.ready {
		return "UDP 就绪", false
	}
	return "UDP 正在等待远端确认", false
}
func (b *udpForward) send(raw []byte) {
	if b.ctx.Err() == nil && b.dc.ReadyState() == webrtc.DataChannelStateOpen && b.dc.BufferedAmount()+uint64(len(raw)) <= 256*1024 {
		_ = b.dc.Send(raw)
	}
}
func (b *udpForward) sendData(id uint64, p []byte) {
	if len(p) > udpMappingPayload {
		return
	}
	raw := make([]byte, 8+len(p))
	binary.BigEndian.PutUint64(raw, id)
	copy(raw[8:], p)
	b.send(raw)
}
func (b *udpForward) maintain() {
	start := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case now := <-t.C:
			var expired []*udpMappingFlow
			b.mu.Lock()
			for id, f := range b.flows {
				if now.Sub(f.last) > 60*time.Second {
					delete(b.flows, id)
					if f.addr != nil {
						delete(b.sources, f.addr.String())
					}
					expired = append(expired, f)
				}
			}
			last, ready := b.lastAck, b.ready
			b.mu.Unlock()
			for _, f := range expired {
				f.close()
			}
			if !b.server {
				if !ready && now.Sub(start) > 10*time.Second {
					b.fail("UDP 未获远端确认，请更新远端客户端")
					return
				}
				if ready && now.Sub(last) > 12*time.Second {
					b.fail("UDP 远端回应超时，正在重试")
					return
				}
				b.send([]byte("UP1"))
			}
		}
	}
}
func (b *udpForward) clientMessage(raw []byte) {
	if string(raw) == "UA1" {
		b.mu.Lock()
		b.ready = true
		b.lastAck = time.Now()
		b.mu.Unlock()
		return
	}
	if len(raw) > 3 && string(raw[:3]) == "UE1" {
		b.fail("UDP 远端目标不可用: " + string(raw[3:]))
		return
	}
	if len(raw) < 8 || len(raw) > 8+udpMappingPayload {
		return
	}
	b.mu.Lock()
	f := b.flows[binary.BigEndian.Uint64(raw)]
	if f != nil {
		f.last = time.Now()
	}
	local := b.local
	b.mu.Unlock()
	if f != nil && local != nil {
		local.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = local.WriteToUDP(raw[8:], f.addr)
	}
}
func (b *udpForward) readClient() {
	buf := make([]byte, 65535)
	for {
		n, addr, e := b.local.ReadFromUDP(buf)
		if e != nil {
			return
		}
		if n > udpMappingPayload || !ruleAllowsSource(b.rule, addr) {
			continue
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		if !b.ready {
			b.mu.Unlock()
			continue
		}
		id := b.sources[addr.String()]
		f := b.flows[id]
		if f == nil {
			if len(b.flows) >= 256 {
				b.mu.Unlock()
				continue
			}
			b.next++
			id = b.next
			f = &udpMappingFlow{addr: addr}
			b.flows[id] = f
			b.sources[addr.String()] = id
		}
		f.last = time.Now()
		b.mu.Unlock()
		b.sendData(id, buf[:n])
	}
}
func startUDPMapping(ctx context.Context, pc *webrtc.PeerConnection, r forwardRule) (*udpForward, error) {
	addr, e := net.ResolveUDPAddr("udp4", r.Listen)
	if e != nil {
		return nil, e
	}
	local, e := net.ListenUDP("udp4", addr)
	if e != nil {
		return nil, e
	}
	_ = local.SetReadBuffer(256 * 1024)
	_ = local.SetWriteBuffer(256 * 1024)
	ordered, retries := false, uint16(0)
	dc, e := pc.CreateDataChannel(udpMappingPrefix+base64.RawURLEncoding.EncodeToString([]byte(r.Target)), &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retries})
	if e != nil {
		local.Close()
		return nil, e
	}
	b := newUDPForward(ctx, dc)
	b.local = local
	b.rule = r
	dc.OnMessage(func(m webrtc.DataChannelMessage) { b.clientMessage(m.Data) })
	dc.OnOpen(func() { b.send([]byte("UP1")); go b.maintain() })
	go b.readClient()
	return b, nil
}
func acceptUDPMapping(peer *rtcPeer, dc *webrtc.DataChannel) {
	raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(dc.Label(), udpMappingPrefix))
	h, p, ae := net.SplitHostPort(string(raw))
	port, pe := strconv.Atoi(p)
	if e != nil || len(raw) > 512 || ae != nil || pe != nil || h == "" || port < 1 || port > 65535 || dc.Ordered() || dc.MaxRetransmits() == nil || *dc.MaxRetransmits() != 0 {
		go dc.Close()
		return
	}
	if peer.udpMappings.Add(1) > 256 {
		peer.udpMappings.Add(-1)
		go dc.Close()
		return
	}
	b := newUDPForward(peer.protocol.ctx, dc)
	b.server = true
	go func() { <-b.ctx.Done(); peer.udpMappings.Add(-1) }()
	dc.OnMessage(func(m webrtc.DataChannelMessage) { b.serverMessage(m.Data) })
	dc.OnOpen(func() {
		go func() {
			ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
			defer cancel()
			ips, e := net.DefaultResolver.LookupIP(ctx, "ip4", h)
			if e != nil || len(ips) == 0 {
				b.send([]byte("UE1无法解析目标 IPv4"))
				return
			}
			ip := ips[0]
			if ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
				b.send([]byte("UE1不支持的目标地址"))
				return
			}
			b.mu.Lock()
			b.target = &net.UDPAddr{IP: ip, Port: port}
			b.ready = true
			b.mu.Unlock()
			b.send([]byte("UA1"))
			go b.maintain()
		}()
	})
}
func (b *udpForward) serverMessage(raw []byte) {
	if string(raw) == "UP1" {
		b.mu.Lock()
		ready := b.ready
		b.mu.Unlock()
		if ready {
			b.send([]byte("UA1"))
		}
		return
	}
	if len(raw) < 8 || len(raw) > 8+udpMappingPayload {
		return
	}
	id := binary.BigEndian.Uint64(raw)
	if id == 0 {
		return
	}
	b.mu.Lock()
	if b.closed || !b.ready {
		b.mu.Unlock()
		return
	}
	f := b.flows[id]
	if f == nil {
		if len(b.flows) >= 256 {
			b.mu.Unlock()
			return
		}
		select {
		case udpMappingSockets <- struct{}{}:
		default:
			b.mu.Unlock()
			return
		}
		conn, e := net.DialUDP("udp4", nil, b.target)
		if e != nil {
			<-udpMappingSockets
			b.mu.Unlock()
			return
		}
		_ = conn.SetReadBuffer(256 * 1024)
		_ = conn.SetWriteBuffer(256 * 1024)
		f = &udpMappingFlow{conn: conn}
		b.flows[id] = f
		go b.readServer(id, f)
	}
	f.last = time.Now()
	b.mu.Unlock()
	f.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = f.conn.Write(raw[8:])
}
func (b *udpForward) readServer(id uint64, f *udpMappingFlow) {
	defer func() {
		b.mu.Lock()
		if b.flows[id] == f {
			delete(b.flows, id)
		}
		b.mu.Unlock()
		f.close()
	}()
	buf := make([]byte, 65535)
	for {
		n, e := f.conn.Read(buf)
		if e != nil {
			return
		}
		b.mu.Lock()
		f.last = time.Now()
		b.mu.Unlock()
		b.sendData(id, buf[:n])
	}
}

func (r forwardRule) transport() string {
	if r.Protocol == "" {
		return "tcp"
	}
	return r.Protocol
}
func (r forwardRule) lanAllowed() bool {
	if r.AllowLAN != nil {
		return *r.AllowLAN
	}
	h, _, _ := net.SplitHostPort(r.Listen)
	return !net.ParseIP(h).IsLoopback()
}
func ruleAllowsSource(r forwardRule, a net.Addr) bool {
	// Legacy CLI rules retain the access implied by their explicit bind address.
	if r.AllowLAN == nil {
		return true
	}
	h, _, e := net.SplitHostPort(a.String())
	ip := net.ParseIP(h)
	if e != nil || ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if !r.lanAllowed() {
		return false
	}
	// Private/link-local sources include routed home LANs and VPN LANs. Public
	// peers are accepted only if they are on an actual directly attached subnet.
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		_, n, e := net.ParseCIDR(a.String())
		if e == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
