package main

// Windows desktop builds expose the controller only.
func terminalDetachPeer(*protocolPeer)      {}
func (*protocolPeer) terminalHandle([]byte) {}
func signalPowerAction([]byte) string       { return "" }
func requestPower(string) error             { return errUnsupportedPlatform }

func isAlpine() bool { return false }
