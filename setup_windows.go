//go:build uulink_setup

package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const windowsSetupMode = true

//go:embed assets/windows-install-core.exe
var windowsInstallCore []byte

//go:embed assets/webview2-setup.exe
var windowsRuntimeSetup []byte

//go:embed native-ui/windows/setup-assets
var windowsSetupAssets embed.FS

type windowsSetupState struct {
	mu         sync.Mutex
	Phase      string `json:"phase"`
	Message    string `json:"message"`
	Path       string `json:"path"`
	Error      string `json:"error"`
	Updating   bool   `json:"updating"`
	statusFile string
}

func setupDefaultPath() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\UULink`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err == nil {
		defer key.Close()
		if path, _, e := key.GetStringValue("InstallLocation"); e == nil && validWindowsInstallPath(path) {
			return path
		}
	}
	base, _ := os.UserCacheDir()
	return filepath.Join(base, "Programs", "UU Link")
}

func validWindowsInstallPath(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(clean) && filepath.Dir(clean) != clean && !strings.ContainsAny(path, "\r\n\x00\"<>|?*") && !strings.HasPrefix(path, `\\`)
}

func runWindowsEntry() error {
	path := setupDefaultPath()
	silent := slices.Contains(os.Args[1:], "/S")
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "/D=") {
			path = strings.TrimPrefix(arg, "/D=")
		} else if arg != "/S" {
			return fmt.Errorf("未知安装参数 %q", arg)
		}
	}
	if !validWindowsInstallPath(path) {
		return errors.New("请选择有效的 Windows 安装目录")
	}
	state := &windowsSetupState{Phase: "ready", Path: path}
	_, err := os.Stat(filepath.Join(path, "UU Link.exe"))
	state.Updating = err == nil
	if silent {
		return state.install(path)
	}
	if _, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString(""); err != nil {
		dir, e := os.MkdirTemp("", "uulink-runtime-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(dir)
		bootstrapper := filepath.Join(dir, "WebView2Setup.exe")
		if e = os.WriteFile(bootstrapper, windowsRuntimeSetup, 0600); e != nil {
			return e
		}
		cmd := exec.Command(bootstrapper, "/silent", "/install")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
		if _, e = webviewloader.GetAvailableCoreWebView2BrowserVersionString(""); e != nil {
			return errors.New("安装界面需要 WebView2，请检查网络后重新运行安装程序")
		}
	}
	return runWindowsHost(windowsHostOptions{
		className: "UULinkSetup", title: "UU Link 安装", width: 800, height: 560, dataSubdir: "Installer",
		serve: state.serve, canClose: func() bool { state.mu.Lock(); defer state.mu.Unlock(); return state.Phase != "installing" },
	})
}

func (s *windowsSetupState) install(path string) error {
	if !validWindowsInstallPath(path) {
		return errors.New("请选择有效的 Windows 安装目录")
	}
	if h, _, _ := winUser.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(winText("UULinkDesktop"))), 0); h != 0 {
		return errors.New("请先从 UU Link 的系统托盘菜单退出软件，再继续安装")
	}
	dir, err := os.MkdirTemp("", "uulink-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	core := filepath.Join(dir, "install-core.exe")
	if err = os.WriteFile(core, windowsInstallCore, 0600); err != nil {
		return err
	}
	status := filepath.Join(dir, "progress.txt")
	s.mu.Lock()
	s.statusFile = status
	s.mu.Unlock()
	cmd := exec.Command(core)
	// NSIS requires /D to be last and unquoted, including paths with spaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: windows.ComposeCommandLine([]string{core, "/S", "/STATUSFILE=" + status}) + " /D=" + path}
	if err = cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 2:
				return errors.New("UU Link 正在运行，请先从系统托盘退出软件")
			case 3:
				return errors.New("UU Link 需要 Windows 10 或 Windows 11 x64")
			case 4:
				return errors.New("WebView2 运行库安装未完成，请检查网络后重试")
			}
		}
		return errors.New("安装未完成，请检查安装目录是否可写、磁盘空间是否充足，然后重试")
	}
	for _, name := range []string{"UU Link.exe", "uulink-engine.exe", "Uninstall.exe", "README.txt", "THIRD_PARTY_NOTICES.txt"} {
		if info, e := os.Stat(filepath.Join(path, name)); e != nil || info.Size() == 0 {
			return fmt.Errorf("安装文件 %s 未完整写入，请重试", name)
		}
	}
	return nil
}

func (s *windowsSetupState) serve(ctx context.Context, ready chan<- string) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	guard := &guiServer{key: hex.EncodeToString(key), origin: "http://" + listener.Addr().String()}
	mux := http.NewServeMux()
	assets, err := fs.Sub(windowsSetupAssets, "native-ui/windows/setup-assets")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := guiAssets.ReadFile("gui_assets/icon.svg")
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(raw)
	})
	mux.HandleFunc("/api/session", guard.handle)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		message := s.Message
		if s.Phase == "installing" {
			raw, _ := os.ReadFile(s.statusFile)
			if value := map[string]string{"runtime": "正在检查运行环境", "files": "正在安装应用文件", "shortcuts": "正在创建开始菜单入口", "done": "正在完成安装"}[string(raw)]; value != "" {
				message = value
			}
		}
		guiJSON(w, 200, map[string]any{"phase": s.Phase, "message": message, "path": s.Path, "error": s.Error, "updating": s.Updating})
	})
	mux.HandleFunc("/api/install", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		if err := guiBody(w, r, &body); err != nil || !validWindowsInstallPath(body.Path) {
			guiJSON(w, 400, map[string]string{"error": "请选择有效的 Windows 安装目录"})
			return
		}
		s.mu.Lock()
		if s.Phase == "installing" || s.Phase == "done" {
			s.mu.Unlock()
			guiJSON(w, 409, map[string]string{"error": "安装任务已开始"})
			return
		}
		s.Phase, s.Path, s.Error, s.Message = "installing", filepath.Clean(body.Path), "", "正在准备安装文件"
		path := s.Path
		s.mu.Unlock()
		go func() {
			err := s.install(path)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				s.Phase, s.Error = "error", err.Error()
			} else {
				s.Phase, s.Message = "done", "安装完成"
			}
		}()
		guiJSON(w, 202, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/launch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.Phase != "done" {
			guiJSON(w, 409, map[string]string{"error": "请等待安装完成"})
			return
		}
		cmd := exec.Command(filepath.Join(s.Path, "UU Link.exe"))
		if err := cmd.Start(); err != nil {
			guiJSON(w, 500, map[string]string{"error": "无法启动 UU Link，请从开始菜单打开"})
			return
		}
		cmd.Process.Release()
		guiJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/platform/preferences", func(w http.ResponseWriter, r *http.Request) {
		data, err := platformPreferences(w, r)
		if err != nil {
			guiJSON(w, 400, map[string]string{"error": err.Error()})
		} else {
			guiJSON(w, 200, data)
		}
	})
	server := &http.Server{Handler: guard.guard(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	ready <- guard.origin + "/#key=" + guard.key
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
