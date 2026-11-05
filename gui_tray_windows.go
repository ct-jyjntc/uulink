package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const wmTray = 0x8004
const wmExit = 0x8003

type winNotifyIconData struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                windows.GUID
	BalloonIcon         uintptr
}

type windowsTray struct {
	data   winNotifyIconData
	active bool
}

var shellNotifyIcon = windows.NewLazySystemDLL("shell32.dll").NewProc("Shell_NotifyIconW")

func newWindowsTray(hwnd, icon uintptr) (*windowsTray, error) {
	t := &windowsTray{data: winNotifyIconData{Window: hwnd, ID: 1, Flags: 0x87, Callback: wmTray, Icon: icon}}
	t.data.Size = uint32(unsafe.Sizeof(t.data))
	copy(t.data.Tip[:], windows.StringToUTF16("UU Link · 右键打开菜单"))
	if !t.add() {
		return nil, fmt.Errorf("无法创建 UU Link 托盘图标")
	}
	return t, nil
}

func (t *windowsTray) add() bool {
	ok, _, _ := shellNotifyIcon.Call(0, uintptr(unsafe.Pointer(&t.data))) // NIM_ADD
	t.active = ok != 0
	if t.active {
		t.data.Version = 4
		shellNotifyIcon.Call(4, uintptr(unsafe.Pointer(&t.data))) // NIM_SETVERSION
	}
	return t.active
}

func (t *windowsTray) close() {
	shellNotifyIcon.Call(2, uintptr(unsafe.Pointer(&t.data))) // NIM_DELETE
	t.active = false
}

func restoreWindowsWindow(hwnd uintptr) {
	show := uintptr(5) // Preserve maximized state when restoring a hidden window.
	if iconic, _, _ := winUser.NewProc("IsIconic").Call(hwnd); iconic != 0 {
		show = 9
	}
	winUser.NewProc("ShowWindow").Call(hwnd, show)
	winUser.NewProc("SetForegroundWindow").Call(hwnd)
}

func (t *windowsTray) menu() uintptr {
	menu, _, _ := winUser.NewProc("CreatePopupMenu").Call()
	if menu == 0 {
		return 0
	}
	defer winUser.NewProc("DestroyMenu").Call(menu)
	appendItem := winUser.NewProc("AppendMenuW")
	appendItem.Call(menu, 0, 1, uintptr(unsafe.Pointer(winText("打开 UU Link"))))
	appendItem.Call(menu, 0, 2, uintptr(unsafe.Pointer(winText("连接"))))
	appendItem.Call(menu, 0, 3, uintptr(unsafe.Pointer(winText("断开连接…"))))
	appendItem.Call(menu, 0x800, 0, 0)
	appendItem.Call(menu, 0, 4, uintptr(unsafe.Pointer(winText("退出 UU Link（断开连接）"))))
	winUser.NewProc("SetMenuDefaultItem").Call(menu, 1, 0)
	var point winPoint
	winUser.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&point)))
	winUser.NewProc("SetForegroundWindow").Call(t.data.Window)
	command, _, _ := winUser.NewProc("TrackPopupMenu").Call(menu, 0x182, uintptr(point.X), uintptr(point.Y), 0, t.data.Window, 0)
	postMessage.Call(t.data.Window, 0, 0, 0)
	return command
}
