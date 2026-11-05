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
const wmAppearance = 0x8001
const wmWindowCommand = 0x8002

func windowsFrameColor(dark bool) uint32 {
	if dark {
		return 0x001e1a1a
	}
	return 0x00f6f4f4
}

func windowsFrameInset(hwnd uintptr) int32 {
	if zoomed, _, _ := winUser.NewProc("IsZoomed").Call(hwnd); zoomed != 0 {
		return 0
	}
	dpi, _, _ := winUser.NewProc("GetDpiForWindow").Call(hwnd)
	if dpi == 0 {
		dpi = 96
	}
	return int32(5 * dpi / 96)
}

func resizeWindowsView(view *edge.Chromium, hwnd uintptr) {
	inset := windowsFrameInset(hwnd)
	view.SetPadding(edge.Rect{Left: inset, Top: inset, Right: inset, Bottom: inset})
	view.Resize()
	// Clip the WebView host as well as the outer DWM frame. Top-layer dialog
	// backdrops otherwise paint square corners inside the rounded window.
	child, _, _ := winUser.NewProc("FindWindowExW").Call(hwnd, 0, uintptr(unsafe.Pointer(winText("Chrome_WidgetWin_0"))), 0)
	if child == 0 {
		return
	}
	var region uintptr
	if inset != 0 {
		var rect [4]int32
		winUser.NewProc("GetClientRect").Call(child, uintptr(unsafe.Pointer(&rect)))
		region, _, _ = windows.NewLazySystemDLL("gdi32.dll").NewProc("CreateRoundRectRgn").Call(0, 0, uintptr(rect[2]+1), uintptr(rect[3]+1), uintptr(inset*3), uintptr(inset*3))
	}
	if ok, _, _ := winUser.NewProc("SetWindowRgn").Call(child, region, 1); ok == 0 && region != 0 {
		windows.NewLazySystemDLL("gdi32.dll").NewProc("DeleteObject").Call(region)
	}
}

func setWindowsAppearance(hwnd uintptr, dark bool) {
	var mode uint32
	caption, text := uint32(0x00f6f4f4), uint32(0x00211c1c)
	if dark {
		mode = 1
		caption, text = 0x001e1a1a, 0x00f1eded
	}
	setAttribute := windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	// Older Windows versions ignore the unsupported caption color attributes.
	setAttribute.Call(hwnd, 20, uintptr(unsafe.Pointer(&mode)), 4)
	setAttribute.Call(hwnd, 35, uintptr(unsafe.Pointer(&caption)), 4)
	setAttribute.Call(hwnd, 36, uintptr(unsafe.Pointer(&text)), 4)
	winUser.NewProc("RedrawWindow").Call(hwnd, 0, 0, 0x0505) // Repaint the native resize border immediately.
}

type windowsHostOptions struct {
	className     string
	title         string
	width, height uintptr
	dataSubdir    string
	tray          bool
	serve         func(context.Context, chan<- string) error
	canClose      func() bool
}

func runWindowsWindow() error {
	return runWindowsHost(windowsHostOptions{
		className: "UULinkDesktop", title: "UU Link", width: 1000, height: 700, tray: true,
		serve: func(ctx context.Context, ready chan<- string) error {
			return runGUI(ctx, []string{"--listen", "127.0.0.1:17633", "--no-open"}, ready)
		},
	})
}

