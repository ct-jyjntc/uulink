package main

import (
	"context"
	"github.com/pion/webrtc/v4"
	"sync"
	"sync/atomic"
)

type rtcPeer struct {
	networkCancel context.CancelFunc
	pc            *webrtc.PeerConnection
	protocol      *protocolPeer
	clientID      string
	mu            sync.Mutex
	gameUDP       *gameUDPBridge
	relays        []*pmRelay
	udpMappings   atomic.Int32
	closed        bool
}

func (p *rtcPeer) addRelay(r *pmRelay) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		r.close()
		return
	}
	p.relays = append(p.relays, r)
	p.mu.Unlock()
}
func (p *rtcPeer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	game := p.gameUDP
	p.gameUDP = nil
	relays := p.relays
	p.relays = nil
	p.mu.Unlock()
	if game != nil {
		game.close()
	}
	if p.networkCancel != nil {
		p.networkCancel()
	}
	if p.protocol != nil {
		p.protocol.close()
	}
	for _, r := range relays {
		r.close()
	}
	if p.pc != nil {
		p.pc.Close()
	}
}
func (s *signalingSession) closePeer(id string) {
	if p := s.peers[id]; p != nil {
		delete(s.peers, id)
		p.close()
	}
	delete(s.candidates, id)
}
func (s *signalingSession) closePC() {
	for id := range s.peers {
		s.closePeer(id)
	}
}
