package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// The application owns its connection engine as a child process. Provider
// globals used for account requests are serialized independently.
//
//go:embed gui_assets
var guiAssets embed.FS

type guiOptions struct{ address, controlled, controller, rules, state, controllerState, sessionDir, gatewayLog string }
type guiServer struct {
	opts        guiOptions
	key, origin string
	apiMu       sync.Mutex
	mu          sync.Mutex
	opsMu       sync.Mutex
	connection  *guiAppRuntime
	pending     map[string]guiPending
}
type guiPending struct {
	Config   agentConfig
	Phone    string
	Until    time.Time
	Original string
}
type guiRole struct {
	Role, Name, DeviceID, Path, Error string
	LoggedIn, Writable                bool
}
type guiService struct {
	Mode                        string
	State, Manager, Name, Error string
	PID                         int
	Installed, AutoStart        bool
	started                     time.Time
}
type guiMapping struct {
	forwardRule
	State string `json:"state"`
}
type guiSnapshot struct {
	LocalAddresses                    []string
	Version, Platform, Hostname, CSRF string
	Connection                        guiConnectionConfig
	Network                           providerNetworkInfo
	Latencies                         map[string]tunnelLatency
	DeviceConnections                 map[string]guiDeviceConnection
	Roles                             []guiRole
	Service                           guiService
	Mappings                          []guiMapping
	RulesPath, RulesError             string
	ControllerRunning                 bool
	Time                              time.Time
}

func guiCLI(args []string) error { return runGUI(context.Background(), args, nil) }

