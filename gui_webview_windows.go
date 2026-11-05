package main

import (
	"net/url"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// Only use the edge bindings. Importing the generated webview2 package registers
// callbacks with Go string arguments and panics before main on Windows x64.
// These slots are from the base WebView2 COM interfaces in WebView2.h.
func winCOMCall(object unsafe.Pointer, slot int, args ...uintptr) error {
	vtable := (*[64]uintptr)(*(*unsafe.Pointer)(object))
	hr, _, _ := syscall.SyscallN(vtable[slot], append([]uintptr{uintptr(object)}, args...)...)
	if int32(hr) < 0 {
		return syscall.Errno(hr)
	}
	return nil
}

func closeWinWebView(controller *edge.ICoreWebView2Controller) {
	_ = winCOMCall(unsafe.Pointer(controller), 24)
}

func enableWindowsDragRegions(settings unsafe.Pointer) bool {
	// ICoreWebView2Settings9 extends Settings8. Its setter follows all 38
	// inherited/getter slots in the native WebView2.h vtable.
	iid := windows.GUID{Data1: 0x0528a73b, Data2: 0xe92d, Data3: 0x49f4, Data4: [8]byte{0x92, 0x7a, 0xe5, 0x47, 0xdd, 0xda, 0xa3, 0x7d}}
	var settings9 unsafe.Pointer
	if err := winCOMCall(settings, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&settings9))); err != nil || settings9 == nil {
		return false
	}
	defer winCOMCall(settings9, 2)
	return winCOMCall(settings9, 38, 1) == nil
}

type winWebHandler struct {
	vtable *[4]uintptr
	iid    windows.GUID
	invoke func(unsafe.Pointer) uintptr
}

var winWebHandlerVtable = [4]uintptr{
	syscall.NewCallback(func(h *winWebHandler, iid *windows.GUID, out *unsafe.Pointer) uintptr {
		*out = nil
		unknown := windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
		if *iid != unknown && *iid != h.iid {
			return 0x80004002
		}
		*out = unsafe.Pointer(h)
		return 0
	}),
	syscall.NewCallback(func(*winWebHandler) uintptr { return 1 }),
	syscall.NewCallback(func(*winWebHandler) uintptr { return 1 }),
	syscall.NewCallback(func(h *winWebHandler, _ uintptr, args unsafe.Pointer) uintptr {
		return h.invoke(args)
	}),
}

type winWebPolicy struct {
	navigation, popup           *winWebHandler
	navigationToken, popupToken int64
}

func newWinWebPolicy(core *edge.ICoreWebView2, origin *url.URL) (*winWebPolicy, error) {
	p := &winWebPolicy{
		navigation: &winWebHandler{
			vtable: &winWebHandlerVtable,
			iid:    windows.GUID{Data1: 0x9adbe429, Data2: 0xf36d, Data3: 0x432b, Data4: [8]byte{0x9d, 0xdc, 0xf8, 0x88, 0x1f, 0xbd, 0x76, 0xe3}},
			invoke: func(args unsafe.Pointer) uintptr {
				var raw *uint16
				err := winCOMCall(args, 3, uintptr(unsafe.Pointer(&raw)))
				u, parseErr := url.Parse(windows.UTF16PtrToString(raw))
				windows.CoTaskMemFree(unsafe.Pointer(raw))
				if err != nil || parseErr != nil || u.Scheme != origin.Scheme || u.Host != origin.Host || u.Path != "/" || u.User != nil {
					_ = winCOMCall(args, 8, 1)
				}
				return 0
			},
		},
		popup: &winWebHandler{
			vtable: &winWebHandlerVtable,
			iid:    windows.GUID{Data1: 0xd4c185fe, Data2: 0xc81c, Data3: 0x4989, Data4: [8]byte{0x97, 0xaf, 0x2d, 0x3f, 0xa7, 0xab, 0x56, 0x51}},
			invoke: func(args unsafe.Pointer) uintptr {
				_ = winCOMCall(args, 6, 1)
				return 0
			},
		},
	}
	err := winCOMCall(unsafe.Pointer(core), 7, uintptr(unsafe.Pointer(p.navigation)), uintptr(unsafe.Pointer(&p.navigationToken)))
	if err != nil {
		return nil, err
	}
	err = winCOMCall(unsafe.Pointer(core), 44, uintptr(unsafe.Pointer(p.popup)), uintptr(unsafe.Pointer(&p.popupToken)))
	if err != nil {
		_ = winCOMCall(unsafe.Pointer(core), 8, uintptr(p.navigationToken))
		return nil, err
	}
	runtime.KeepAlive(p)
	return p, nil
}

func (p *winWebPolicy) close(core *edge.ICoreWebView2) {
	_ = winCOMCall(unsafe.Pointer(core), 8, uintptr(p.navigationToken))
	_ = winCOMCall(unsafe.Pointer(core), 45, uintptr(p.popupToken))
}
