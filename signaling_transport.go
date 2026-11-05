package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

type signalingRoom struct {
	Token  string `json:"token"`
	Server string `json:"signaling_server"`
}

// Only transport failures may resume a room. A rejected key, explicit room
// leave, or malformed protocol reply must end the session and release peers.
type signalingTransportError struct{ error }

func (e *signalingTransportError) Unwrap() error { return e.error }

func runSession(ctx context.Context) error {
	var room signalingRoom
	if err := api(ctx, "/api/v1/room/create", []byte(`{"last_controlled_interval":-1}`), &room); err != nil {
		return err
	}
	if room.Token == "" || room.Server == "" {
		return fmt.Errorf("room/create missing token/server")
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &signalingSession{ctx: sessionCtx, cancel: cancel, started: time.Now()}
	defer s.close()
	return s.run(room, time.Minute, 2*time.Second)
}

func (s *signalingSession) run(room signalingRoom, resumeWindow, retryDelay time.Duration) error {
	for {
		err := s.connect(room)
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		var transportErr *signalingTransportError
		if s.reconnectKey == "" || !errors.As(err, &transportErr) {
			return err
		}
		if s.resumeUntil.IsZero() {
			s.resumeUntil = time.Now().Add(resumeWindow)
		}
		remaining := time.Until(s.resumeUntil)
		if remaining <= 0 {
			return fmt.Errorf("signaling resume deadline exceeded: %w", err)
		}
		log.Printf("signaling transport lost: %v; retaining peers=%d, resume in %s", err, s.peerCount(), min(retryDelay, remaining))
		timer := time.NewTimer(min(retryDelay, remaining))
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *signalingSession) connect(room signalingRoom) error {
	networkStamp, err := providerNetworkStamp(s.ctx)
	if err != nil {
		return err
	}
	if s.networkStamp != "" && s.networkStamp != networkStamp {
		return fmt.Errorf("物理出口已改变，重新建立隧道")
	}
	s.networkStamp = networkStamp
	u, err := url.Parse(room.Server)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	}
	u.Path = "/socket.io/"
	u.RawQuery = fmt.Sprintf("EIO=4&transport=websocket&t=%d", time.Now().Unix())
	h := http.Header{}
	h.Set("X-NRD-AUTH", room.Token)
	if s.reconnectKey != "" {
		h.Set("X-NRD-RECONN-KEY", s.reconnectKey)
	}
	h.Set("streamer_flag", `{"sdp_flags":{"gzip_sdp":true}}`)
	h.Set("streamer_version", "V4.7.3")
	h.Set("User-Agent", "WebSocket++/0.8.2")
	dialCtx := s.ctx
	if !s.resumeUntil.IsZero() {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithDeadline(s.ctx, s.resumeUntil)
		defer cancel()
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: nil, NetDialContext: providerDialContext}
	ws, resp, err := dialer.DialContext(dialCtx, u.String(), h)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return fmt.Errorf("signaling handshake rejected: HTTP %d", resp.StatusCode)
			}
		}
		return &signalingTransportError{err}
	}
	// The peer context belongs to the room, not this individual WebSocket.
	connectionCtx, cancel := context.WithCancel(s.ctx)
	s.connectionCtx = connectionCtx
	watchProviderNetwork(connectionCtx, networkStamp, s.cancel)
	s.joined = false
	s.pending = nil
	s.pendingOptions = false
	s.refreshSent.Store(0)
	s.refreshAck.Store(0)
	s.sendMu.Lock()
	s.ws = ws
	s.sendMu.Unlock()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-connectionCtx.Done()
		ws.Close()
	}()
	defer func() {
		cancel()
		ws.Close()
		// The old heartbeat must exit before another socket becomes writable.
		s.workers.Wait()
		<-watcherDone
		s.sendMu.Lock()
		s.ws = nil
		s.sendMu.Unlock()
	}()
	log.Printf("signaling connected local=%s remote=%s resume=%t", ws.LocalAddr(), ws.RemoteAddr(), s.reconnectKey != "")
	ws.SetReadLimit(4 << 20)
	if err := s.text("40"); err != nil {
		return err
	}
	for {
		timeout := 45 * time.Second
		if !s.joined {
			timeout = 15 * time.Second
		}
		deadline := time.Now().Add(timeout)
		if !s.resumeUntil.IsZero() && s.resumeUntil.Before(deadline) {
			deadline = s.resumeUntil
		}
		ws.SetReadDeadline(deadline)
		kind, packet, err := ws.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && (closeErr.Code == websocket.ClosePolicyViolation || closeErr.Code == websocket.CloseProtocolError) {
				return fmt.Errorf("signaling rejected transport: %w", err)
			}
			return &signalingTransportError{err}
		}
		s.lastPacket.Store(time.Now().UnixMilli())
		// Failed transports close asynchronously in Pion callbacks. Remove their
		// bookkeeping here, on the same reader goroutine that handles offers.
		for id, p := range s.peers {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				delete(s.peers, id)
				delete(s.candidates, id)
			}
		}
		if kind == websocket.TextMessage {
			err = s.handleText(string(packet))
		} else if kind == websocket.BinaryMessage {
			err = s.handleBinary(packet)
		}
		if err != nil {
			return err
		}
	}
}
