package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"unsafe"
)

func TestWindowsTrayPreferences(t *testing.T) {
	dir := t.TempDir()
	p := newWinPrefs(dir)
	defer p.close()
	if !p.runInBackground() {
		t.Fatal("new installations should keep running in the tray")
	}
	if err := os.WriteFile(p.path, []byte(`{"awake":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := newWinPrefs(dir)
	defer legacy.close()
	if !legacy.runInBackground() {
		t.Fatal("legacy preferences must inherit the tray default")
	}
	if err := p.set("menuBar", false); err != nil {
		t.Fatal(err)
	}
	if err := p.set("awake", false); err != nil {
		t.Fatal(err)
	}
	restored := newWinPrefs(dir)
	defer restored.close()
	if restored.runInBackground() {
		t.Fatal("saving another preference lost the explicit tray setting")
	}
	if err := restored.set("menuBar", true); err != nil {
		t.Fatal(err)
	}
	if restored.runInBackground() != true {
		t.Fatal("tray preference was not updated")
	}
}

func TestWindowsTrayNativeLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(winNotifyIconData{}) != 976 {
		t.Fatalf("NOTIFYICONDATAW must match the Windows x64 ABI, got %d", unsafe.Sizeof(winNotifyIconData{}))
	}
}

func TestWindowsApplicationIcon(t *testing.T) {
	data, err := os.ReadFile("native-ui/windows/AppIcon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 6 || binary.LittleEndian.Uint16(data[2:4]) != 1 {
		t.Fatal("invalid Windows icon header")
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count < 7 || len(data) < 6+count*16 {
		t.Fatal("missing Windows icon sizes")
	}
	for i := 0; i < count; i++ {
		entry := data[6+i*16 : 6+(i+1)*16]
		size, offset := int(binary.LittleEndian.Uint32(entry[8:12])), int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset > len(data) || size > len(data)-offset {
			t.Fatal("invalid Windows icon image bounds")
		}
		img, err := png.Decode(bytes.NewReader(data[offset : offset+size]))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, a := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/8).RGBA()
		if b <= r || r <= g || a != 0xffff {
			t.Errorf("icon size %d lost the purple app background", img.Bounds().Dx())
		}
	}
}

func TestWindowsConfigurationReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "rules.json")
	for _, name := range []string{"first", "replacement"} {
		doc := ruleDocument{Version: 1, Rules: []forwardRule{{ID: "test", Name: name, Device: "test", Listen: "127.0.0.1:12345", Target: "127.0.0.1:80", Protocol: "tcp"}}}
		if err := atomicControllerJSON(path, doc); err != nil {
			t.Fatal(err)
		}
		got, err := readRules(path)
		if err != nil || len(got.Rules) != 1 || got.Rules[0].Name != name {
			t.Fatalf("configuration replacement failed: %+v, %v", got, err)
		}
	}
}

func TestWindowsEmbeddedPageResources(t *testing.T) {
	page, err := guiAssets.ReadFile("gui_assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range regexp.MustCompile(`(?:src|href)="(/[^"#]+)"`).FindAllSubmatch(page, -1) {
		if _, err := fs.Stat(guiAssets, "gui_assets"+string(match[1])); err != nil {
			t.Errorf("missing embedded page resource %s: %v", match[1], err)
		}
	}
}
