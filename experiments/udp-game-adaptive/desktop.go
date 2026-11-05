package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

//go:embed assets/no-desktop.png
var noDesktopPNG []byte

// A single independent IDR frame: minimal installations need no encoder.
//
//go:embed assets/no-desktop.h264
var noDesktopH264 []byte

type desktopState struct {
	display, authority string
	width, height      int
}

var geometryPattern = regexp.MustCompile(`dimensions:\s+(\d+)x(\d+)`)

func detectDesktop(ctx context.Context) desktopState {
	sockets, _ := filepath.Glob("/tmp/.X11-unix/X*")
	auth := map[string]string{}
	processes, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, p := range processes {
		b, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		name := strings.TrimSpace(string(b))
		if name != "xfce4-session" && name != "gnome-shell" && name != "plasmashell" && name != "Xorg" && name != "Xvfb" {
			continue
		}
		env, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "environ"))
		values := map[string]string{}
		for _, v := range bytes.Split(env, []byte{0}) {
			k, value, ok := strings.Cut(string(v), "=")
			if ok && (k == "DISPLAY" || k == "XAUTHORITY") {
				values[k] = value
			}
		}
		if values["DISPLAY"] != "" {
			auth[strings.Split(values["DISPLAY"], ".")[0]] = values["XAUTHORITY"]
		}
	}
	for _, socket := range sockets {
		display := ":" + strings.TrimPrefix(filepath.Base(socket), "X")
		c, cancel := context.WithTimeout(ctx, time.Second)
		cmd := exec.CommandContext(c, "xdpyinfo", "-display", display)
		cmd.Env = append(os.Environ(), "DISPLAY="+display, "XAUTHORITY="+auth[display])
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		m := geometryPattern.FindSubmatch(out)
		if len(m) == 3 {
			w, _ := strconv.Atoi(string(m[1]))
			h, _ := strconv.Atoi(string(m[2]))
			if w > 0 && h > 0 {
				return desktopState{display, auth[display], w, h}
			}
		}
	}
	return desktopState{}
}

type desktopStream struct {
	peer      *protocolPeer
	track     *webrtc.TrackLocalStaticSample
	once      sync.Once
	mu        sync.Mutex
	state     desktopState
	requested bool
}

func (p *protocolPeer) setupDesktop(pc *webrtc.PeerConnection) error {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, "video0", "uulink")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.desktop = &desktopStream{peer: p, track: track}
	p.mu.Unlock()
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	return nil
}
func (p *protocolPeer) startVideo() {
	p.mu.Lock()
	d := p.desktop
	p.mu.Unlock()
	if d != nil {
		d.mu.Lock()
		requested := d.requested
		d.mu.Unlock()
		if requested {
			d.once.Do(func() { go d.run() })
		}
	}
}
func (p *protocolPeer) requestVideo() {
	p.mu.Lock()
	d := p.desktop
	p.mu.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	d.requested = true
	d.mu.Unlock()
	p.startVideo()
}
func (p *protocolPeer) announceDesktop() {
	p.mu.Lock()
	d := p.desktop
	p.mu.Unlock()
	if d == nil {
		return
	}
	resolution := putVar(nil, 3, 1280)
	resolution = putVar(resolution, 4, 720)
	resolution = putVar(resolution, 5, 1280)
	resolution = putVar(resolution, 6, 720)
	screen := putVar(nil, 1, 1)
	screen = putVar(screen, 2, 1)
	screen = putBytes(screen, 3, resolution)
	screen = putBytes(screen, 4, resolution)
	screen = putVar(screen, 5, 1)
	screen = putBytes(screen, 6, resolution)
	screen = putVar(screen, 7, 1)
	screen = binary.AppendUvarint(screen, 8<<3|1)
	screen = binary.LittleEndian.AppendUint64(screen, math.Float64bits(1))
	dpi := putVar(nil, 1, 96)
	dpi = putVar(dpi, 2, 96)
	dpi = putVar(dpi, 3, 96)
	screen = putBytes(screen, 9, dpi)
	screen = putBytes(screen, 10, []byte("Linux Desktop"))
	screen = putVar(screen, 12, 0)
	screens := putBytes(nil, 1, screen)
	screens = putVar(screens, 2, 1)
	p.send("TEXT_DATA_CHANNEL", 7, screens)
	change := putVar(nil, 2, 1)
	change = putBytes(change, 3, []byte("Linux Desktop"))
	p.send("TEXT_DATA_CHANNEL", 8, change)
}
func (d *desktopStream) run() {
	if !d.peer.features.desktop {
		d.peer.announceDesktop()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := d.track.WriteSample(media.Sample{Data: noDesktopH264, Duration: time.Second}); err != nil {
				return
			}
			select {
			case <-d.peer.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	ctx := d.peer.ctx
	f, err := os.CreateTemp("", "uulink-no-desktop-*.png")
	if err != nil {
		log.Printf("video placeholder: %v", err)
		return
	}
	png := f.Name()
	defer os.Remove(png)
	if _, err = f.Write(noDesktopPNG); err != nil {
		f.Close()
		return
	}
	f.Close()
	for ctx.Err() == nil {
		state := detectDesktop(ctx)
		d.mu.Lock()
		d.state = state
		d.mu.Unlock()
		source := "no desktop"
		fps := 5
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
		if state.display != "" {
			source = state.display
			fps = 15
			args = append(args, "-f", "x11grab", "-framerate", strconv.Itoa(fps), "-video_size", fmt.Sprintf("%dx%d", state.width, state.height), "-i", state.display)
		} else {
			args = append(args, "-re", "-loop", "1", "-framerate", strconv.Itoa(fps), "-i", png)
		}
		args = append(args, "-an", "-vf", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,format=yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-r", strconv.Itoa(fps), "-g", strconv.Itoa(fps), "-keyint_min", strconv.Itoa(fps), "-x264-params", "repeat-headers=1:aud=1:scenecut=0", "-f", "h264", "pipe:1")
		videoCtx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(videoCtx, "ffmpeg", args...)
		cmd.Env = append(os.Environ(), "DISPLAY="+state.display, "XAUTHORITY="+state.authority)
		stdout, e := cmd.StdoutPipe()
		if e != nil {
			cancel()
			return
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if e = cmd.Start(); e != nil {
			cancel()
			log.Printf("video encoder: %v", e)
			return
		}
		log.Printf("desktop video source=%s output=1280x720 fps=%d", source, fps)
		d.peer.announceDesktop()
		stopped := make(chan struct{})
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-videoCtx.Done():
					return
				case <-stopped:
					return
				case <-ticker.C:
					next := detectDesktop(videoCtx)
					if next != state {
						cancel()
						return
					}
				}
			}
		}()
		reader, e := h264reader.NewReader(stdout)
		if e == nil {
			var au []byte
			for {
				nal, e := reader.NextNAL()
				if e != nil {
					break
				}
				typ := nal.Data[0] & 31
				if typ == 9 && len(au) > 0 {
					if e = d.track.WriteSample(media.Sample{Data: au, Duration: time.Second / time.Duration(fps)}); e != nil {
						break
					}
					au = nil
				}
				au = append(au, 0, 0, 0, 1)
				au = append(au, nal.Data...)
			}
		}
		close(stopped)
		cancel()
		cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if stderr.Len() > 0 {
			log.Printf("video encoder stopped: %.400s", stderr.String())
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
