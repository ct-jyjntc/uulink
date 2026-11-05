package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/go-webview2/pkg/webview2"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

var (
	winUser       = windows.NewLazySystemDLL("user32.dll")
	winKernel     = windows.NewLazySystemDLL("kernel32.dll")
	defWindowProc = winUser.NewProc("DefWindowProcW")
	postQuit      = winUser.NewProc("PostQuitMessage")
	destroyWindow = winUser.NewProc("DestroyWindow")
	postMessage   = winUser.NewProc("PostMessageW")
)

func winText(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }
func windowsError(s string) {
	winUser.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(winText(s))), uintptr(unsafe.Pointer(winText("UU Link"))), 0x10)
}

type winClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	SmallIcon                          uintptr
}

type winPoint struct{ X, Y int32 }
type winMinMax struct{ Reserved, MaxSize, MaxPosition, MinTrackSize, MaxTrackSize winPoint }
type winMsg struct {
	HWND           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          winPoint
	Private        uint32
}

const wmClose = 0x10
const wmQuit = 0x12

type winWebPolicy struct{ origin *url.URL }

func (*winWebPolicy) QueryInterface(_, _ uintptr) uintptr { return 0x80004002 }
func (*winWebPolicy) AddRef() uintptr                     { return 1 }
func (*winWebPolicy) Release() uintptr                    { return 1 }
func (p *winWebPolicy) NavigationStarting(_ *webview2.ICoreWebView2, args *webview2.ICoreWebView2NavigationStartingEventArgs) uintptr {
	raw, err := args.GetUri()
	u, parseErr := url.Parse(raw)
	if err != nil || parseErr != nil || u.Scheme != p.origin.Scheme || u.Host != p.origin.Host || u.Path != "/" || u.User != nil {
		_ = args.PutCancel(true)
	}
	return 0
}
func (*winWebPolicy) NewWindowRequested(_ *webview2.ICoreWebView2, args *webview2.ICoreWebView2NewWindowRequestedEventArgs) uintptr {
	_ = args.PutHandled(true)
	return 0
}

