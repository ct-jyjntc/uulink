package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

func pbFields(raw []byte) map[uint64]pbField {
	fs, _ := parsePB(raw)
	out := map[uint64]pbField{}
	for _, f := range fs {
		out[f.num] = f
	}
	return out
}
func pbNumber(raw []byte, n uint64) uint64 { return pbFields(raw)[n].value }
func pbData(raw []byte, n uint64) []byte   { return pbFields(raw)[n].data }
func traceProtocol(channel string, raw []byte) {
	if channel == "STREAMER_DATA_CHANNEL" {
		return
	}
	fs, err := parsePB(raw)
	if err != nil {
		log.Printf("PROTO channel=%s length=%d binary-prefix=%x", channel, len(raw), raw[:min(len(raw), 96)])
		return
	}
	var desc []string
	for _, f := range fs {
		if f.wire == 2 {
			desc = append(desc, fmt.Sprintf("%d:bytes(%d)", f.num, len(f.data)))
		} else {
			desc = append(desc, fmt.Sprintf("%d:%d", f.num, f.value))
		}
	}
	log.Printf("PROTO channel=%s fields=%v", channel, desc)
	if channel == "CONTROL_DATA_CHANNEL" && len(raw) < 1024 {
		log.Printf("PROTO control=%x", raw)
	}
}

type protocolPeer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	channels map[string]*webrtc.DataChannel
	seq      atomic.Uint64
	files    *fileService
	desktop  *desktopStream
	input    *inputEngine
	features components
}

func newProtocolPeer(ctx context.Context) *protocolPeer {
	ctx, cancel := context.WithCancel(ctx)
	p := &protocolPeer{ctx: ctx, cancel: cancel, channels: map[string]*webrtc.DataChannel{}, features: runtimeConfig.features()}
	p.files = newFileService(p)
	if p.features.desktop {
		p.input = newInputEngine(p)
	}
	return p
}
func (p *protocolPeer) close() { p.cancel(); terminalDetachPeer(p); p.files.close() }
func (p *protocolPeer) register(dc *webrtc.DataChannel) {
	p.mu.Lock()
	p.channels[dc.Label()] = dc
	p.mu.Unlock()
}
func (p *protocolPeer) send(channel string, field uint64, data []byte) error {
	b := putVar(nil, 1, p.seq.Add(1))
	b = putVar(b, 2, uint64(time.Now().Unix()))
	b = putBytes(b, field, data)
	p.mu.Lock()
	dc := p.channels[channel]
	p.mu.Unlock()
	if dc == nil {
		return fmt.Errorf("channel %s unavailable", channel)
	}
	return dc.Send(b)
}
func (p *protocolPeer) rpcReply(channel string, header []byte, field uint64, data []byte) error {
	b := putBytes(nil, 1, header)
	b = putBytes(b, field, data)
	return p.send(channel, 22, b)
}
func (p *protocolPeer) handle(channel string, raw []byte) {
	select {
	case <-p.ctx.Done():
		return
	default:
	}
	if len(raw) > 0 && raw[0] == '{' && (channel == "CONTROL_DATA_CHANNEL" || channel == "TEXT_DATA_CHANNEL") {
		if p.input != nil {
			p.input.enqueue(raw)
		}
		return
	}
	if channel == "BINARY_DATA_CHANNEL" && len(raw) >= 4 && string(raw[:4]) == "TERM" {
		p.terminalHandle(raw)
		return
	}
	if os.Getenv("UU_TRACE_FRAMES") == "1" {
		traceProtocol(channel, raw)
	}
	fs, err := parsePB(raw)
	if err != nil {
		return
	}
	for _, f := range fs {
		switch f.num {
		case 3:
			action := pbNumber(f.data, 1)
			switch action {
			case 0:
				flags := putVar(nil, 1, 1)
				flags = putVar(flags, 3, 1)
				flags = putVar(flags, 6, 1)
				reply := putVar(nil, 1, 1)
				reply = putBytes(reply, 4, flags)
				p.send("CONTROL_DATA_CHANNEL", 3, reply)
			case 4, 5, 6, 8:
				p.requestVideo()
			case 9:
				if err := requestPower("reboot"); err != nil {
					log.Printf("POWER rejected: %v", err)
				}
			case 12:
				if err := requestPower("poweroff"); err != nil {
					log.Printf("POWER rejected: %v", err)
				}
			default:
				log.Printf("SIMPLE action=%d", action)
			}
		case 13:
			setting := putVar(nil, 8, 2)
			setting = putVar(setting, 9, 2)
			setting = putVar(setting, 10, 2)
			response := putBytes(nil, 3, setting)
			response = putVar(response, 4, 1)
			p.send(channel, 13, response)
		case 22:
			p.files.response(f.data)
		case 21:
			fields := pbFields(f.data)
			header := fields[1].data
			for num, v := range fields {
				if num == 1 {
					continue
				}
				log.Printf("RPC request type=%d bytes=%d", num, len(v.data))
				switch num {
				case 2:
					p.rpcReply(channel, header, 2, nil)
					p.announceDesktop()
				case 15:
					p.rpcReply(channel, header, 11, nil)
				case 28:
					p.rpcReply(channel, header, 24, nil)
				case 8:
					p.files.handle(channel, header, v.data)
				}
			}
		}
	}
}
