package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

type inputEvent struct {
	Action, Heartbeat string
	Key               int
	Button            json.RawMessage
	AbsX              float64 `json:"abs_x"`
	AbsY              float64 `json:"abs_y"`
	RelX              float64 `json:"rel_x"`
	RelY              float64 `json:"rel_y"`
	DeltaX            float64 `json:"delta_x"`
	DeltaY            float64 `json:"delta_y"`
	Text              string
}
type inputEngine struct {
	peer   *protocolPeer
	events chan inputEvent
}

func newInputEngine(p *protocolPeer) *inputEngine {
	i := &inputEngine{peer: p, events: make(chan inputEvent, 512)}
	go i.run()
	return i
}
func (i *inputEngine) enqueue(raw []byte) {
	var e inputEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if os.Getenv("UU_TRACE_FRAMES") == "1" && e.Action != "" && !strings.HasPrefix(e.Action, "mouse_move") {
		log.Printf("INPUT action=%s key=%d button=%s", e.Action, e.Key, string(e.Button))
	}
	if e.Action == "" && e.Heartbeat == "" {
		return
	}
	select {
	case i.events <- e:
	case <-i.peer.ctx.Done():
	default:
		if !strings.HasPrefix(e.Action, "mouse_move") {
			select {
			case i.events <- e:
			case <-i.peer.ctx.Done():
			}
		}
	}
}
func readDisplayCookie(path, display string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	r := bytes.NewReader(b)
	read := func() ([]byte, error) {
		var n uint16
		if e := binary.Read(r, binary.BigEndian, &n); e != nil {
			return nil, e
		}
		b := make([]byte, n)
		_, e := io.ReadFull(r, b)
		return b, e
	}
	for r.Len() > 0 {
		var family uint16
		if err = binary.Read(r, binary.BigEndian, &family); err != nil {
			return "", err
		}
		_, e := read()
		if e != nil {
			return "", e
		}
		num, e := read()
		if e != nil {
			return "", e
		}
		name, e := read()
		if e != nil {
			return "", e
		}
		cookie, e := read()
		if e != nil {
			return "", e
		}
		if (string(num) == strings.TrimPrefix(display, ":") || len(num) == 0) && string(name) == "MIT-MAGIC-COOKIE-1" && len(cookie) == 16 {
			return hex.EncodeToString(cookie), nil
		}
	}
	return "", fmt.Errorf("no display authorization cookie")
}

type inputConnection struct {
	c       *xgb.Conn
	root    xproto.Window
	state   desktopState
	keys    map[uint32]byte
	held    map[byte]bool
	buttons map[byte]bool
}

