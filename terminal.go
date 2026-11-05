//go:build !windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Official TerminalFrame: TERM, version u8, type u8, flags u16 LE,
// session ID u32 LE, payload length u32 LE, payload.
type terminalFrame struct {
	kind    byte
	flags   uint16
	id      uint32
	payload []byte
}

func parseTerminal(raw []byte) (terminalFrame, error) {
	var f terminalFrame
	if len(raw) < 16 || !bytes.Equal(raw[:4], []byte("TERM")) || raw[4] != 1 {
		return f, fmt.Errorf("invalid terminal header")
	}
	n := binary.LittleEndian.Uint32(raw[12:16])
	if n > 1<<20 || int(n) != len(raw)-16 {
		return f, fmt.Errorf("invalid terminal length")
	}
	f = terminalFrame{raw[5], binary.LittleEndian.Uint16(raw[6:8]), binary.LittleEndian.Uint32(raw[8:12]), raw[16:]}
	return f, nil
}
func encodeTerminal(f terminalFrame) []byte {
	b := make([]byte, 16+len(f.payload))
	copy(b, "TERM")
	b[4] = 1
	b[5] = f.kind
	binary.LittleEndian.PutUint16(b[6:], f.flags)
	binary.LittleEndian.PutUint32(b[8:], f.id)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(f.payload)))
	copy(b[16:], f.payload)
	return b
}
func (p *protocolPeer) terminalSend(kind byte, id uint32, data []byte) error {
	p.mu.Lock()
	dc := p.channels["BINARY_DATA_CHANNEL"]
	p.mu.Unlock()
	if dc == nil {
		return fmt.Errorf("binary channel unavailable")
	}
	return dc.Send(encodeTerminal(terminalFrame{kind: kind, id: id, payload: data}))
}
func (p *protocolPeer) terminalJSON(kind byte, id uint32, v any) {
	b, _ := json.Marshal(v)
	if e := p.terminalSend(kind, id, b); e != nil {
		log.Printf("terminal send: %v", e)
	}
}

type shellSession struct {
	id          uint32
	name, shell string
	created     int64
	file        *os.File
	cmd         *exec.Cmd
	mu          sync.Mutex
	peer        *protocolPeer
	scrollback  []byte
}

var shellsMu sync.Mutex
var shells = map[uint32]*shellSession{}
var nextShell atomic.Uint32

