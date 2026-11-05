//go:build !uulink_setup

package main

const windowsSetupMode = false

func runWindowsEntry() error { return runWindowsWindow() }
