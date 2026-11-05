//go:build uulink_setup

package main

import "testing"

func TestWindowsInstallerPaths(t *testing.T) {
	for _, path := range []string{`C:\Users\Test User\AppData\Local\Programs\UU Link`, `D:\Apps\UU Link`} {
		if !validWindowsInstallPath(path) {
			t.Errorf("valid path rejected %q", path)
		}
	}
	for _, path := range []string{"", ".", `C:\`, `relative\folder`, `\\server\share`, `C:\bad"folder`, "C:\\bad\nfolder"} {
		if validWindowsInstallPath(path) {
			t.Errorf("invalid path accepted %q", path)
		}
	}
}
