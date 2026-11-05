package main

import "golang.org/x/sys/windows/registry"

func machineID() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, _ := key.GetStringValue("MachineGuid")
	return value
}
