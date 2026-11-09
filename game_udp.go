package main

// Experimental fixed-target UDP transport. SS continues to authenticate and
// encrypt datagrams; the target is chosen by the server operator, never a peer.
import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
)

const gameUDPLabel = "UULINK_UDP_V2"
const gameMaxPayload = 16384
const gameMaxFlows = 512
const gameBufferLimit = 32 * 1024

const (
	dropQueue = iota
	dropRate
	dropSend
	dropOversize
	dropInvalid
	dropCapacity
	dropSocket
	dropSource
	dropNotReady
	dropUnknownFlow
	dropReasonCount
)

var gameUDPListen, gameUDPAllowedIP string
var gameUDPCopies = 2

type gameUDPFlow struct {
	id   uint64
	addr *net.UDPAddr
	conn *net.UDPConn
	last atomic.Int64
}

type gameUDPBridge struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	dc                      *webrtc.DataChannel
	local                   *net.UDPConn
	target                  *net.UDPAddr
	allow                   net.IP
	mu                      sync.Mutex
	sendMu                  sync.Mutex
	flows                   map[uint64]*gameUDPFlow
	sources                 map[string]uint64
	next                    uint64
	ready                   atomic.Bool
	sent, received, dropped atomic.Uint64
	dropReasons             [dropReasonCount]atomic.Uint64
	bufferPeak              atomic.Uint64
	// Every channel message is charged to the per-connection payload bucket.
	window                                       time.Time
	windowBytes                                  int
	loadWindow                                   time.Time
	loadBytes                                    int
	copyHoldUntil                                time.Time
	packetSeq                                    atomic.Uint64
	dedupMu                                      sync.Mutex
	seen                                         map[uint64]struct{}
	seenRing                                     [4096]uint64
	seenPos                                      int
	copies                                       int
	repeatSlots                                  chan struct{}
	duplicateReceived, repeatSent, repeatSkipped atomic.Uint64
}

func newGameUDPBridge(ctx context.Context, dc *webrtc.DataChannel) *gameUDPBridge {
	ctx, cancel := context.WithCancel(ctx)
	return &gameUDPBridge{ctx: ctx, cancel: cancel, dc: dc, flows: map[uint64]*gameUDPFlow{}, sources: map[string]uint64{}, seen: map[uint64]struct{}{}, copies: gameUDPCopies, repeatSlots: make(chan struct{}, 128)}
}

func (b *gameUDPBridge) close() {
	b.cancel()
	b.mu.Lock()
	if b.local != nil {
		b.local.Close()
	}
	for _, f := range b.flows {
		if f.conn != nil {
			f.conn.Close()
		}
	}
	b.flows = map[uint64]*gameUDPFlow{}
	b.sources = map[string]uint64{}
	b.mu.Unlock()
}

func (b *gameUDPBridge) drop(reason int) {
	b.dropped.Add(1)
	b.dropReasons[reason].Add(1)
}

func (b *gameUDPBridge) observeBuffer() {
	n := b.dc.BufferedAmount()
	for old := b.bufferPeak.Load(); n > old; old = b.bufferPeak.Load() {
		if b.bufferPeak.CompareAndSwap(old, n) {
			break
		}
	}
}

func (b *gameUDPBridge) send(raw []byte) error {
	return b.sendBudget(raw, false)
}

func (b *gameUDPBridge) sendBudget(raw []byte, repeat bool) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.ctx.Err() != nil {
		return b.ctx.Err()
	}
	if time.Since(b.window) >= time.Second {
		b.window = time.Now()
		b.windowBytes = 0
	}
	// Optional copies may use only an uncongested queue. Original datagrams
	// retain the full budget; skipped copies are not lost original datagrams.
	if repeat && (time.Now().Before(b.copyHoldUntil) || b.dc.BufferedAmount()+uint64(len(raw)) > 4096 || b.windowBytes+len(raw) > 1024*1024) {
		b.repeatSkipped.Add(1)
		return fmt.Errorf("optional UDP copy suppressed")
	}
	// Bound memory/egress for this experimental game-only endpoint to 8 Mbit/s.
	b.observeBuffer()
	if b.dc.BufferedAmount()+uint64(len(raw)) > gameBufferLimit {
		b.drop(dropQueue)
		return fmt.Errorf("UDP buffer budget exceeded")
	}
	if b.windowBytes+len(raw) > 1024*1024 {
		b.drop(dropRate)
		return fmt.Errorf("UDP rate budget exceeded")
	}
	b.windowBytes += len(raw)
	if err := b.dc.Send(raw); err != nil {
		b.drop(dropSend)
		return err
	}
	b.observeBuffer()
	b.sent.Add(1)
	return nil
}

