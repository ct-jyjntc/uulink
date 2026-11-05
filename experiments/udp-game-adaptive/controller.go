package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

func (s *signalingSession) uploadStatus(ctx context.Context) error {
	if s.controller != nil {
		return nil
	}
	return uploadApp(ctx)
}

func (s *signalingSession) peerCount() int {
	n := len(s.peers)
	if s.controller != nil && s.controller.pc != nil {
		n++
	}
	return n
}

type controllerClient struct {
	networkCancel           context.CancelFunc
	ready                   atomic.Bool
	s                       *signalingSession
	publisher, appID, iceID string
	relayOnly               bool
	requested               bool
	pc                      *webrtc.PeerConnection
	pending                 bool
	candidates              []webrtc.ICECandidateInit
	gameUDP                 *gameUDPBridge
	mappings                *controllerMappings
	rules                   func() ([]forwardRule, error)
	report                  func(map[string]string)
	latency                 func(time.Duration)
}

func connectCLI(args []string) error {
	f := flag.NewFlagSet("connect", flag.ContinueOnError)
	config := f.String("config", "/etc/uulink-controller/config.json", "controller login configuration")
	device := f.String("device", "", "target UU device ID")
	listen := f.String("listen", "127.0.0.1:28006", "local TCP listening address")
	target := f.String("target", "127.0.0.1:22", "TCP destination on controlled device")
	relay := f.Bool("relay", false, "force WebRTC TURN relay")
	once := f.Bool("once", false, "exit when the session ends")
	logPath := f.String("log-file", "", "bounded log file (1 MiB x 2)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *device == "" || strings.ContainsAny(*device, "/?# ") {
		return fmt.Errorf("valid --device is required")
	}
	for _, addr := range []string{*listen, *target} {
		h, p, e := net.SplitHostPort(addr)
		if e != nil {
			return e
		}
		n, e := strconv.Atoi(p)
		if h == "" || e != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid TCP address %q", addr)
		}
	}
	if *logPath != "" {
		w, e := openRotatingLog(*logPath)
		if e != nil {
			return e
		}
		defer w.Close()
		log.SetOutput(w)
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	resourcesConfigured := false
	for ctx.Err() == nil {
		// Re-read credentials on every fresh session, so completing login or
		// repairing the config also recovers without restarting the controller.
		err := loadAgentConfig(*config)
		if err == nil {
			if !resourcesConfigured {
				configureResources(runtimeConfig)
				resourcesConfigured = true
			}
			err = runController(ctx, *device, *listen, *target, *relay)
		}
		if ctx.Err() != nil {
			return nil
		}
		if *once {
			return err
		}
		log.Printf("controller session ended: %v; retry in 5s", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

func runController(ctx context.Context, device, listen, target string, relay bool) error {
	return runControllerRules(ctx, device, relay, func() ([]forwardRule, error) {
		return []forwardRule{{ID: "single", Listen: listen, Target: target, Enabled: true}}, nil
	}, nil)
}
func runControllerRules(ctx context.Context, device string, relay bool, rules func() ([]forwardRule, error), report func(map[string]string), latency ...func(time.Duration)) error {
	var room signalingRoom
	if err := api(ctx, controllerJoinPrefix+device, []byte(`{"force_join":false}`), &room); err != nil {
		return err
	}
	if room.Token == "" || room.Server == "" {
		return fmt.Errorf("room/join missing token/server")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &signalingSession{ctx: ctx, cancel: cancel, started: time.Now()}
	c := &controllerClient{s: s, rules: rules, report: report, relayOnly: relay, appID: strings.ReplaceAll(uuid.NewString(), "-", ""), iceID: strings.ReplaceAll(uuid.NewString(), "-", "")}
	if len(latency) > 0 {
		c.latency = latency[0]
	}
	s.controller = c
	timer := time.AfterFunc(60*time.Second, func() {
		if !c.ready.Load() {
			log.Printf("controller connection did not become ready within 60s")
			cancel()
		}
	})
	defer timer.Stop()
	defer s.close()
	return s.run(room, time.Minute, 2*time.Second)
}

func signalAck(raw string) (string, json.RawMessage, error) {
	var a []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", nil, err
	}
	if len(a) == 1 {
		return "success", a[0], nil
	}
	if len(a) < 2 {
		return "", nil, fmt.Errorf("short signaling acknowledgement")
	}
	var status string
	if err := json.Unmarshal(a[0], &status); err != nil {
		return "", nil, err
	}
	return status, a[1], nil
}

func (c *controllerClient) roomInfo(raw string) error {
	status, data, err := signalAck(raw)
	if err != nil {
		return err
	}
	if status != "success" && status != "ok" {
		return fmt.Errorf("room information rejected: %s", status)
	}
	var info struct {
		Publisher struct {
			ClientID string `json:"client_id"`
		} `json:"publisher"`
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return err
	}
	log.Printf("controller room status=%s publisher=%s", status, info.Publisher.ClientID)
	if c.requested {
		return nil
	}
	if info.Publisher.ClientID == "" {
		return fmt.Errorf("room has no publisher")
	}
	c.publisher = info.Publisher.ClientID
	c.requested = true
	options := putVar(nil, 1, 0)
	options = putVar(options, 8, 4)
	options = putBytes(options, 9, []byte(runtimeConfig.DeviceID))
	options = putBytes(options, 12, []byte("4.41.2"))
	payload := map[string]any{"app_control_id": c.appID, "app_data": map[string]any{"_placeholder": true, "num": 0}, "streamer_data": "{}", "geo_exit_ip": ""}
	b, _ := json.Marshal(payload)
	return c.s.sendSDP([]byte(`451-3["control",`+string(b)+`]`), append([]byte{4}, options...))
}

func (c *controllerClient) controlReply(raw string) error {
	status, data, err := signalAck(raw)
	if err != nil {
		return err
	}
	var r struct {
		Code       int              `json:"code"`
		ClientID   string           `json:"client_id"`
		IceID      string           `json:"ice_id"`
		ForceRelay bool             `json:"force_relay"`
		IceServers []map[string]any `json:"iceServers"`
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return err
	}
	log.Printf("control reply status=%s code=%d client=%s ICE=%d", status, r.Code, r.ClientID, len(r.IceServers))
	if status != "success" && status != "ok" {
		return fmt.Errorf("control rejected: status=%s code=%d", status, r.Code)
	}
	if r.Code != 0 {
		return fmt.Errorf("control code=%d", r.Code)
	}
	if r.ClientID != "" {
		c.publisher = r.ClientID
	}
	if c.pc != nil {
		return nil
	}
	if r.IceID != "" {
		c.iceID = r.IceID
	}
	policy := webrtc.ICETransportPolicyAll
	if c.relayOnly || r.ForceRelay {
		policy = webrtc.ICETransportPolicyRelay
	}
	pc, networkCancel, err := newProviderPeerConnection(c.s.ctx, webrtc.Configuration{ICEServers: buildICEServers(r.IceServers), ICETransportPolicy: policy})
	if err != nil {
		return err
	}
	c.pc = pc
	c.networkCancel = networkCancel
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("controller WebRTC state=%s", st)
		if st == webrtc.PeerConnectionStateFailed {
			c.s.cancel()
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		if st == webrtc.ICEConnectionStateConnected {
			p, e := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
			if e == nil && p != nil {
				log.Printf("controller ICE selected local=%s remote=%s", p.Local, p.Remote)
			}
		}
	})
	pc.OnICECandidate(func(v *webrtc.ICECandidate) {
		if v != nil {
			if e := c.soac(map[string]any{"type": "candidate", "candidate": v.ToJSON()}); e != nil {
				log.Printf("controller candidate: %v", e)
			}
		}
	})
	if gameUDPListen != "" {
		if err := startGameUDPClient(c); err != nil {
			return err
		}
	}
	{
		dc, err := pc.CreateDataChannel("FILE_DATA_CHANNEL", nil)
		if err != nil {
			return err
		}
		c.mappings = newControllerMappings(c.s.ctx, dc)
		c.mappings.onLatency = c.latency
		c.mappings.udpFactory = func(r forwardRule) (*udpForward, error) { return startUDPMapping(c.s.ctx, pc, r) }
		dc.OnMessage(func(m webrtc.DataChannelMessage) { c.mappings.handle(m.Data) })
		dc.OnOpen(func() {
			c.ready.Store(true)
			log.Printf("controller data channel open")
			c.mappings.start(c.rules, c.report)
			c.mappings.startProbes()
		})
		dc.OnClose(func() { c.mappings.close(); c.s.cancel() })
		dc.OnError(func(e error) { log.Printf("controller data channel: %v", e) })
	}
	o, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = pc.SetLocalDescription(o); err != nil {
		return err
	}
	return c.soac(map[string]any{"type": "offer", "sdp": o.SDP})
}

func (c *controllerClient) soac(data map[string]any) error {
	data["app_control_id"] = c.appID
	data["ice_id"] = c.iceID
	b, err := json.Marshal(map[string]any{"client_id": c.publisher, "data": data})
	if err != nil {
		return err
	}
	return c.s.text(`42["soac",` + string(b) + `]`)
}
func (c *controllerClient) event(event string, raw json.RawMessage) error {
	switch event {
	case "soac":
		var o signalOffer
		if e := json.Unmarshal(raw, &o); e != nil {
			return e
		}
		if o.Data.IceID != c.iceID {
			return nil
		}
		if c.pc == nil {
			return fmt.Errorf("SOAC before peer creation")
		}
		switch o.Data.Type {
		case "answer":
			if o.Data.SDP != "" {
				return c.answer(o.Data.SDP)
			}
			c.pending = true
		case "candidate":
			if c.pc.RemoteDescription() == nil {
				c.candidates = append(c.candidates, o.Data.Candidate)
			} else {
				return c.pc.AddICECandidate(o.Data.Candidate)
			}
		}
	case "released", "left":
		var leaving struct {
			IceID    string `json:"ice_id"`
			ClientID string `json:"client_id"`
		}
		if err := json.Unmarshal(raw, &leaving); err != nil {
			return err
		}
		if leaving.IceID != "" && leaving.IceID != c.iceID {
			return nil
		}
		if leaving.IceID == "" && leaving.ClientID != "" && leaving.ClientID != c.publisher {
			return nil
		}
		return fmt.Errorf("controller received %s", event)
	}
	return nil
}
func (c *controllerClient) binary(b []byte) error {
	if !c.pending {
		return nil
	}
	c.pending = false
	if len(b) > 0 && b[0] == 4 {
		b = b[1:]
	}
	z, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return e
	}
	defer z.Close()
	sdp, e := io.ReadAll(io.LimitReader(z, (2<<20)+1))
	if e != nil {
		return e
	}
	if len(sdp) > 2<<20 {
		return fmt.Errorf("SDP too large")
	}
	return c.answer(string(sdp))
}
func (c *controllerClient) answer(sdp string) error {
	if e := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); e != nil {
		return e
	}
	for _, v := range c.candidates {
		if e := c.pc.AddICECandidate(v); e != nil {
			return e
		}
	}
	c.candidates = nil
	log.Printf("controller answer accepted")
	return nil
}
func (c *controllerClient) close() {
	if c.networkCancel != nil {
		c.networkCancel()
	}
	if c.gameUDP != nil {
		c.gameUDP.close()
	}
	if c.mappings != nil {
		c.mappings.close()
	}
	if c.pc != nil {
		c.pc.Close()
	}
}

