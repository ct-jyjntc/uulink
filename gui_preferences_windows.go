package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var winNativePrefs *windowsPreferences

const loginRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const loginRunName = "UULink"

type windowsPreferences struct {
	mu    sync.Mutex
	path  string
	awake windows.Handle
}
type winPrefsView struct {
	Login bool `json:"login"`
	Awake bool `json:"awake"`
}
type powerReason struct {
	Version, Flags uint32
	Reason         *uint16
	UnionPad       [2]uintptr
}

func newWinPrefs(data string) *windowsPreferences {
	p := &windowsPreferences{path: filepath.Join(data, "windows-preferences.json")}
	if raw, err := os.ReadFile(p.path); err == nil {
		var saved winPrefsView
		if json.Unmarshal(raw, &saved) == nil && saved.Awake {
			_ = p.openAwake()
		}
	}
	return p
}

func (p *windowsPreferences) openAwake() error {
	if p.awake != 0 {
		return nil
	}
	reason := powerReason{Flags: 1, Reason: winText("UU Link port forwarding")}
	h, _, e := winKernel.NewProc("PowerCreateRequest").Call(uintptr(unsafe.Pointer(&reason)))
	if h == 0 || h == ^uintptr(0) {
		return fmt.Errorf("无法申请防止休眠: %w", e)
	}
	if a, _, err := winKernel.NewProc("PowerSetRequest").Call(h, 1); a == 0 {
		windows.CloseHandle(windows.Handle(h))
		return fmt.Errorf("无法阻止系统休眠: %w", err)
	}
	p.awake = windows.Handle(h)
	return nil
}
func (p *windowsPreferences) closeAwake() {
	if p.awake != 0 {
		winKernel.NewProc("PowerClearRequest").Call(uintptr(p.awake), 1)
		_ = windows.CloseHandle(p.awake)
		p.awake = 0
	}
}
func (p *windowsPreferences) close() { p.mu.Lock(); defer p.mu.Unlock(); p.closeAwake() }
func windowsLoginCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}
func (p *windowsPreferences) view() winPrefsView {
	state := winPrefsView{Awake: p.awake != 0}
	key, err := registry.OpenKey(registry.CURRENT_USER, loginRunKey, registry.QUERY_VALUE)
	if err != nil {
		return state
	}
	defer key.Close()
	saved, _, err := key.GetStringValue(loginRunName)
	current, _ := windowsLoginCommand()
	state.Login = err == nil && saved == current
	return state
}
func (p *windowsPreferences) set(name string, enabled bool) error {
	switch name {
	case "login":
		key, _, err := registry.CreateKey(registry.CURRENT_USER, loginRunKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer key.Close()
		if !enabled {
			err = key.DeleteValue(loginRunName)
			if errors.Is(err, registry.ErrNotExist) {
				return nil
			}
			return err
		}
		cmd, err := windowsLoginCommand()
		if err != nil {
			return err
		}
		return key.SetStringValue(loginRunName, cmd)
	case "awake":
		was := p.awake != 0
		if was == enabled {
			return nil
		}
		if enabled {
			if err := p.openAwake(); err != nil {
				return err
			}
		}
		if err := atomicControllerJSON(p.path, winPrefsView{Awake: enabled}); err != nil {
			if enabled {
				p.closeAwake()
			}
			return err
		}
		if !enabled {
			p.closeAwake()
		}
		return nil
	}
	return errors.New("未知设置")
}

func platformPreferences(w http.ResponseWriter, r *http.Request) (any, error) {
	p := winNativePrefs
	if p == nil {
		return nil, errors.New("请在 Windows 桌面版中调整此设置")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		return p.view(), nil
	case http.MethodPost:
		var change struct {
			Key   string `json:"key"`
			Value *bool  `json:"value"`
		}
		if err := guiBody(w, r, &change); err != nil {
			return nil, err
		}
		if change.Value == nil {
			return nil, errors.New("缺少设置值")
		}
		if err := p.set(change.Key, *change.Value); err != nil {
			return nil, err
		}
		return p.view(), nil
	}
	return nil, errors.New("不支持的操作")
}