func terminalDetachPeer(p *protocolPeer) {
	shellsMu.Lock()
	defer shellsMu.Unlock()
	for _, s := range shells {
		s.mu.Lock()
		if s.peer == p {
			s.peer = nil
		}
		s.mu.Unlock()
	}
}
func (p *protocolPeer) terminalHandle(raw []byte) {
	f, err := parseTerminal(raw)
	if err != nil {
		log.Printf("terminal frame: %v", err)
		return
	}
	if !p.features.terminal {
		p.terminalJSON(13, f.id, map[string]any{"code": 1, "message": "Terminal component is disabled; enable it with uulink install --components mapping,terminal"})
		return
	}
	if f.kind != 2 || os.Getenv("UU_TRACE_FRAMES") == "1" {
		log.Printf("TERMINAL type=%d id=%d bytes=%d", f.kind, f.id, len(f.payload))
	}
	shellsMu.Lock()
	s := shells[f.id]
	shellsMu.Unlock()
	switch f.kind {
	case 9:
		list := []map[string]any{}
		shellsMu.Lock()
		for _, s := range shells {
			s.mu.Lock()
			state := "running"
			if s.peer == nil {
				state = "detached"
			}
			list = append(list, map[string]any{"session_id": s.id, "sessionId": s.id, "name": s.name, "shell": s.shell, "state": state, "created_at": s.created, "createdAtMs": s.created})
			s.mu.Unlock()
		}
		shellsMu.Unlock()
		p.terminalJSON(10, f.id, map[string]any{"sessions": list})
	case 1:
		var v struct {
			Cols, Rows       int
			Shell, Cwd, Name string
		}
		if len(f.payload) > 0 {
			if json.Unmarshal(f.payload, &v) != nil {
				p.terminalJSON(13, f.id, map[string]any{"code": 1, "message": "Invalid open request"})
				return
			}
		}
		if s != nil {
			s.mu.Lock()
			s.peer = p
			b := append([]byte(nil), s.scrollback...)
			s.mu.Unlock()
			p.terminalJSON(13, f.id, map[string]any{"code": 0, "message": ""})
			p.terminalSend(5, f.id, b)
			return
		}
		id := f.id
		if id == 0 {
			id = nextShell.Add(1)
		}
		if v.Cols <= 0 || v.Cols > 1000 {
			v.Cols = 100
		}
		if v.Rows <= 0 || v.Rows > 1000 {
			v.Rows = 30
		}
		v.Shell = resolveShell(v.Shell, exec.LookPath)
		if v.Cwd == "" {
			v.Cwd = "/root"
		}
		if v.Name == "" {
			v.Name = fmt.Sprintf("Terminal %d", id)
		}
		cmd := exec.Command(v.Shell, "-l")
		cmd.Dir = v.Cwd
		cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "LANG=C.UTF-8")
		file, e := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(v.Rows), Cols: uint16(v.Cols)})
		if e != nil {
			p.terminalJSON(13, id, map[string]any{"code": 1, "message": e.Error()})
			return
		}
		s = &shellSession{id: id, name: v.Name, shell: v.Shell, created: time.Now().UnixMilli(), file: file, cmd: cmd, peer: p}
		shellsMu.Lock()
		shells[id] = s
		shellsMu.Unlock()
		p.terminalJSON(13, id, map[string]any{"code": 0, "message": ""})
		go s.readLoop()
	case 2:
		if s != nil {
			if _, e := s.file.Write(f.payload); e != nil {
				p.terminalJSON(8, f.id, map[string]any{"code": 1, "message": e.Error()})
			}
		}
	case 3:
		if s != nil {
			var v struct{ Cols, Rows int }
			if json.Unmarshal(f.payload, &v) == nil && v.Cols > 0 && v.Cols <= 1000 && v.Rows > 0 && v.Rows <= 1000 {
				pty.Setsize(s.file, &pty.Winsize{Rows: uint16(v.Rows), Cols: uint16(v.Cols)})
			}
		}
	case 4, 11:
		if s != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
			s.file.Close()
		}
		if f.kind == 11 {
			p.terminalJSON(12, f.id, map[string]any{"code": 0, "message": ""})
		}
	case 16:
		if s != nil {
			s.mu.Lock()
			s.peer = nil
			s.mu.Unlock()
		}
	case 7:
		if s != nil {
			var v struct{ Signal string }
			json.Unmarshal(f.payload, &v)
			signals := map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM, "SIGHUP": syscall.SIGHUP, "SIGKILL": syscall.SIGKILL}
			if sig, ok := signals[v.Signal]; ok {
				syscall.Kill(-s.cmd.Process.Pid, sig)
			}
		}
	case 14:
		p.terminalJSON(15, f.id, map[string]any{"code": 1, "message": "Terminal unlock is not configured"})
	}
}

func resolveShell(requested string, lookup func(string) (string, error)) string {
	if requested != "" {
		if path, err := lookup(requested); err == nil {
			return path
		}
		switch filepath.Base(requested) {
		case "bash", "zsh", "ash", "sh":
		default:
			return requested
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/ash", "/bin/sh"} {
		if path, err := lookup(candidate); err == nil {
			return path
		}
	}
	return "/bin/sh"
}
func (s *shellSession) readLoop() {
	b := make([]byte, 16*1024)
	for {
		n, err := s.file.Read(b)
		if n > 0 {
			s.mu.Lock()
			s.scrollback = append(s.scrollback, b[:n]...)
			if len(s.scrollback) > 256*1024 {
				s.scrollback = append([]byte(nil), s.scrollback[len(s.scrollback)-256*1024:]...)
			}
			p := s.peer
			s.mu.Unlock()
			if p != nil {
				p.terminalSend(5, s.id, b[:n])
			}
		}
		if err != nil {
			break
		}
	}
	s.file.Close()
	err := s.cmd.Wait()
	code := 0
	if err != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	shellsMu.Lock()
	delete(shells, s.id)
	shellsMu.Unlock()
	s.mu.Lock()
	p := s.peer
	s.mu.Unlock()
	if p != nil {
		p.terminalJSON(6, s.id, map[string]any{"exit_code": code})
	}
}