func runWindowsWindow() error {
	if h, _, _ := winUser.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(winText("UULinkDesktop"))), 0); h != 0 {
		winUser.NewProc("ShowWindow").Call(h, 9)
		winUser.NewProc("SetForegroundWindow").Call(h)
		return nil
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString(""); err != nil {
		return errors.New("此电脑需要 Microsoft Edge WebView2 Runtime。请运行 UU Link 安装程序，或从 Microsoft 安装 WebView2 Runtime 后重试。")
	}
	if err := windows.CoInitializeEx(0, 2); err != nil && !errors.Is(err, syscall.Errno(1)) {
		return fmt.Errorf("无法初始化 Windows 界面: %w", err)
	}
	defer windows.CoUninitialize()
	winUser.NewProc("SetProcessDpiAwarenessContext").Call(^uintptr(3))

	data, err := windowsDataDir()
	if err != nil {
		return err
	}
	prefs := newWinPrefs(data)
	defer prefs.close()
	winNativePrefs = prefs
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() { done <- runGUI(ctx, []string{"--listen", "127.0.0.1:17633", "--no-open"}, ready); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	}()
	var link string
	select {
	case err = <-done:
		if err == nil {
			return errors.New("本机面板未启动")
		}
		return err
	case link = <-ready:
	case <-time.After(15 * time.Second):
		return errors.New("本机面板启动超时")
	}
	endpoint, err := url.Parse(link)
	if err != nil {
		return err
	}

	var view *edge.Chromium
	var controller *webview2.ICoreWebView2Controller
	var finalErr error
	var hwnd uintptr
	dpiForWindow := winUser.NewProc("GetDpiForWindow")
	proc := syscall.NewCallback(func(h, msg, w, l uintptr) uintptr {
		switch msg {
		case 0x0024: // WM_GETMINMAXINFO
			dpi, _, _ := dpiForWindow.Call(h)
			if dpi == 0 {
				dpi = 96
			}
			info := (*winMinMax)(unsafe.Pointer(l))
			info.MinTrackSize = winPoint{int32(700 * dpi / 96), int32(520 * dpi / 96)}
			return 0
		case 0x0005: // WM_SIZE
			if view != nil && controller != nil && w != 1 {
				view.Resize()
			}
			return 0
		case 0x0003: // WM_MOVE
			if view != nil && controller != nil {
				_ = view.NotifyParentWindowPositionChanged()
			}
		case 0x02e0: // WM_DPICHANGED
			r := (*[4]int32)(unsafe.Pointer(l))
			winUser.NewProc("SetWindowPos").Call(h, 0, uintptr(r[0]), uintptr(r[1]), uintptr(r[2]-r[0]), uintptr(r[3]-r[1]), 0x14)
			return 0
		case wmClose:
			if controller != nil {
				view.ShuttingDown()
				_ = controller.Close()
				controller = nil
			}
			destroyWindow.Call(h)
			return 0
		case 2: // WM_DESTROY
			hwnd = 0
			postQuit.Call(0)
			return 0
		}
		ret, _, _ := defWindowProc.Call(h, msg, w, l)
		return ret
	})
	inst, _, _ := winKernel.NewProc("GetModuleHandleW").Call(0)
	className := winText("UULinkDesktop")
	cursor, _, _ := winUser.NewProc("LoadCursorW").Call(0, 32512)
	icon, _, _ := winUser.NewProc("LoadIconW").Call(inst, 1)
	if icon == 0 {
		icon, _, _ = winUser.NewProc("LoadIconW").Call(0, 32512)
	}
	cls := winClass{Size: uint32(unsafe.Sizeof(winClass{})), Proc: proc, Instance: inst, Icon: icon, SmallIcon: icon, Cursor: cursor, Background: 6, ClassName: className}
	if a, _, e := winUser.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&cls))); a == 0 {
		return fmt.Errorf("无法注册窗口: %w", e)
	}
	const useDefault = 0x80000000
	hwnd, _, err = winUser.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(winText("UU Link"))), 0x00cf0000|0x02000000|0x04000000, useDefault, useDefault, 1000, 700, 0, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("无法创建窗口: %w", err)
	}
	defer func() {
		if hwnd != 0 {
			if controller != nil {
				view.ShuttingDown()
				_ = controller.Close()
			}
			destroyWindow.Call(hwnd)
		}
	}()

	view = edge.NewChromium()
	view.DataPath = filepath.Join(data, "WebView2")
	view.AdditionalBrowserArgs = []string{"--no-proxy-server"}
	view.SetGlobalPermission(edge.CoreWebView2PermissionStateDeny)
	view.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateDefault)
	view.SetErrorCallback(func(err error) { cancel(); windowsError(fmt.Sprintf("WebView2 无法启动: %v", err)) })
	if !view.Embed(hwnd) || hwnd == 0 || view.GetController() == nil {
		return errors.New("WebView2 窗口初始化失败")
	}
	c := view.GetController()
	controller = (*webview2.ICoreWebView2Controller)(unsafe.Pointer(c))
	core, err := controller.GetCoreWebView2()
	if err != nil {
		return err
	}
	defer core.Vtbl.Release.Call(uintptr(unsafe.Pointer(core)))
	policy := &winWebPolicy{origin: endpoint}
	nav := webview2.NewICoreWebView2NavigationStartingEventHandler(policy)
	popup := webview2.NewICoreWebView2NewWindowRequestedEventHandler(policy)
	navToken, err := core.AddNavigationStarting(nav)
	if err != nil {
		return err
	}
	popupToken, err := core.AddNewWindowRequested(popup)
	if err != nil {
		return err
	}
	defer func() {
		if controller != nil {
			_ = core.RemoveNavigationStarting(navToken)
			_ = core.RemoveNewWindowRequested(popupToken)
		}
		runtime.KeepAlive(nav)
		runtime.KeepAlive(popup)
	}()
	if settings, err := view.GetSettings(); err == nil {
		_ = settings.PutAreDevToolsEnabled(false)
		_ = settings.PutAreDefaultContextMenusEnabled(false)
		_ = settings.PutIsStatusBarEnabled(false)
		_ = settings.PutIsWebMessageEnabled(false)
	}
	_ = view.AllowExternalDrag(false)
	_ = view.PutIsGeneralAutofillEnabled(false)
	_ = view.PutIsPasswordAutosaveEnabled(false)
	pageLoaded := false
	view.NavigationCompletedCallback = func(_ *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		ok, err := (*webview2.ICoreWebView2NavigationCompletedEventArgs)(unsafe.Pointer(args)).GetIsSuccess()
		if ok && err == nil {
			pageLoaded = true
			return
		}
		if !pageLoaded {
			finalErr = errors.New("无法打开本机管理界面，请重新启动 UU Link")
			postMessage.Call(hwnd, wmClose, 0, 0)
		}
	}
	view.ProcessFailedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2ProcessFailedEventArgs) {
		finalErr = errors.New("界面进程退出，请重新启动 UU Link")
		postMessage.Call(hwnd, wmClose, 0, 0)
	}
	view.Init(`window.uuDesktopWindows=true;`)
	view.Navigate(link)
	view.Resize()
	winUser.NewProc("ShowWindow").Call(hwnd, 5)
	_ = view.Show()
	var msg winMsg
	get := winUser.NewProc("GetMessageW")
	translate := winUser.NewProc("TranslateMessage")
	dispatch := winUser.NewProc("DispatchMessageW")
	for {
		result, _, e := get.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(result) < 0 {
			return fmt.Errorf("窗口消息循环失败: %w", e)
		}
		if msg.Message == wmQuit || result == 0 {
			break
		}
		translate.Call(uintptr(unsafe.Pointer(&msg)))
		dispatch.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return finalErr
}
