package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Swift FrameType: syn=0, fin=1, data=2, synAck=3, dataAck=4.
const (
	pmSYN uint64 = iota
	pmFIN
	pmDATA
	pmSYNACK
	pmDATAACK
)

type pmKey struct{ session, rule, stream uint64 }
type pmFrame struct {
	key           pmKey
	seq, ts, kind uint64
	payload       []byte
}
type pbField struct {
	num, wire uint64
	value     uint64
	data      []byte
}

func parsePB(b []byte) ([]pbField, error) {
	var out []pbField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, fmt.Errorf("invalid protobuf tag")
		}
		b = b[n:]
		f := pbField{num: tag >> 3, wire: tag & 7}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, io.ErrUnexpectedEOF
			}
			f.value = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[8:]
		case 2:
			v, n := binary.Uvarint(b)
			if n <= 0 || v > uint64(len(b)-n) {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[n:]
			f.data = b[:int(v)]
			b = b[int(v):]
		case 5:
			if len(b) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("unsupported wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}
func putVar(b []byte, n, v uint64) []byte {
	b = binary.AppendUvarint(b, n<<3)
	return binary.AppendUvarint(b, v)
}
func putBytes(b []byte, n uint64, v []byte) []byte {
	b = binary.AppendUvarint(b, n<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}
func decodePM(raw []byte) (pmFrame, error) {
	var f pmFrame
	fs, err := parsePB(raw)
	if err != nil {
		return f, err
	}
	var inner []byte
	for _, p := range fs {
		switch p.num {
		case 1:
			f.seq = p.value
		case 2:
			f.ts = p.value
		case 27:
			inner = p.data
		}
	}
	if inner == nil {
		return f, fmt.Errorf("not a port mapping message")
	}
	fs, err = parsePB(inner)
	if err != nil {
		return f, err
	}
	for _, p := range fs {
		switch p.num {
		case 1:
			f.key.session = p.value
		case 2:
			f.key.rule = p.value
		case 3:
			f.key.stream = p.value
		case 4:
			f.kind = p.value
		case 5:
			f.payload = p.data
		}
	}
	return f, nil
}
func encodePM(f pmFrame) []byte {
	b := putVar(nil, 1, f.key.session)
	b = putVar(b, 2, f.key.rule)
	b = putVar(b, 3, f.key.stream)
	if f.kind != 0 {
		b = putVar(b, 4, f.kind)
	}
	if len(f.payload) > 0 {
		b = putBytes(b, 5, f.payload)
	}
	out := putVar(nil, 1, f.seq)
	out = putVar(out, 2, f.ts)
	return putBytes(out, 27, b)
}

type pmSender interface{ Send([]byte) error }
type pmRelay struct {
	mu      sync.Mutex
	streams map[pmKey]*pmStream
	sender  pmSender
	seq     atomic.Uint64
	closed  bool
}
type pmStream struct {
	key  pmKey
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func newPMRelay(s pmSender) *pmRelay { return &pmRelay{streams: make(map[pmKey]*pmStream), sender: s} }
func (r *pmRelay) send(key pmKey, kind uint64, p []byte) error {
	f := pmFrame{key: key, seq: r.seq.Add(1), ts: uint64(time.Now().Unix()), kind: kind, payload: p}
	if kind == pmDATA {
		if dc, ok := r.sender.(interface{ BufferedAmount() uint64 }); ok {
			deadline := time.Now().Add(30 * time.Second)
			for dc.BufferedAmount() > 1<<20 {
				r.mu.Lock()
				closed := r.closed
				r.mu.Unlock()
				if closed || time.Now().After(deadline) {
					return fmt.Errorf("data channel backpressure timeout")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	err := r.sender.Send(encodePM(f))
	if err != nil {
		log.Printf("PM send error: %v", err)
	}
	return err
}
func (r *pmRelay) closeStream(s *pmStream, notify bool) {
	s.once.Do(func() {
		close(s.done)
		r.mu.Lock()
		if r.streams[s.key] == s {
			delete(r.streams, s.key)
		}
		c := s.conn
		r.mu.Unlock()
		if c != nil {
			c.Close()
		}
		if notify {
			r.send(s.key, pmFIN, nil)
		}
	})
}
func (r *pmRelay) close() {
	r.mu.Lock()
	r.closed = true
	all := make([]*pmStream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.mu.Unlock()
	for _, s := range all {
		r.closeStream(s, false)
	}
}
func (r *pmRelay) handle(raw []byte) {
	f, err := decodePM(raw)
	if err != nil {
		log.Printf("PM decode: %v", err)
		return
	}
	if f.kind != pmDATA && f.kind != pmDATAACK {
		log.Printf("PM recv type=%d session=%d rule=%d stream=%d payload=%s", f.kind, f.key.session, f.key.rule, f.key.stream, f.payload)
	}
	r.mu.Lock()
	s := r.streams[f.key]
	isClosed := r.closed
	r.mu.Unlock()
	if isClosed {
		return
	}
	switch f.kind {
	case pmSYN:
		if s != nil {
			return
		}
		s = &pmStream{key: f.key, done: make(chan struct{})}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.streams[f.key] = s
		r.mu.Unlock()
		go r.open(s, f.payload)
	case pmFIN:
		if s != nil {
			r.closeStream(s, false)
		}
	case pmDATA:
		if s == nil {
			return
		}
		r.mu.Lock()
		c := s.conn
		r.mu.Unlock()
		if c == nil {
			return
		}
		c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_, err = io.Copy(c, bytes.NewReader(f.payload))
		if err != nil {
			log.Printf("PM write: %v", err)
			r.closeStream(s, true)
			return
		}
		if r.send(f.key, pmDATAACK, nil) != nil {
			r.closeStream(s, false)
		}
	case pmDATAACK:
		// The official controller does not require B-to-A acknowledgements.

	default:
		log.Printf("PM unsupported type=%d", f.kind)
	}
}
func (r *pmRelay) open(s *pmStream, payload []byte) {
	var target struct {
		Version int    `json:"version"`
		Host    string `json:"target_host"`
		Port    int    `json:"target_port"`
	}
	err := json.Unmarshal(payload, &target)
	if err == nil && (target.Version != 1 || target.Host == "" || target.Port < 1 || target.Port > 65535) {
		err = fmt.Errorf("invalid SYN target")
	}
	var c net.Conn
	if err == nil {
		c, err = net.DialTimeout("tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port)), 8*time.Second)
	}
	if err != nil {
		p, _ := json.Marshal(map[string]any{"version": 1, "ok": false, "error": err.Error()})
		r.send(s.key, pmSYNACK, p)
		r.closeStream(s, false)
		return
	}
	r.mu.Lock()
	select {
	case <-s.done:
		r.mu.Unlock()
		c.Close()
		return
	default:
	}
	s.conn = c
	r.mu.Unlock()
	if r.send(s.key, pmSYNACK, []byte(`{"version":1,"ok":true}`)) != nil {
		r.closeStream(s, false)
		return
	}
	log.Printf("PM connected rule=%d stream=%d target=%s", s.key.rule, s.key.stream, c.RemoteAddr())
	defer r.closeStream(s, true)
	buf := make([]byte, 16*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if r.send(s.key, pmDATA, buf[:n]) != nil {
				return
			}

		}
		if err != nil {
			if err != io.EOF {
				log.Printf("PM read: %v", err)
			}
			return
		}
	}
}