func runGUI(parent context.Context, args []string, ready chan<- string) error {
	home, _ := os.UserHomeDir()
	o := guiOptions{controlled: configDefault, controller: controllerConfigPath, rules: controllerRulesPath, state: clientStateDir, controllerState: controllerStateDir, sessionDir: filepath.Join(home, ".config", "uulink-ui"), gatewayLog: filepath.Join(home, ".local", "share", "uulink-apex", "logs", "uu-game.log")}
	if runtime.GOOS == "darwin" {
		o.controlled = filepath.Join(o.sessionDir, "controlled.json")
		o.controller = filepath.Join(home, ".config", "uulink-apex", "controller.json")
		o.rules = filepath.Join(o.sessionDir, "rules.json")
		o.controllerState = filepath.Join(home, ".local", "share", "uulink-ui", "controller")
	}
	if runtime.GOOS == "windows" {
		var err error
		o, err = windowsGUIOptions()
		if err != nil {
			return err
		}
	}
	f := flag.NewFlagSet("gui", flag.ContinueOnError)
	f.StringVar(&o.address, "listen", "127.0.0.1:17633", "loopback interface for the visual panel")
	f.StringVar(&o.controlled, "config", o.controlled, "controlled identity")
	f.StringVar(&o.controller, "controller-config", o.controller, "controller identity")
	f.StringVar(&o.rules, "rules", o.rules, "controller rules")
	f.StringVar(&o.state, "state-dir", o.state, "unified service status")
	f.StringVar(&o.controllerState, "controller-state-dir", o.controllerState, "controller status")
	f.StringVar(&o.sessionDir, "session-dir", o.sessionDir, "private panel state")
	f.StringVar(&o.gatewayLog, "gateway-log", o.gatewayLog, "UU gateway log")
	parentPID := f.Int("parent-pid", 0, "owning native application PID")
	noOpen := f.Bool("no-open", false, "do not open the browser")
	if err := f.Parse(args); err != nil {
		return err
	}
	var pathErr error
	o.controlled, pathErr = guiCanonicalPath(o.controlled)
	if pathErr != nil {
		return pathErr
	}
	o.controller, pathErr = guiCanonicalPath(o.controller)
	if pathErr != nil {
		return pathErr
	}
	if o.controlled == o.controller {
		return fmt.Errorf("两种身份必须使用独立配置文件")
	}
	h, _, err := net.SplitHostPort(o.address)
	if err != nil || net.ParseIP(h) == nil || !net.ParseIP(h).IsLoopback() {
		return fmt.Errorf("管理界面只允许监听本机回环 IP")
	}
	if err = os.MkdirAll(o.sessionDir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(o.sessionDir, 0700); err != nil {
		return err
	}
	lock, err := controllerLock(filepath.Join(o.sessionDir, "ui.lock"), true)
	if err != nil {
		var record struct {
			URL string
			Key string
			PID int
		}
		b, e := os.ReadFile(filepath.Join(o.sessionDir, "session.json"))
		if e == nil && json.Unmarshal(b, &record) == nil && record.PID > 0 {
			u, e := url.Parse(record.URL)
			if e == nil && u.Scheme == "http" && u.Host == o.address && u.Path == "/" && len(record.Key) == 64 {
				r, _ := http.NewRequest("GET", "http://"+u.Host+"/api/health", nil)
				r.Header.Set("X-UULink-Key", record.Key)
				client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				resp, e := client.Do(r)
				if e == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						if !*noOpen {
							guiOpen(record.URL)
						}
						fmt.Println("UU Link 界面已在运行：http://" + u.Host)
						return nil
					}
				}
			}
		}
		return err
	}
	defer lock.Close()
	listener, err := net.Listen("tcp", o.address)
	if err != nil {
		return err
	}
	defer listener.Close()
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *parentPID > 0 {
		if os.Getppid() != *parentPID {
			return fmt.Errorf("所属应用已经退出")
		}
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if os.Getppid() != *parentPID {
						stop()
						return
					}
				}
			}
		}()
	}
	g := &guiServer{connection: &guiAppRuntime{ctx: ctx}, opts: o, key: hex.EncodeToString(buf), origin: "http://" + listener.Addr().String(), pending: map[string]guiPending{}}
	if probePath := filepath.Join(g.opts.controllerState, "probes.json"); len(readProbeRequests(probePath)) > 0 {
		if err = atomicControllerJSON(probePath, map[string]time.Time{}); err != nil {
			return err
		}
	}
	if err = g.migrateMappings(); err != nil {
		return fmt.Errorf("无法加载映射配置: %w", err)
	}
	link := g.origin + "/#key=" + g.key
	if err = atomicControllerJSON(filepath.Join(o.sessionDir, "session.json"), map[string]any{"URL": link, "Key": g.key, "PID": os.Getpid()}); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(o.sessionDir, "session.json"))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", g.handle)
	assets, _ := fs.Sub(guiAssets, "gui_assets")
	mux.Handle("/", http.FileServer(http.FS(assets)))
	srv := &http.Server{Handler: g.guard(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 45 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	defer g.stopConnection()
	if c, e := g.connectionConfig(); e == nil && c.ConnectOnOpen {
		if e = g.startConnection(); e != nil {
			g.connection.lastError = e.Error()
		}
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		// Connection ownership ends immediately, even while an unrelated
		// account/device request is still unwinding.
		g.stopConnection()
		c, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		srv.Shutdown(c)
		g.opsMu.Lock()
		g.stopConnection()
		g.opsMu.Unlock()
	}()
	fmt.Println("UU Link 可视化界面：" + g.origin)
	if runtime.GOOS == "windows" {
		fmt.Println("关闭窗口退出应用并断开连接。")
	} else {
		fmt.Println("应用管理连接引擎；关闭窗口保留菜单栏，退出应用断开连接。")
	}
	if !*noOpen {
		guiOpen(link)
	}
	if ready != nil {
		ready <- link
	}
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func (g *guiServer) authenticated(r *http.Request) bool {
	v := r.Header.Get("X-UULink-Key")
	if v == "" {
		if c, e := r.Cookie("uulink_ui"); e == nil {
			v = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(v), []byte(g.key)) == 1
}
func (g *guiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Host != strings.TrimPrefix(g.origin, "http://") {
			http.Error(w, "Host 不匹配", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/session" {
			if !g.authenticated(r) {
				guiJSON(w, 401, map[string]string{"error": "请从 UU Link 应用或本机认证链接打开界面"})
				return
			}
			if r.Method != "GET" && (r.Header.Get("Origin") != g.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-UULink-CSRF")), []byte(g.key)) != 1) {
				guiJSON(w, 403, map[string]string{"error": "请求来源不匹配"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func guiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func guiBody(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("需要 JSON 请求")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return fmt.Errorf("请求格式错误")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("只允许一份 JSON 请求")
	}
	return nil
}
func (g *guiServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.URL.Path != "/api/session" {
		g.opsMu.Lock()
		defer g.opsMu.Unlock()
	}
	var data any
	var err error
	switch {
	case r.URL.Path == "/api/session" && r.Method == "POST":
		var b struct {
			Key string `json:"key"`
		}
		err = guiBody(w, r, &b)
		if err == nil && (r.Header.Get("Origin") != g.origin || subtle.ConstantTimeCompare([]byte(b.Key), []byte(g.key)) != 1) {
			guiJSON(w, 403, map[string]string{"error": "认证链接无效，请重新打开 UU Link"})
			return
		}
		if err == nil {
			http.SetCookie(w, &http.Cookie{Name: "uulink_ui", Value: g.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			data = map[string]bool{"ok": true}
		}
	case r.URL.Path == "/api/platform/preferences":
		data, err = platformPreferences(w, r)
	case r.URL.Path == "/api/health" && r.Method == "GET":
		data = map[string]bool{"ok": true}
	case r.URL.Path == "/api/state" && r.Method == "GET":
		data = g.snapshot()
	case r.URL.Path == "/api/logs" && r.Method == "GET":
		data = map[string]string{"text": guiSanitize(g.logText())}
	case r.URL.Path == "/api/devices" && r.Method == "GET":
		g.apiMu.Lock()
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		data, err = controllerDevices(ctx, g.opts.controller)
		cancel()
		g.apiMu.Unlock()
	case strings.HasPrefix(r.URL.Path, "/api/latency/") && r.Method == "POST":
		device := strings.TrimPrefix(r.URL.Path, "/api/latency/")
		if !safeControllerID.MatchString(device) {
			err = fmt.Errorf("无效设备 ID")
		} else if err = g.requireOnlineDevice(r.Context(), device); err == nil {
			path := filepath.Join(g.opts.controllerState, "probes.json")
			requests := activeProbeRequests(path)
			if len(requests) >= 16 && requests[device].IsZero() {
				err = fmt.Errorf("同时最多测量 16 台设备")
			} else {
				requests[device] = time.Now().Add(30 * time.Second)
				err = atomicControllerJSON(path, requests)
				if err == nil {
					err = g.startLatencyProbe()
				}
			}
		}
		data = map[string]bool{"ok": err == nil}
	case r.URL.Path == "/api/rules" && r.Method == "POST":
		var b forwardRule
		err = guiBody(w, r, &b)
		if err == nil {
			err = g.saveRule(b)
			if err == nil && b.Enabled {
				g.startSavedMappings()
			}
			data = map[string]bool{"ok": true}
		}
	case strings.HasPrefix(r.URL.Path, "/api/rules/") && (r.Method == "PATCH" || r.Method == "DELETE"):
		id := strings.TrimPrefix(r.URL.Path, "/api/rules/")
		var b struct {
			Enabled bool `json:"enabled"`
		}
		if !safeControllerID.MatchString(id) {
			err = fmt.Errorf("无效规则 ID")
		} else if r.Method == "PATCH" {
			err = guiBody(w, r, &b)
		}
		if err == nil {
			err = updateRules(g.opts.rules, func(doc *ruleDocument) error {
				for i, x := range doc.Rules {
					if x.ID == id {
						if r.Method == "DELETE" {
							doc.Rules = append(doc.Rules[:i], doc.Rules[i+1:]...)
						} else {
							doc.Rules[i].Enabled = b.Enabled
						}
						return nil
					}
				}
				return fmt.Errorf("规则已不存在，请刷新")
			})
			if err == nil && r.Method == "PATCH" && b.Enabled {
				g.startSavedMappings()
			}
			data = map[string]bool{"ok": true}
		}
	case r.URL.Path == "/api/connection" && r.Method == "POST":
		var b struct {
			Action string `json:"action"`
		}
		err = guiBody(w, r, &b)
		if err == nil {
			err = g.control(b.Action)
			data = map[string]bool{"ok": true}
		}
	case r.URL.Path == "/api/preferences" && r.Method == "POST":
		var b struct {
			Enabled bool `json:"enabled"`
		}
		err = guiBody(w, r, &b)
		if err == nil {
			err = g.setConnectOnOpen(b.Enabled)
			data = map[string]bool{"ok": true}
		}
	case r.URL.Path == "/api/login/code" && r.Method == "POST":
		var b struct {
			Role  string `json:"role"`
			Phone string `json:"phone"`
			Name  string `json:"name"`
		}
		err = guiBody(w, r, &b)
		if err == nil {
			err = g.sendCode(r.Context(), b.Role, b.Phone, b.Name)
			data = map[string]bool{"ok": true}
		}
	case r.URL.Path == "/api/login/verify" && r.Method == "POST":
		var b struct {
			Role string `json:"role"`
			Code string `json:"code"`
		}
		err = guiBody(w, r, &b)
		if err == nil {
			err = g.verifyCode(r.Context(), b.Role, b.Code)
			data = map[string]bool{"ok": true}
		}
	default:
		guiJSON(w, 404, map[string]string{"error": "没有这个接口"})
		return
	}
	if err != nil {
		guiJSON(w, 400, map[string]string{"error": guiSanitize(err.Error())})
		return
	}
	guiJSON(w, 200, data)
}

// Recheck provider state before creating a probe; the panel's device list may
// already be stale. Unknown status and missing devices must not start probes.
func (g *guiServer) requireOnlineDevice(ctx context.Context, id string) error {
	g.apiMu.Lock()
	defer g.apiMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	devices, err := controllerDevices(ctx, g.opts.controller)
	if err != nil {
		return fmt.Errorf("无法确认设备在线状态：%w", err)
	}
	for _, d := range devices {
		if d.ID != id {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(d.Status)) {
		case "online", "connected", "active", "available":
			return nil
		default:
			return fmt.Errorf("设备未在线，无法测量延迟")
		}
	}
	return fmt.Errorf("设备不在当前列表，无法测量延迟")
}

func guiWritable(path string) bool {
	p := filepath.Dir(path)
	for {
		if _, e := os.Stat(p); e == nil {
			return writableGUIDirectory(p)
		}
		n := filepath.Dir(p)
		if n == p {
			return false
		}
		p = n
	}
}
func (g *guiServer) rolePath(role string) (string, error) {
	switch role {
	case "controlled":
		if runtime.GOOS == "windows" {
			return "", fmt.Errorf("Windows 版仅提供端口转发控制端")
		}
		return g.opts.controlled, nil
	case "controller":
		return g.opts.controller, nil
	}
	return "", fmt.Errorf("请选择控制端或被控端身份")
}
func (g *guiServer) roles() []guiRole {
	var out []guiRole
	for _, role := range []string{"controller", "controlled"} {
		p, pathErr := g.rolePath(role)
		if pathErr != nil {
			continue
		}
		c, e := readConfig(p)
		v := guiRole{Role: role, Name: c.Name, DeviceID: c.DeviceID, Path: p, Writable: guiWritable(p), LoggedIn: c.Bearer != "" && c.UserID != "" && c.ClientID != "" && c.DeviceID != ""}
		if e != nil && !os.IsNotExist(e) {
			v.Error = "身份文件无法读取"
		}
		out = append(out, v)
	}
	return out
}
func (g *guiServer) saveRule(b forwardRule) error {
	if b.Protocol == "" {
		b.Protocol = "tcp+udp"
	}
	if b.AllowLAN == nil {
		return fmt.Errorf("请选择是否允许局域网访问")
	}
	h, _, e := net.SplitHostPort(b.Listen)
	if e != nil {
		return fmt.Errorf("无效的本机入口")
	}
	ip := net.ParseIP(h)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("本机入口需要 IPv4 地址")
	}
	if !*b.AllowLAN && !ip.IsLoopback() {
		return fmt.Errorf("仅本机访问必须使用 127.0.0.1")
	}
	if !ip.IsUnspecified() && !ip.IsLoopback() {
		found := false
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			local, _, _ := net.ParseCIDR(a.String())
			if local.Equal(ip) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("入口地址不属于本机")
		}
	}
	if b.ID == "" {
		b.ID = uuid.NewString()
	}
	return updateRules(g.opts.rules, func(doc *ruleDocument) error {
		found := -1
		for i, x := range doc.Rules {
			if x.ID == b.ID {
				found = i
			}
		}
		probeTCP, probeUDP := b.Enabled && b.transport() != "udp", b.Enabled && b.transport() != "tcp"
		if found >= 0 && doc.Rules[found].Enabled {
			old := doc.Rules[found]
			_, oldPort, _ := net.SplitHostPort(old.Listen)
			_, newPort, _ := net.SplitHostPort(b.Listen)
			if oldPort == newPort {
				if old.transport() != "udp" {
					probeTCP = false
				}
				if old.transport() != "tcp" {
					probeUDP = false
				}
			}
		}
		if found < 0 {
			doc.Rules = append(doc.Rules, b)
		} else {
			doc.Rules[found] = b
		}
		if e := validateRules(*doc); e != nil {
			return e
		}
		if probeTCP || probeUDP {
			if probeTCP {
				l, e := net.Listen("tcp4", b.Listen)
				if e != nil {
					return fmt.Errorf("TCP 端口不可用: %s", b.Listen)
				}
				defer l.Close()
			}
			if probeUDP {
				a, e := net.ResolveUDPAddr("udp4", b.Listen)
				if e != nil {
					return e
				}
				l, e := net.ListenUDP("udp4", a)
				if e != nil {
					return fmt.Errorf("UDP 端口不可用: %s", b.Listen)
				}
				defer l.Close()
			}
		}
		return nil
	})
}
func guiCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if e != nil {
		return string(b), fmt.Errorf("操作失败：%s", strings.TrimSpace(string(b)))
	}
	return string(b), nil
}
func (g *guiServer) logText() string {
	p := g.opts.gatewayLog
	f, e := os.Open(p)
	if e != nil {
		return ""
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return ""
	}
	if st.Size() > 192<<10 {
		f.Seek(st.Size()-(192<<10), io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, 192<<10))
	return string(b)
}

func guiSanitize(s string) string {
	s = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._+/=-]+`).ReplaceAllString(s, "Bearer [已隐藏]")
	s = regexp.MustCompile(`(?i)("?(?:password|bearer|token|secret|authorization)"?\s*[:=]\s*)[^\s,}]+`).ReplaceAllString(s, "${1}[已隐藏]")
	return s
}
func (g *guiServer) snapshot() guiSnapshot {
	host, _ := os.Hostname()
	s := guiSnapshot{Version: version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Hostname: host, CSRF: g.key, Roles: g.roles(), Service: g.service(), Mappings: []guiMapping{}, RulesPath: g.opts.rules, Time: time.Now()}
	s.Connection, _ = g.connectionConfig()
	s.Network = providerNetworkStatus()
	s.Latencies = readTunnelLatencies(g.opts.controllerState)
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, a := range addresses {
			ip, _, _ := net.ParseCIDR(a.String())
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				s.LocalAddresses = append(s.LocalAddresses, ip.String())
			}
		}
	}
	d, e := readRules(g.opts.rules)
	if e != nil && !os.IsNotExist(e) {
		s.RulesError = "规则文件读取失败，现有文件不会被覆盖"
	}
	daemon := readControllerStatus(filepath.Join(g.opts.controllerState, "daemon.json"))
	daemonLive := !s.Service.started.IsZero() && !daemon.Updated.Before(s.Service.started) && strings.HasPrefix(daemon.State, "运行中，")
	s.ControllerRunning = s.Service.State == "running" && daemonLive
	reports := map[string]controllerStatus{}
	if daemonLive {
		wanted := activeProbeRequests(filepath.Join(g.opts.controllerState, "probes.json"))
		if s.ControllerRunning {
			for _, rule := range d.Rules {
				if rule.Enabled && safeControllerID.MatchString(rule.Device) {
					wanted[rule.Device] = time.Time{}
				}
			}
		}
		for device := range wanted {
			report := readControllerStatus(filepath.Join(g.opts.controllerState, device+".json"))
			if !report.Updated.Before(s.Service.started) {
				reports[device] = report
			}
		}
	}
	s.DeviceConnections = readDeviceConnections(g.opts.controllerState, reports, s.Time)
	for _, v := range d.Rules {
		st := "已停用"
		if v.Enabled {
			st = "等待连接"
			if s.Service.Error != "" && s.Service.State != "running" {
				st = s.Service.Error
			}
			if s.ControllerRunning {
				report, ok := reports[v.Device]
				if !ok {
					report.State = "状态未更新"
				}
				st = report.State
				if report.Error != "" {
					st = report.Error
				}
				if a := report.Rules[v.ID]; a != "" {
					st = a
				}
			}
		}
		s.Mappings = append(s.Mappings, guiMapping{v, st})
	}
	return s
}
func (g *guiServer) machineID() (string, error) {
	if runtime.GOOS == "darwin" {
		b, e := guiCommand("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if e != nil {
			return "", e
		}
		for _, l := range strings.Split(b, "\n") {
			if strings.Contains(l, `"IOPlatformUUID"`) {
				a := strings.Split(l, `"`)
				if len(a) > 3 {
					return a[3], nil
				}
			}
		}
		return "", fmt.Errorf("无法读取本机身份")
	}
	if m := machineID(); m != "" {
		return m, nil
	}
	p := filepath.Join(g.opts.sessionDir, "machine-id")
	if b, e := os.ReadFile(p); e == nil {
		return strings.TrimSpace(string(b)), nil
	}
	m := uuid.NewString()
	return m, os.WriteFile(p, []byte(m+"\n"), 0600)
}
func (g *guiServer) sendCode(ctx context.Context, role, phone, name string) error {
	if !regexp.MustCompile(`^\+?[0-9]{6,20}$`).MatchString(phone) {
		return fmt.Errorf("请输入有效手机号")
	}
	path, e := g.rolePath(role)
	if e != nil {
		return e
	}
	if !guiWritable(path) {
		return fmt.Errorf("此身份配置需要管理员权限")
	}
	g.apiMu.Lock()
	defer g.apiMu.Unlock()
	g.mu.Lock()
	p, ok := g.pending[role]
	g.mu.Unlock()
	if ok && time.Until(p.Until) > 4*time.Minute {
		return fmt.Errorf("验证码刚刚发送，请稍后再试")
	}
	original, e := guiFingerprint(path)
	if e != nil {
		return e
	}
	c, e := readConfig(path)
	if e != nil && !os.IsNotExist(e) {
		return fmt.Errorf("身份文件无法读取，未覆盖")
	}
	mid, e := g.machineID()
	if e != nil {
		return e
	}
	if c.HostID != "" && c.HostID != mid {
		return fmt.Errorf("身份属于另一台机器，已拒绝覆盖")
	}
	c.HostID = mid
	if c.ClientID == "" {
		c.ClientID = strings.ToUpper(uuid.NewString())
	}
	if name != "" {
		c.Name = name
	}
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = "UU-" + h + "-" + role
	}
	if len(c.Name) > 100 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return fmt.Errorf("设备名称无效")
	}
	runtimeConfig = c
	linuxBearer = ""
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if c.DeviceID == "" {
		// The provider requires mac even before an account is logged in. Keep
		// registration consistent with the CLI; existing identities skip it.
		mac, err := guiRegistrationMAC()
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"system_id": c.ClientID, "client_id": c.ClientID, "system_name": "macOS", "system_version": "27.2.0", "model_number": "UULINK-LINUX", "model_identifier": "Linux,1", "model": "Linux", "name": c.Name, "mac": mac, "cpu": runtime.GOARCH, "resolution": "1280x720", "dpi": 96, "video": []string{"Linux desktop"}, "controllable": true, "memory": "4096", "os": runtime.GOOS})
		var result struct {
			DeviceID string `json:"device_id"`
		}
		if e = api(ctx, "/api/v1/device/macos/init", body, &result); e != nil {
			return fmt.Errorf("注册本机设备失败：%w", e)
		}
		if result.DeviceID == "" {
			return fmt.Errorf("UU未返回设备身份")
		}
		c.DeviceID = result.DeviceID
		runtimeConfig = c
	}
	body, _ := json.Marshal(map[string]string{"mobile": phone, "type": "login"})
	if e = api(ctx, "/api/v1/security/mobile/code", body, nil); e != nil {
		return e
	}
	g.mu.Lock()
	g.pending[role] = guiPending{c, phone, time.Now().Add(5 * time.Minute), original}
	g.mu.Unlock()
	return nil
}
func (g *guiServer) verifyCode(ctx context.Context, role, code string) error {
	if !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(code) {
		return fmt.Errorf("请输入短信验证码")
	}
	p, e := g.rolePath(role)
	if e != nil {
		return e
	}
	g.apiMu.Lock()
	defer g.apiMu.Unlock()
	g.mu.Lock()
	v, ok := g.pending[role]
	g.mu.Unlock()
	if !ok || time.Now().After(v.Until) {
		return fmt.Errorf("验证码登录已过期，请重新发送")
	}
	runtimeConfig = v.Config
	linuxBearer = ""
	body, _ := json.Marshal(map[string]string{"mobile": v.Phone, "code": code})
	var result struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if e = api(ctx, "/api/v1/login/by_mobile", body, &result); e != nil {
		return e
	}
	if result.UserID == "" || result.Token == "" {
		return fmt.Errorf("登录未返回有效身份")
	}
	v.Config.UserID = result.UserID
	v.Config.Bearer = result.Token
	other := "controlled"
	if role == other {
		other = "controller"
	}
	op, _ := g.rolePath(other)
	oc, _ := readConfig(op)
	if oc.DeviceID == v.Config.DeviceID || oc.ClientID == v.Config.ClientID {
		return fmt.Errorf("控制与被控身份必须独立，已拒绝保存")
	}
	lock, e := controllerLock(p+".write.lock", false)
	if e != nil {
		return e
	}
	defer lock.Close()
	current, e := guiFingerprint(p)
	if e != nil {
		return e
	}
	if current != v.Original {
		return fmt.Errorf("身份配置已被其他操作更新，请重新登录；未覆盖当前身份")
	}
	if e = writeConfigUnlocked(p, v.Config); e != nil {
		return e
	}
	g.mu.Lock()
	delete(g.pending, role)
	g.mu.Unlock()
	return nil
}