func (b *gameUDPBridge) duplicate(seq uint64) bool {
	b.dedupMu.Lock()
	defer b.dedupMu.Unlock()
	if _, ok := b.seen[seq]; ok {
		b.duplicateReceived.Add(1)
		return true
	}
	old := b.seenRing[b.seenPos]
	if old != 0 {
		delete(b.seen, old)
	}
	b.seenRing[b.seenPos] = seq
	b.seen[seq] = struct{}{}
	b.seenPos = (b.seenPos + 1) % len(b.seenRing)
	return false
}

func (b *gameUDPBridge) allowRepeat(size int) bool {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	now := time.Now()
	if now.Sub(b.loadWindow) >= 100*time.Millisecond {
		b.loadWindow = now
		b.loadBytes = 0
	}
	b.loadBytes += size
	if b.loadBytes > 4096 {
		b.copyHoldUntil = now.Add(time.Second)
	}
	if now.Before(b.copyHoldUntil) {
		b.repeatSkipped.Add(1)
		return false
	}
	return true
}

func (b *gameUDPBridge) sendData(id uint64, payload []byte) {
	if len(payload) > gameMaxPayload {
		b.drop(dropOversize)
		return
	}
	raw := make([]byte, 20+len(payload))
	copy(raw, "UGD2")
	binary.BigEndian.PutUint64(raw[4:12], id)
	binary.BigEndian.PutUint64(raw[12:20], b.packetSeq.Add(1))
	copy(raw[20:], payload)
	started := time.Now()
	if err := b.send(raw); err != nil {
		return
	}
	if b.copies < 2 || !b.allowRepeat(len(raw)) {
		return
	}
	select {
	case b.repeatSlots <- struct{}{}:
	default:
		b.repeatSkipped.Add(1)
		return
	}
	time.AfterFunc(3*time.Millisecond, func() {
		defer func() { <-b.repeatSlots }()
		if b.ctx.Err() != nil {
			return
		}
		if time.Since(started) > 15*time.Millisecond {
			b.repeatSkipped.Add(1)
			return
		}
		if err := b.sendBudget(raw, true); err == nil {
			b.repeatSent.Add(1)
		}
	})
}

func (b *gameUDPBridge) maintain() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
			b.mu.Lock()
			for id, f := range b.flows {
				if time.Now().UnixNano()-f.last.Load() > int64(60*time.Second) {
					if f.conn != nil {
						f.conn.Close()
					}
					if f.addr != nil {
						delete(b.sources, f.addr.String())
					}
					delete(b.flows, id)
				}
			}
			n := len(b.flows)
			b.mu.Unlock()
			log.Printf("game UDP flows=%d sent=%d received=%d dropped=%d buffered=%d buffer_peak=%d drop_queue=%d drop_rate=%d drop_send=%d drop_oversize=%d drop_invalid=%d drop_capacity=%d drop_socket=%d drop_source=%d drop_not_ready=%d drop_unknown_flow=%d dup_received=%d repeat_sent=%d repeat_skipped=%d", n, b.sent.Load(), b.received.Load(), b.dropped.Load(), b.dc.BufferedAmount(), b.bufferPeak.Load(), b.dropReasons[dropQueue].Load(), b.dropReasons[dropRate].Load(), b.dropReasons[dropSend].Load(), b.dropReasons[dropOversize].Load(), b.dropReasons[dropInvalid].Load(), b.dropReasons[dropCapacity].Load(), b.dropReasons[dropSocket].Load(), b.dropReasons[dropSource].Load(), b.dropReasons[dropNotReady].Load(), b.dropReasons[dropUnknownFlow].Load(), b.duplicateReceived.Load(), b.repeatSent.Load(), b.repeatSkipped.Load())
		}
	}
}

