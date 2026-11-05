package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var apiClient = &http.Client{Timeout: 15 * time.Second, Transport: providerHTTPTransport()}
var linuxBearer string

type providerAPIError struct {
	path         string
	status, code int
	message      string
}

func (e *providerAPIError) Error() string {
	return fmt.Sprintf("%s HTTP=%d code=%d msg=%s", e.path, e.status, e.code, e.message)
}

func api(ctx context.Context, path string, body []byte, out any) error {
	return apiRequest(ctx, "POST", path, body, out)
}

func apiRequest(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://api.nrd.nie.163.com"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	p := standardParams(runtimeConfig.DeviceID, runtimeConfig.ClientID)
	for k, v := range p {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Param-SIGN", nrdSign(method, path, p, body))
	if linuxBearer != "" {
		req.Header.Set("Authorization", "Bearer "+linuxBearer)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "UURemoteServer/624 CFNetwork/3896.200.41 Darwin/27.2.0")
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return err
	}
	if resp.StatusCode != 200 || envelope.Code != 0 {
		return &providerAPIError{path, resp.StatusCode, envelope.Code, envelope.Msg}
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}
func uploadApp(ctx context.Context) error {
	body := []byte(fmt.Sprintf(`[{"dapp_id":"6ab224c782973fd144c1b2ec","last_launched_at":%d,"app_status":1}]`, time.Now().Unix()))
	return api(ctx, "/api/v1/app/list/of/current_device/status", body, nil)
}

type signalingSession struct {
	controller     *controllerClient
	ctx            context.Context
	cancel         context.CancelFunc
	networkStamp   string
	ws             *websocket.Conn
	sendMu         sync.Mutex
	peers          map[string]*rtcPeer
	iceServers     []webrtc.ICEServer
	candidates     map[string][]webrtc.ICECandidateInit
	pending        *signalOffer
	pendingOptions bool
	captureType    uint64
	joined         bool
	lastPacket     atomic.Int64
	pingCount      atomic.Uint64
	refreshSent    atomic.Int64
	refreshAck     atomic.Int64
	workers        sync.WaitGroup
	started        time.Time
	connectionCtx  context.Context
	reconnectKey   string // Ephemeral: never logged or persisted to disk.
	resumeUntil    time.Time
}
type signalOffer struct {
	ClientID string `json:"client_id"`
	Data     struct {
		AppControlID string                  `json:"app_control_id"`
		IceID        string                  `json:"ice_id"`
		Type         string                  `json:"type"`
		SDP          string                  `json:"sdp"`
		Candidate    webrtc.ICECandidateInit `json:"candidate"`
	} `json:"data"`
	attachments []byte
}

func (s *signalingSession) write(kind int, b []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	if s.ws == nil {
		return &signalingTransportError{fmt.Errorf("signaling transport unavailable")}
	}
	s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.ws.WriteMessage(kind, b); err != nil {
		return &signalingTransportError{err}
	}
	return nil
}
func (s *signalingSession) text(v string) error { return s.write(websocket.TextMessage, []byte(v)) }
func (s *signalingSession) sendSDP(event []byte, attachment []byte) error {
	// Socket.IO binary envelope and attachment must not be interleaved with another event.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ws == nil {
		return &signalingTransportError{fmt.Errorf("signaling transport unavailable")}
	}
	s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.ws.WriteMessage(websocket.TextMessage, event); err != nil {
		return &signalingTransportError{err}
	}
	if err := s.ws.WriteMessage(websocket.BinaryMessage, attachment); err != nil {
		return &signalingTransportError{err}
	}
	return nil
}
func (s *signalingSession) close() {
	s.cancel()
	s.closePC()
	if s.controller != nil {
		s.controller.close()
	}
}
func (s *signalingSession) refreshKey() error {
	s.refreshSent.Store(time.Now().UnixMilli())
	return s.text(`422["refresh_reconnect_key"]`)
}
func (s *signalingSession) keepalive(ctx context.Context, ws *websocket.Conn) {
	if err := s.refreshKey(); err != nil {
		ws.Close()
		return
	}
	if err := s.uploadStatus(ctx); err != nil {
		log.Printf("app status: %v", err)
	}
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sent, ack := s.refreshSent.Load(), s.refreshAck.Load()
			if sent > ack && time.Since(time.UnixMilli(sent)) > 15*time.Second {
				log.Printf("signaling key refresh timed out")
				ws.Close()
				return
			}
			if time.Since(time.UnixMilli(sent)) >= time.Minute {
				if err := s.refreshKey(); err != nil {
					ws.Close()
					return
				}
			}
			if err := s.uploadStatus(ctx); err != nil {
				log.Printf("app status: %v", err)
			}
			log.Printf("session health uptime=%s last_packet=%s pings=%d", time.Since(s.started).Round(time.Second), time.Since(time.UnixMilli(s.lastPacket.Load())).Round(time.Second), s.pingCount.Load())
		}
	}
}
func buildICEServers(list []map[string]any) []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, v := range list {
		var urls []string
		switch u := v["urls"].(type) {
		case string:
			urls = []string{u}
		case []any:
			for _, x := range u {
				if z, ok := x.(string); ok {
					urls = append(urls, z)
				}
			}
		}
		if len(urls) == 0 {
			continue
		}
		user, _ := v["username"].(string)
		cred, _ := v["credential"].(string)
		out = append(out, webrtc.ICEServer{URLs: urls, Username: user, Credential: cred})
	}
	return out
}
func (s *signalingSession) offer(o *signalOffer, sdp string) error {
	if s.peers == nil {
		s.peers = map[string]*rtcPeer{}
	}
	id := o.Data.IceID
	// Preserve queued ICE candidates while replacing only this transport.
	if old := s.peers[id]; old != nil {
		old.close()
		delete(s.peers, id)
	}
	policy := webrtc.ICETransportPolicyAll
	if os.Getenv("UU_ICE_POLICY") == "relay" || runtimeConfig.ICEPolicy == "relay" {
		policy = webrtc.ICETransportPolicyRelay
	}
	pc, networkCancel, err := newProviderPeerConnection(s.ctx, webrtc.Configuration{ICEServers: s.iceServers, ICETransportPolicy: policy})
	if err != nil {
		return err
	}
	proto := newProtocolPeer(s.ctx)
	transport := &rtcPeer{pc: pc, protocol: proto, clientID: o.ClientID, networkCancel: networkCancel}
	s.peers[id] = transport
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("WebRTC ice=%s state=%s", id, st)
		if st == webrtc.PeerConnectionStateConnected {
			proto.startVideo()
		}
		if st == webrtc.PeerConnectionStateFailed {
			// A failed media transport cannot recover by reconnecting signaling.
			// Close outside Pion's callback to avoid blocking its state dispatcher.
			go transport.close()
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		log.Printf("ICE ice=%s state=%s", id, st)
		if st == webrtc.ICEConnectionStateConnected {
			if transport := pc.SCTP(); transport != nil {
				pair, err := transport.Transport().ICETransport().GetSelectedCandidatePair()
				if err == nil && pair != nil {
					log.Printf("ICE selected local=%s remote=%s", pair.Local, pair.Remote)
				}
			}
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		payload := map[string]any{"client_id": o.ClientID, "data": map[string]any{"app_control_id": o.Data.AppControlID, "ice_id": o.Data.IceID, "type": "candidate", "candidate": c.ToJSON()}}
		b, _ := json.Marshal(payload)
		if err := s.text(`42["soac",` + string(b) + `]`); err != nil {
			log.Printf("candidate send: %v", err)
		}
	})
	var relayMu sync.Mutex
	var relay *pmRelay
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if strings.HasPrefix(dc.Label(), udpMappingPrefix) {
			acceptUDPMapping(transport, dc)
			return
		}
		log.Printf("DataChannel %s", dc.Label())
		dc.OnError(func(err error) { log.Printf("DataChannel ice=%s label=%s error=%v", id, dc.Label(), err) })
		proto.register(dc)
		if dc.Label() == "FILE_DATA_CHANNEL" {
			relayMu.Lock()
			relay = newPMRelay(dc)
			r := relay
			relayMu.Unlock()
			transport.addRelay(r)
			dc.OnClose(r.close)
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if replyTunnelProbe(dc, msg.Data) {
				return
			}
			proto.handle(dc.Label(), msg.Data)
			if f, e := decodePM(msg.Data); e == nil {
				if os.Getenv("UU_TRACE_FRAMES") == "1" {
					log.Printf("PM channel=%s type=%d stream=%d bytes=%d", dc.Label(), f.kind, f.key.stream, len(f.payload))
				}
				relayMu.Lock()
				r := relay
				relayMu.Unlock()
				if r != nil {
					r.handle(msg.Data)
				}
			}
		})
		dc.OnOpen(func() {
			log.Printf("DataChannel open %s", dc.Label())
			if dc.Label() == "TEXT_DATA_CHANNEL" {
				proto.announceDesktop()
			}
		})
	})

	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return err
	}
	log.Printf("WebRTC offer video=%t initial_capture=%d", strings.Contains(sdp, "m=video "), s.captureType)
	if strings.Contains(sdp, "m=video ") || s.captureType == 1 || s.captureType == 2 || s.captureType == 6 {
		if err := proto.setupDesktop(pc); err != nil {
			return err
		}
		proto.desktop.mu.Lock()
		proto.desktop.requested = s.captureType == 1 || s.captureType == 2 || s.captureType == 6
		proto.desktop.mu.Unlock()
	}
	for _, c := range s.candidates[id] {
		if e := pc.AddICECandidate(c); e != nil {
			log.Printf("queued ICE: %v", e)
		}
	}
	delete(s.candidates, id)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		return err
	}
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write([]byte(answer.SDP))
	z.Close()
	payload := map[string]any{"client_id": o.ClientID, "data": map[string]any{"app_control_id": o.Data.AppControlID, "ice_id": o.Data.IceID, "type": "answer", "sdp": "", "gzip_sdp": map[string]any{"_placeholder": true, "num": 0}}}
	b, _ := json.Marshal(payload)
	if err = s.sendSDP([]byte(`451-["soac",`+string(b)+`]`), append([]byte{4}, buf.Bytes()...)); err != nil {
		return err
	}
	log.Printf("WebRTC answer sent (%d bytes)", buf.Len())
	return nil
}
func sessionDisconnect(packet string) bool {
	return packet == "41" || strings.HasPrefix(packet, "41{") || strings.HasPrefix(packet, "44") || packet == "1"
}
func (s *signalingSession) handleText(packet string) error {
	if sessionDisconnect(packet) {
		return fmt.Errorf("signaling session disconnected: %.160s", packet)
	}
	if strings.HasPrefix(packet, "2") {
		s.pingCount.Add(1)
		return s.text("3" + packet[1:])
	}
	if strings.HasPrefix(packet, "0") || packet == "3" {
		return nil
	}
	if strings.HasPrefix(packet, "40") {
		if s.joined {
			return nil
		}
		s.joined = true
		log.Printf("Socket.IO joined")
		if err := s.text(`421["room_info"]`); err != nil {
			return err
		}
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.keepalive(s.connectionCtx, s.ws) }()
		return nil
	}
	if strings.HasPrefix(packet, "432[") {
		var result []json.RawMessage
		if err := json.Unmarshal([]byte(packet[3:]), &result); err != nil {
			return err
		}
		var status string
		var data struct {
			Key string `json:"reconnect_key"`
		}
		if len(result) >= 2 {
			json.Unmarshal(result[0], &status)
			json.Unmarshal(result[1], &data)
		}
		if status != "success" || data.Key == "" {
			return fmt.Errorf("signaling key refresh rejected (status=%q)", status)
		}
		s.reconnectKey = data.Key
		s.refreshAck.Store(time.Now().UnixMilli())
		if !s.resumeUntil.IsZero() {
			log.Printf("signaling resumed; retained peers=%d", s.peerCount())
			s.resumeUntil = time.Time{}
		}
		log.Printf("signaling key refreshed")
		return nil
	}
	if strings.HasPrefix(packet, "431") {
		if s.controller != nil {
			return s.controller.roomInfo(packet[3:])
		}
		log.Printf("room info received")
		return nil
	}
	if s.controller != nil && strings.HasPrefix(packet, "433") {
		return s.controller.controlReply(packet[3:])
	}
	offset := 0
	if strings.HasPrefix(packet, "451-") {
		offset = 4
	} else if strings.HasPrefix(packet, "42") {
		offset = 2
	} else {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(packet[offset:]), &arr); err != nil {
		return err
	}
	if len(arr) == 0 {
		return nil
	}
	var event string
	json.Unmarshal(arr[0], &event)
	if event == "leave" {
		return fmt.Errorf("server requested room leave")
	}
	if len(arr) < 2 {
		return nil
	}
	if s.controller != nil {
		return s.controller.event(event, arr[1])
	}
	switch event {
	case "bmsg_push":
		var summary struct {
			Type   string `json:"type"`
			Action string `json:"action"`
		}
		json.Unmarshal(arr[1], &summary)
		log.Printf("signal push type=%s action=%s", summary.Type, summary.Action)
		if action := signalPowerAction(arr[1]); action != "" {
			if err := requestPower(action); err != nil {
				log.Printf("POWER rejected: %v", err)
			}
		}
	case "be-controlled":
		var d struct {
			IceServers []map[string]any `json:"iceServers"`
		}
		if err := json.Unmarshal(arr[1], &d); err != nil {
			return err
		}
		s.iceServers = buildICEServers(d.IceServers)
		s.pendingOptions = true
		log.Printf("controller arrived, ICE servers=%d", len(s.iceServers))
	case "soac":
		var o signalOffer
		if err := json.Unmarshal(arr[1], &o); err != nil {
			return err
		}
		switch o.Data.Type {
		case "offer":
			if o.Data.SDP != "" {
				return s.offer(&o, o.Data.SDP)
			}
			s.pending = &o
		case "candidate":
			if peer := s.peers[o.Data.IceID]; peer != nil && peer.pc.RemoteDescription() != nil {
				if err := peer.pc.AddICECandidate(o.Data.Candidate); err != nil {
					log.Printf("ICE candidate: %v", err)
				}
			} else {
				if s.candidates == nil {
					s.candidates = map[string][]webrtc.ICECandidateInit{}
				}
				s.candidates[o.Data.IceID] = append(s.candidates[o.Data.IceID], o.Data.Candidate)
			}
		}
	case "released", "left":
		var left struct {
			IceID    string `json:"ice_id"`
			ClientID string `json:"client_id"`
		}
		json.Unmarshal(arr[1], &left)
		log.Printf("controller event %s ice=%s", event, left.IceID)
		if left.IceID != "" {
			s.closePeer(left.IceID)
		} else if left.ClientID != "" {
			for id, p := range s.peers {
				if p.clientID == left.ClientID {
					s.closePeer(id)
				}
			}
		}
	}
	return nil
}
func (s *signalingSession) handleBinary(packet []byte) error {
	if s.controller != nil {
		return s.controller.binary(packet)
	}
	if s.pendingOptions {
		s.pendingOptions = false
		b := packet
		if len(b) > 0 && b[0] == 4 {
			b = b[1:]
		}
		s.captureType = pbNumber(b, 1)
		log.Printf("controller capture_type=%d", s.captureType)
		return nil
	}
	if s.pending == nil {
		return nil
	}
	s.pending.attachments = append(s.pending.attachments, packet...)
	b := s.pending.attachments
	if len(b) > 2<<20 {
		return fmt.Errorf("SDP attachment too large")
	}
	if len(b) > 0 && b[0] == 4 {
		b = b[1:]
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(z, 2<<20))
	z.Close()
	if err != nil {
		return nil
	}
	o := s.pending
	s.pending = nil
	return s.offer(o, string(raw))
}

