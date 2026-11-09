package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

const version = "0.3.6-tunopt"
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
	case "app-run":
		if len(args) < 3 || args[2] != "controller-daemon" {
			return errUnsupportedPlatform
		}
		pid, err := strconv.Atoi(args[1])
		if err != nil || pid < 2 || pid != os.Getppid() {
			return errors.New("所属应用已经退出")
		}
		// The parent opens this gate only after assigning this process to its job.
		var marker [1]byte
		if _, err = io.ReadFull(os.Stdin, marker[:]); err != nil || marker[0] != 0x55 {
			return errors.New("引擎所属应用未就绪")
		}
		var assigned int32
		if ok, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob").Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&assigned))); ok == 0 || assigned == 0 {
			return errors.New("引擎没有加入应用的进程组")
		}
		return controllerDaemonCLI(args[3:])
	case "controller-worker":
		return controllerWorkerCLI(args[1:])
	}
	return errUnsupportedPlatform
}