func acceptGameUDP(peer *rtcPeer, dc *webrtc.DataChannel) {
	target := os.Getenv("UULINK_GAME_UDP_TARGET")
	a, err := net.ResolveUDPAddr("udp4", target)
	// Deliberately expose only an explicitly configured loopback SS/probe endpoint.
	if target == "" || err != nil || a.IP == nil || !a.IP.IsLoopback() || a.Port < 1 || dc.Ordered() || dc.MaxRetransmits() == nil || *dc.MaxRetransmits() != 0 {
		log.Printf("game UDP rejected: disabled, invalid target or channel reliability")
		go dc.Close()
		return
	}
	b := newGameUDPBridge(peer.protocol.ctx, dc)
	b.target = a
	if raw := os.Getenv("UULINK_GAME_UDP_COPIES"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 2 {
			b.close()
			go dc.Close()
			return
		}
		b.copies = n
	}
	peer.mu.Lock()
	if peer.closed || peer.gameUDP != nil {
		peer.mu.Unlock()
		b.close()
		go dc.Close()
		return
	}
	peer.gameUDP = b
	peer.mu.Unlock()
	dc.OnClose(b.close)
	dc.OnMessage(func(m webrtc.DataChannelMessage) { b.serverMessage(m.Data) })
	dc.OnOpen(func() { log.Printf("game UDP server open target=%s unordered=true retransmits=0", a); go b.maintain() })
}

func (b *gameUDPBridge) serverMessage(raw []byte) {
	if len(raw) == 4 && string(raw) == "UGH2" {
		b.send([]byte("UGA2"))
		return
	}
	if len(raw) == 12 && string(raw[:4]) == "UGP2" {
		reply := append([]byte(nil), raw...)
		copy(reply, "UGQ2")
		b.send(reply)
		return
	}
	if len(raw) < 20 || len(raw) > 20+gameMaxPayload || string(raw[:4]) != "UGD2" {
		b.drop(dropInvalid)
		return
	}
	id := binary.BigEndian.Uint64(raw[4:12])
	seq := binary.BigEndian.Uint64(raw[12:20])
	if seq == 0 {
		b.drop(dropInvalid)
		return
	}
	if b.duplicate(seq) {
		return
	}
	if id == 0 {
		b.drop(dropInvalid)
		return
	}
	b.mu.Lock()
	if b.ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	f := b.flows[id]
	if f == nil {
		if len(b.flows) >= gameMaxFlows {
			b.mu.Unlock()
			b.drop(dropCapacity)
			return
		}
		conn, err := net.DialUDP("udp4", nil, b.target)
		if err != nil {
			b.mu.Unlock()
			b.drop(dropSocket)
			return
		}
		f = &gameUDPFlow{id: id, conn: conn}
		b.flows[id] = f
		go b.readServer(f)
	}
	f.last.Store(time.Now().UnixNano())
	b.mu.Unlock()
	f.conn.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
	if _, err := f.conn.Write(raw[20:]); err != nil {
		b.drop(dropSocket)
	} else {
		b.received.Add(1)
	}
}

func (b *gameUDPBridge) readServer(f *gameUDPFlow) {
	defer func() {
		f.conn.Close()
		b.mu.Lock()
		if b.flows[f.id] == f {
			delete(b.flows, f.id)
		}
		b.mu.Unlock()
	}()
	buf := make([]byte, 65535)
	for {
		n, err := f.conn.Read(buf)
		if err != nil {
			return
		}
		f.last.Store(time.Now().UnixNano())
		b.sendData(f.id, buf[:n])
	}
}

func startGameUDPClient(c *controllerClient) error {
	ordered := false
	retries := uint16(0)
	dc, err := c.pc.CreateDataChannel(gameUDPLabel, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retries})
	if err != nil {
		return err
	}
	b := newGameUDPBridge(c.s.ctx, dc)
	b.allow = net.ParseIP(gameUDPAllowedIP)
	c.gameUDP = b
	dc.OnClose(func() { b.close(); c.s.cancel() })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		raw := m.Data
		if len(raw) == 4 && string(raw) == "UGA2" {
			if b.ready.CompareAndSwap(false, true) {
				log.Printf("game UDP capability acknowledged")
			}
			return
		}
		if len(raw) == 12 && string(raw[:4]) == "UGQ2" {
			ns := int64(binary.BigEndian.Uint64(raw[4:]))
			log.Printf("game UDP tunnel RTT=%.3fms", float64(time.Now().UnixNano()-ns)/1e6)
			return
		}
		if len(raw) < 20 || len(raw) > gameMaxPayload+20 || string(raw[:4]) != "UGD2" {
			b.drop(dropInvalid)
			return
		}
		id := binary.BigEndian.Uint64(raw[4:12])
		seq := binary.BigEndian.Uint64(raw[12:20])
		if seq == 0 {
			b.drop(dropInvalid)
			return
		}
		if b.duplicate(seq) {
			return
		}
		b.mu.Lock()
		f := b.flows[id]
		conn := b.local
		b.mu.Unlock()
		if f == nil || conn == nil {
			b.drop(dropUnknownFlow)
			return
		}
		f.last.Store(time.Now().UnixNano())
		conn.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
		if _, err := conn.WriteToUDP(raw[20:], f.addr); err != nil {
			b.drop(dropSocket)
		} else {
			b.received.Add(1)
		}
	})
	dc.OnOpen(func() {
		a, err := net.ResolveUDPAddr("udp4", gameUDPListen)
		if err != nil {
			log.Printf("game UDP listen: %v", err)
			c.s.cancel()
			return
		}
		conn, err := net.ListenUDP("udp4", a)
		if err != nil {
			log.Printf("game UDP listen: %v", err)
			c.s.cancel()
			return
		}
		b.mu.Lock()
		if b.ctx.Err() != nil {
			b.mu.Unlock()
			conn.Close()
			return
		}
		b.local = conn
		b.mu.Unlock()
		c.ready.Store(true)
		log.Printf("game UDP client listening=%s allow=%s unordered=true retransmits=0", a, b.allow)
		go b.maintain()
		go b.readClient()
		go b.probe()
	})
	return nil
}

