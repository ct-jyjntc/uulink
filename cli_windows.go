package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

const version = "0.3.5"
const configDefault = ""
const clientStateDir = ""

var errUnsupportedPlatform = errors.New("Windows 版仅提供端口转发控制端")

func main() {
	if err := runWindowsCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "UU Link:", err)
		windowsError(err.Error())
		os.Exit(1)
	}
}
func runWindowsCLI(args []string) error {
	if len(args) == 0 {
		return runWindowsWindow()
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