func openInputConnection(s desktopState) (*inputConnection, error) {
	conn, err := net.DialTimeout("unix", filepath.Join("/tmp/.X11-unix", "X"+strings.TrimPrefix(s.display, ":")), time.Second)
	if err != nil {
		return nil, err
	}
	cookie, _ := readDisplayCookie(s.authority, s.display)
	var c *xgb.Conn
	if cookie != "" {
		c, err = xgb.NewConnNetWithCookieHex(conn, cookie)
	} else {
		c, err = xgb.NewConnNet(conn)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = xtest.Init(c); err != nil {
		c.Close()
		return nil, err
	}
	setup := xproto.Setup(c)
	if len(setup.Roots) == 0 {
		c.Close()
		return nil, fmt.Errorf("display has no screen")
	}
	mapping, err := xproto.GetKeyboardMapping(c, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err != nil {
		c.Close()
		return nil, err
	}
	keys := map[uint32]byte{}
	for key := int(setup.MinKeycode); key <= int(setup.MaxKeycode); key++ {
		for col := 0; col < int(mapping.KeysymsPerKeycode); col++ {
			sym := mapping.Keysyms[(key-int(setup.MinKeycode))*int(mapping.KeysymsPerKeycode)+col]
			if sym != 0 {
				if _, ok := keys[uint32(sym)]; !ok {
					keys[uint32(sym)] = byte(key)
				}
			}
		}
	}
	return &inputConnection{c: c, root: setup.Roots[0].Root, state: s, keys: keys, held: map[byte]bool{}, buttons: map[byte]bool{}}, nil
}
func (c *inputConnection) releaseKeys() {
	for k := range c.held {
		xtest.FakeInput(c.c, xproto.KeyRelease, k, 0, c.root, 0, 0, 0)
	}
	c.held = map[byte]bool{}
	c.c.Sync()
}
func (c *inputConnection) close() {
	c.releaseKeys()
	for b := range c.buttons {
		xtest.FakeInput(c.c, xproto.ButtonRelease, b, 0, c.root, 0, 0, 0)
	}
	c.c.Sync()
	c.c.Close()
}

var macKeysyms = map[int]uint32{
	0: 'a', 1: 's', 2: 'd', 3: 'f', 4: 'h', 5: 'g', 6: 'z', 7: 'x', 8: 'c', 9: 'v', 10: 0xa7, 11: 'b', 12: 'q', 13: 'w', 14: 'e', 15: 'r', 16: 'y', 17: 't', 18: '1', 19: '2', 20: '3', 21: '4', 22: '6', 23: '5', 24: '=', 25: '9', 26: '7', 27: '-', 28: '8', 29: '0', 30: ']', 31: 'o', 32: 'u', 33: '[', 34: 'i', 35: 'p', 36: 0xff0d, 37: 'l', 38: 'j', 39: '\'', 40: 'k', 41: ';', 42: '\\', 43: ',', 44: '/', 45: 'n', 46: 'm', 47: '.', 48: 0xff09, 49: ' ', 50: '`', 51: 0xff08, 53: 0xff1b, 54: 0xffec, 55: 0xffeb, 56: 0xffe1, 57: 0xffe5, 58: 0xffe9, 59: 0xffe3, 60: 0xffe2, 61: 0xffea, 62: 0xffe4,
	65: 0xffae, 67: 0xffaa, 69: 0xffab, 71: 0xff7f, 75: 0xffaf, 76: 0xff8d, 78: 0xffad, 81: 0xffbd, 82: 0xffb0, 83: 0xffb1, 84: 0xffb2, 85: 0xffb3, 86: 0xffb4, 87: 0xffb5, 88: 0xffb6, 89: 0xffb7, 91: 0xffb8, 92: 0xffb9,
	96: 0xffc2, 97: 0xffc3, 98: 0xffc4, 99: 0xffc0, 100: 0xffc5, 101: 0xffc6, 103: 0xffc8, 105: 0xffca, 106: 0xffcd, 107: 0xffcb, 109: 0xffc7, 111: 0xffc9, 113: 0xffcc, 114: 0xff63, 115: 0xff50, 116: 0xff55, 117: 0xffff, 118: 0xffc1, 119: 0xff57, 120: 0xffbf, 121: 0xff56, 122: 0xffbe, 123: 0xff51, 124: 0xff53, 125: 0xff54, 126: 0xff52,
}

func mouseButton(raw json.RawMessage) byte {
	var n int
	if json.Unmarshal(raw, &n) != nil {
		var s string
		json.Unmarshal(raw, &s)
		switch strings.ToLower(s) {
		case "right":
			n = 1
		case "middle":
			n = 2
		default:
			n = 0
		}
	}
	switch n {
	case 0:
		return 1
	case 1:
		return 3
	case 2:
		return 2
	default:
		return byte(n + 1)
	}
}
func screenPoint(s desktopState, x, y float64) (int16, int16) {
	scale := math.Min(1280/float64(s.width), 720/float64(s.height))
	px := (x*1280 - (1280-float64(s.width)*scale)/2) / scale
	py := (y*720 - (720-float64(s.height)*scale)/2) / scale
	px = math.Max(0, math.Min(float64(s.width-1), px))
	py = math.Max(0, math.Min(float64(s.height-1), py))
	return int16(px), int16(py)
}
func (c *inputConnection) apply(e inputEvent) {
	switch e.Action {
	case "mouse_move_absolute":
		x, y := screenPoint(c.state, e.AbsX, e.AbsY)
		xtest.FakeInput(c.c, xproto.MotionNotify, 0, 0, c.root, x, y, 0)
	case "mouse_move_relative", "mouse_move_variety_relative":
		xtest.FakeInput(c.c, xproto.MotionNotify, 1, 0, c.root, int16(e.RelX), int16(e.RelY), 0)
	case "mouse_press", "mouse_release", "mouse_click":
		b := mouseButton(e.Button)
		if e.Action != "mouse_release" {
			xtest.FakeInput(c.c, xproto.ButtonPress, b, 0, c.root, 0, 0, 0)
			c.buttons[b] = true
		}
		if e.Action != "mouse_press" {
			xtest.FakeInput(c.c, xproto.ButtonRelease, b, 0, c.root, 0, 0, 0)
			delete(c.buttons, b)
		}
	case "kbd_press", "kbd_release", "kbd_click":
		sym, ok := macKeysyms[e.Key]
		if !ok {
			return
		}
		key, ok := c.keys[sym]
		if !ok {
			return
		}
		if e.Action != "kbd_release" {
			xtest.FakeInput(c.c, xproto.KeyPress, key, 0, c.root, 0, 0, 0)
			c.held[key] = true
		}
		if e.Action != "kbd_press" {
			xtest.FakeInput(c.c, xproto.KeyRelease, key, 0, c.root, 0, 0, 0)
			delete(c.held, key)
		}
	case "mouse_scroll":
		for _, axis := range []struct {
			delta              float64
			positive, negative byte
		}{{e.DeltaY, 4, 5}, {e.DeltaX, 7, 6}} {
			if axis.delta == 0 {
				continue
			}
			b := axis.positive
			if axis.delta < 0 {
				b = axis.negative
			}
			count := int(math.Min(20, math.Max(1, math.Ceil(math.Abs(axis.delta)/10))))
			for j := 0; j < count; j++ {
				xtest.FakeInput(c.c, xproto.ButtonPress, b, 0, c.root, 0, 0, 0)
				xtest.FakeInput(c.c, xproto.ButtonRelease, b, 0, c.root, 0, 0, 0)
			}
		}
	}
	c.c.Sync()
}
func (i *inputEngine) run() {
	var connection *inputConnection
	defer func() {
		if connection != nil {
			connection.close()
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastKey := time.Now()
	for {
		select {
		case <-i.peer.ctx.Done():
			return
		case <-ticker.C:
			if connection != nil && len(connection.held) > 0 && time.Since(lastKey) > 3*time.Second {
				connection.releaseKeys()
			}
		case e := <-i.events:
			if e.Heartbeat != "" || strings.HasPrefix(e.Action, "kbd_") {
				lastKey = time.Now()
			}
			if e.Action == "" {
				continue
			}
			i.peer.mu.Lock()
			d := i.peer.desktop
			i.peer.mu.Unlock()
			if d == nil {
				continue
			}
			d.mu.Lock()
			state := d.state
			d.mu.Unlock()
			if connection != nil && connection.state != state {
				connection.close()
				connection = nil
			}
			if state.display == "" {
				continue
			}
			if connection == nil {
				var err error
				connection, err = openInputConnection(state)
				if err != nil {
					log.Printf("desktop input connect: %v", err)
					continue
				}
				log.Printf("desktop input connected display=%s", state.display)
			}
			if e.Action == "text_input" && e.Text != "" {
				cmd := exec.CommandContext(i.peer.ctx, "xdotool", "type", "--clearmodifiers", "--delay", "1", "--", e.Text)
				cmd.Env = append(os.Environ(), "DISPLAY="+state.display, "XAUTHORITY="+state.authority)
				if err := cmd.Run(); err != nil {
					log.Printf("desktop text input: %v", err)
				}
				continue
			}
			connection.apply(e)
		}
	}
}