func (b *gameUDPBridge) probe() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if !b.ready.Load() {
			b.send([]byte("UGH2"))
		} else {
			raw := make([]byte, 12)
			copy(raw, "UGP2")
			binary.BigEndian.PutUint64(raw[4:], uint64(time.Now().UnixNano()))
			b.send(raw)
		}
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *gameUDPBridge) readClient() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := b.local.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !addr.IP.IsLoopback() && !addr.IP.Equal(b.allow) && !addr.IP.Equal(b.local.LocalAddr().(*net.UDPAddr).IP) {
			b.drop(dropSource)
			continue
		}
		if n > gameMaxPayload {
			b.drop(dropOversize)
			continue
		}
		if !b.ready.Load() {
			b.drop(dropNotReady)
			continue
		}
		b.mu.Lock()
		id := b.sources[addr.String()]
		f := b.flows[id]
		if f == nil {
			if len(b.flows) >= gameMaxFlows {
				b.mu.Unlock()
				b.drop(dropCapacity)
				continue
			}
			b.next++
			id = b.next
			f = &gameUDPFlow{id: id, addr: addr}
			b.flows[id] = f
			b.sources[addr.String()] = id
			log.Printf("game UDP source=%s flow=%d", addr, id)
		}
		f.last.Store(time.Now().UnixNano())
		b.mu.Unlock()
		b.sendData(id, buf[:n])
	}
}

func gameUDPConnectCLI(args []string) error {
	f := flag.NewFlagSet("udp-connect", flag.ContinueOnError)
	config := f.String("config", "", "private controller configuration")
	device := f.String("device", "", "controlled device ID")
	listen := f.String("listen", "127.0.0.1:38443", "UDP listener")
	allow := f.String("allow-client", "127.0.0.1", "additional allowed source IP")
	relay := f.Bool("relay", true, "require UU TURN relay")
	logPath := f.String("log-file", "", "bounded log file")
	copies := f.Int("udp-copies", 2, "1 or up to 2 copies per datagram; optional duplicate delayed by 3ms")
	if err := f.Parse(args); err != nil {
		return err
	}
	h, p, err := net.SplitHostPort(*listen)
	port, e := strconv.Atoi(p)
	if err != nil || e != nil || port < 1 || port > 65535 || net.ParseIP(h) == nil || net.ParseIP(*allow) == nil || *device == "" || *config == "" {
		return fmt.Errorf("valid --config, --device, --listen IP:port and --allow-client required")
	}
	if *logPath != "" {
		w, err := openRotatingLog(*logPath)
		if err != nil {
			return err
		}
		defer w.Close()
		log.SetOutput(w)
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	if *copies < 1 || *copies > 2 {
		return fmt.Errorf("udp-copies must be 1 or 2")
	}
	gameUDPCopies = *copies
	gameUDPListen = *listen
	gameUDPAllowedIP = *allow
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for ctx.Err() == nil {
		err = loadAgentConfig(*config)
		if err == nil {
			err = runControllerRules(ctx, *device, *relay, func() ([]forwardRule, error) {
				return []forwardRule{{ID: "ss-game", Listen: *listen, Target: "127.0.0.1:28443", Enabled: true}}, nil
			}, nil)
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("game UDP session ended: %v; retry in 5s", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}