func runWindowsHost(options windowsHostOptions) error {
	if h, _, _ := winUser.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(winText(options.className))), 0); h != 0 {
		restoreWindowsWindow(h)
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
	if options.dataSubdir != "" {
		data = filepath.Join(data, options.dataSubdir)
	}
	prefs := newWinPrefs(data)
	defer prefs.close()
	winNativePrefs = prefs
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() { done <- options.serve(ctx, ready); close(done) }()
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
	var controller *edge.ICoreWebView2Controller
	var finalErr error
	var hwnd uintptr
	var tray *windowsTray
	taskbarCreated, _, _ := winUser.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(winText("TaskbarCreated"))))
	background := windowsFrameColor(windowsSystemDark())
	dpiForWindow := winUser.NewProc("GetDpiForWindow")
	proc := syscall.NewCallback(func(h, msg, w, l uintptr) uintptr {
		if taskbarCreated != 0 && msg == taskbarCreated && tray != nil {
			if !tray.add() {
				restoreWindowsWindow(h)
			}
			return 0
		}
		switch msg {
		case wmTray:
			switch l & 0xffff {
			case 0x400, 0x401, 0x0203: // NIN_SELECT, NIN_KEYSELECT, double click
				restoreWindowsWindow(h)
			case 0x007b, 0x0205: // Context menu, legacy right click
				if tray != nil {
					switch tray.menu() {
					case 1:
						restoreWindowsWindow(h)
					case 2:
						if view != nil {
							view.Eval(`window.uulinkMenuAction?.('connect')`)
						}
					case 3:
						restoreWindowsWindow(h)
						if view != nil {
							view.Eval(`window.uulinkMenuAction?.('disconnect')`)
						}
					case 4:
						postMessage.Call(h, wmExit, 0, 0)
					}
				}
			}
			return 0
		case 0x0011: // WM_QUERYENDSESSION
			return 1
		case 0x0016: // WM_ENDSESSION, exit during logoff/shutdown.
			if w != 0 {
				postMessage.Call(h, wmExit, 0, 0)
			}
			return 0
		case 0x0014: // WM_ERASEBKGND, paint the narrow native resize border.
			var rect [4]int32
			winUser.NewProc("GetClientRect").Call(h, uintptr(unsafe.Pointer(&rect)))
			gdi := windows.NewLazySystemDLL("gdi32.dll")
			brush, _, _ := gdi.NewProc("CreateSolidBrush").Call(uintptr(background))
			winUser.NewProc("FillRect").Call(w, uintptr(unsafe.Pointer(&rect)), brush)
			gdi.NewProc("DeleteObject").Call(brush)
			return 1
		case 0x0084: // WM_NCHITTEST, the WebView leaves this border to Windows.
			var rect [4]int32
			winUser.NewProc("GetWindowRect").Call(h, uintptr(unsafe.Pointer(&rect)))
			x, y := int32(int16(l&0xffff)), int32(int16((l>>16)&0xffff))
			inset := windowsFrameInset(h)
			left, right := x < rect[0]+inset, x >= rect[2]-inset
			top, bottom := y < rect[1]+inset, y >= rect[3]-inset
			switch {
			case top && left:
				return 13
			case top && right:
				return 14
			case bottom && left:
				return 16
			case bottom && right:
				return 17
			case left:
				return 10
			case right:
				return 11
			case top:
				return 12
			case bottom:
				return 15
			}
			return 1
		case 0x0083: // WM_NCCALCSIZE, extend the client over the native caption.
			if w != 0 {
				if zoomed, _, _ := winUser.NewProc("IsZoomed").Call(h); zoomed != 0 {
					dpi, _, _ := dpiForWindow.Call(h)
					metrics := winUser.NewProc("GetSystemMetricsForDpi")
					frame, _, _ := metrics.Call(33, dpi)   // SM_CYSIZEFRAME
					padding, _, _ := metrics.Call(92, dpi) // SM_CXPADDEDBORDER
					inset := int32(frame + padding)
					r := (*[4]int32)(unsafe.Pointer(l))
					r[0] += inset
					r[1] += inset
					r[2] -= inset
					r[3] -= inset
				}
				return 0
			}
		case wmWindowCommand:
			if w == 0xf100 {
				postMessage.Call(h, wmExit, 0, 0)
				return 0
			}
			if w == 0xf030 { // SC_MAXIMIZE toggles restore.
				if zoomed, _, _ := winUser.NewProc("IsZoomed").Call(h); zoomed != 0 {
					w = 0xf120
				}
			}
			winUser.NewProc("ReleaseCapture").Call()
			winUser.NewProc("SendMessageW").Call(h, 0x0112, w, 0) // WM_SYSCOMMAND
			return 0
		case wmAppearance:
			background = windowsFrameColor(w != 0)
			setWindowsAppearance(h, w != 0)
			return 0
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
				resizeWindowsView(view, h)
				view.Eval(fmt.Sprintf("window.uulinkWindowState?.(%t)", w == 2))
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
		case wmClose, wmExit:
			if options.canClose != nil && !options.canClose() && finalErr == nil {
				return 0
			}
			if msg == wmClose && finalErr == nil && tray != nil && prefs.runInBackground() && (tray.active || tray.add()) {
				winUser.NewProc("ShowWindow").Call(h, 0)
				return 0
			}
			if controller != nil {
				view.ShuttingDown()
				closeWinWebView(controller)
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
	className := winText(options.className)
	cursor, _, _ := winUser.NewProc("LoadCursorW").Call(0, 32512)
	icon, _, _ := winUser.NewProc("LoadImageW").Call(inst, 1, 1, 32, 32, 0x8000)
	if icon == 0 {
		icon, _, _ = winUser.NewProc("LoadIconW").Call(0, 32512)
	}
	smallIcon, _, _ := winUser.NewProc("LoadImageW").Call(inst, 1, 1, 16, 16, 0x8000)
	if smallIcon == 0 {
		smallIcon = icon
	}
	cls := winClass{Size: uint32(unsafe.Sizeof(winClass{})), Proc: proc, Instance: inst, Icon: icon, SmallIcon: smallIcon, Cursor: cursor, Background: 6, ClassName: className}
	if a, _, e := winUser.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&cls))); a == 0 {
		return fmt.Errorf("无法注册窗口: %w", e)
	}
	const useDefault = 0x80000000
	hwnd, _, err = winUser.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(winText(options.title))), 0x00cf0000|0x02000000|0x04000000, useDefault, useDefault, options.width, options.height, 0, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("无法创建窗口: %w", err)
	}
	prefs.mu.Lock()
	prefs.hwnd = hwnd
	prefs.mu.Unlock()
	setWindowsAppearance(hwnd, windowsSystemDark())
	corner := uint32(2) // DWMWCP_ROUND, retain the Windows 11 rounded frame.
	windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute").Call(hwnd, 33, uintptr(unsafe.Pointer(&corner)), 4)
	winUser.NewProc("SetWindowPos").Call(hwnd, 0, 0, 0, 0, 0, 0x37) // Recalculate the custom client frame.
	defer func() {
		prefs.mu.Lock()
		prefs.hwnd = 0
		prefs.mu.Unlock()
		if hwnd != 0 {
			if controller != nil {
				view.ShuttingDown()
				closeWinWebView(controller)
			}
			destroyWindow.Call(hwnd)
		}
	}()
	if options.tray {
		tray, err = newWindowsTray(hwnd, smallIcon)
		if err != nil {
			return err
		}
		defer tray.close()
	}

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
	controller = c
	core, err := controller.GetCoreWebView2()
	if err != nil {
		return err
	}
	defer core.Release()
	policy, err := newWinWebPolicy(core, endpoint)
	if err != nil {
		return err
	}
	defer func() {
		if controller != nil {
			policy.close(core)
		}
		runtime.KeepAlive(policy)
	}()
	nativeDrag := false
	if settings, err := view.GetSettings(); err == nil {
		nativeDrag = enableWindowsDragRegions(unsafe.Pointer(settings))
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
		var success int32
		err := winCOMCall(unsafe.Pointer(args), 3, uintptr(unsafe.Pointer(&success)))
		ok := success != 0
		if ok && err == nil {
			pageLoaded = true
			return
		}
		if !pageLoaded {
			finalErr = errors.New("无法打开本机管理界面，请重新启动 UU Link")
			postMessage.Call(hwnd, wmExit, 0, 0)
		}
	}
	view.ProcessFailedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2ProcessFailedEventArgs) {
		finalErr = errors.New("界面进程退出，请重新启动 UU Link")
		postMessage.Call(hwnd, wmExit, 0, 0)
	}
	view.Init(fmt.Sprintf(`window.uuDesktopWindows=true;window.uuNativeDrag=%t;`, nativeDrag))
	view.Navigate(link)
	resizeWindowsView(view, hwnd)
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
