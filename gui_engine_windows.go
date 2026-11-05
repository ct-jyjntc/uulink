package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const appEngineName = "uulink-engine.exe"

func startAppEngine(cmd *exec.Cmd) (func(bool), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	reader, gate, err := os.Pipe()
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer reader.Close()
	defer gate.Close()
	cmd.Stdin = reader
	configureControllerChild(cmd)
	if err = cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	abort := func(e error) (func(bool), error) {
		windows.CloseHandle(job)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, e
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return abort(err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return abort(fmt.Errorf("无法为转发引擎建立退出保护: %w", err))
	}
	if _, err = gate.Write([]byte{0x55}); err != nil {
		return abort(err)
	}
	var closeOnce sync.Once
	return func(bool) { closeOnce.Do(func() { _ = windows.CloseHandle(job) }) }, nil
}