type forwardStream struct {
	conn   net.Conn
	frames chan pmFrame
	done   chan struct{}
	once   sync.Once
}
type pmForwarder struct {
	ctx         context.Context
	cancel      context.CancelFunc
	sender      pmSender
	target      string
	mu          sync.Mutex
	streams     map[pmKey]*forwardStream
	listener    net.Listener
	closed      bool
	allowSource func(net.Addr) bool
	seq         atomic.Uint64
	streamID    atomic.Uint64
	session     uint64
}

var forwardSessionID atomic.Uint64

func newPMForwarder(ctx context.Context, sender pmSender, target string) *pmForwarder {
	ctx, cancel := context.WithCancel(ctx)
	return &pmForwarder{ctx: ctx, cancel: cancel, sender: sender, target: target, streams: map[pmKey]*forwardStream{}, session: forwardSessionID.Add(1)}
}
func (r *pmForwarder) send(k pmKey, kind uint64, b []byte) error {
	if dc, ok := r.sender.(interface{ BufferedAmount() uint64 }); ok && kind == pmDATA {
		deadline := time.Now().Add(30 * time.Second)
		for dc.BufferedAmount() > 1<<20 {
			if time.Now().After(deadline) {
				return fmt.Errorf("mapping send timeout")
			}
			select {
			case <-r.ctx.Done():
				return r.ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return r.sender.Send(encodePM(pmFrame{key: k, seq: r.seq.Add(1), ts: uint64(time.Now().Unix()), kind: kind, payload: b}))
}
func (r *pmForwarder) listen(addr string) error {
	if e := r.bind(addr); e != nil {
		return e
	}
	return r.accept()
}
func (r *pmForwarder) bind(addr string) error {
	l, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		l.Close()
		return nil
	}
	r.listener = l
	r.mu.Unlock()
	log.Printf("mapping ready listen=%s target=%s", l.Addr(), r.target)
	return nil
}
func (r *pmForwarder) accept() error {
	r.mu.Lock()
	l := r.listener
	r.mu.Unlock()
	if l == nil {
		return nil
	}
	for {
		conn, e := l.Accept()
		if e != nil {
			if r.ctx.Err() != nil {
				return nil
			}
			return e
		}
		if r.allowSource != nil && !r.allowSource(conn.RemoteAddr()) {
			conn.Close()
			continue
		}
		go r.serveConn(conn)
	}
}
func (r *pmForwarder) drop(k pmKey, s *forwardStream, notify bool) {
	s.once.Do(func() {
		close(s.done)
		s.conn.Close()
		r.mu.Lock()
		delete(r.streams, k)
		r.mu.Unlock()
		if notify {
			r.send(k, pmFIN, nil)
		}
	})
}
func (r *pmForwarder) serveConn(conn net.Conn) {
	k := pmKey{session: r.session, rule: 1, stream: r.streamID.Add(1)}
	s := &forwardStream{conn: conn, frames: make(chan pmFrame, 64), done: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		conn.Close()
		return
	}
	r.streams[k] = s
	r.mu.Unlock()
	defer r.drop(k, s, true)
	h, p, _ := net.SplitHostPort(r.target)
	port, _ := strconv.Atoi(p)
	b, _ := json.Marshal(map[string]any{"version": 1, "target_host": h, "target_port": port})
	if e := r.send(k, pmSYN, b); e != nil {
		return
	}
	select {
	case <-r.ctx.Done():
		return
	case <-s.done:
		return
	case <-time.After(12 * time.Second):
		return
	case f := <-s.frames:
		var a struct {
			OK bool `json:"ok"`
		}
		if f.kind != pmSYNACK || json.Unmarshal(f.payload, &a) != nil || !a.OK {
			log.Printf("mapping target refused stream=%d", k.stream)
			return
		}
	}
	go func() {
		defer r.drop(k, s, true)
		buf := make([]byte, 16*1024)
		for {
			n, e := conn.Read(buf)
			if n > 0 {
				if r.send(k, pmDATA, buf[:n]) != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-s.done:
			return
		case f := <-s.frames:
			switch f.kind {
			case pmFIN:
				return
			case pmDATA:
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if _, e := io.Copy(conn, bytes.NewReader(f.payload)); e != nil {
					return
				}
				if r.send(k, pmDATAACK, nil) != nil {
					return
				}
			}
		}
	}
}
func (r *pmForwarder) handle(b []byte) {
	f, e := decodePM(b)
	if e != nil || f.kind == pmDATAACK {
		return
	}
	r.handleFrame(f)
}
func (r *pmForwarder) handleFrame(f pmFrame) {
	f.payload = bytes.Clone(f.payload)
	r.mu.Lock()
	s := r.streams[f.key]
	r.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.frames <- f:
	case <-s.done:
	default:
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case s.frames <- f:
		case <-s.done:
		case <-r.ctx.Done():
		case <-timer.C:
			r.drop(f.key, s, true)
		}
	}
}
func (r *pmForwarder) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.cancel()
	l := r.listener
	all := make(map[pmKey]*forwardStream, len(r.streams))
	for k, s := range r.streams {
		all[k] = s
	}
	r.mu.Unlock()
	if l != nil {
		l.Close()
	}
	for k, s := range all {
		r.drop(k, s, true)
	}
}