// serve runs the controlled-side session loop. The optional expectedIdentity is
// the DeviceID+ClientID the caller locked on. A login/relogin can atomically
// rewrite the config with a different identity; if that happens the running
// process holds the wrong identity lock, so it must exit and let the supervisor
// (or a fresh `uulink serve`) re-lock the new identity instead of running under
// a stale lock. Token refreshes keep the same identity and are ignored.
func serve(configPath string, expectedIdentity ...string) {
	want := ""
	if len(expectedIdentity) > 0 {
		want = expectedIdentity[0]
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	retry := 2 * time.Second
	resourcesConfigured := false
	for ctx.Err() == nil {
		err := loadAgentConfig(configPath)
		started := time.Now()
		if err == nil {
			// Refuse to serve with an identity copied from another machine.
			if runtimeConfig.HostID != "" && machineID() != "" && runtimeConfig.HostID != machineID() {
				log.Printf("配置属于另一台机器；请在本机使用独立设备身份重新登录")
				return
			}
			// The identity lock was taken for the identity read before locking.
			// If the config now names a different identity, this process must not
			// keep running under the old lock; exit so the new identity is locked.
			if want != "" && runtimeConfig.DeviceID+"\x00"+runtimeConfig.ClientID != want {
				log.Printf("设备身份已变更，退出以重新获取锁")
				return
			}
			if !resourcesConfigured {
				configureResources(runtimeConfig)
				resourcesConfigured = true
			}
			err = runSession(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("session ended: %v; reconnect in %s", err, retry)
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if time.Since(started) > time.Minute {
			retry = 2 * time.Second
		} else if retry < 30*time.Second {
			retry *= 2
			if retry > 30*time.Second {
				retry = 30 * time.Second
			}
		}
	}
}
