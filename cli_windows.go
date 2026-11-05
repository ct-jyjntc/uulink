package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
)

const version = "0.3.5"
const configDefault = ""
const clientStateDir = ""

var errUnsupportedPlatform = errors.New("Windows 版仅提供端口转发控制端")

func main() {
	if err := runWindowsCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "UU Link:", err)
		if !windowsSetupMode || !slices.Contains(os.Args[1:], "/S") {
			windowsError(err.Error())
		}
		os.Exit(1)
	}
}
func runWindowsCLI(args []string) error {
	if windowsSetupMode {
		return runWindowsEntry()
	}
	if len(args) == 0 {
		return runWindowsEntry()
	}
	switch args[0] {
	case "network-check":
		return physicalNetworkCLI(args[1:])
	case "version", "--version":
		fmt.Printf("UU Link %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "gui":
		return guiCLI(args[1:])
	}
	return errUnsupportedPlatform
}
