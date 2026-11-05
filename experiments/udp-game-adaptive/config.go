package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type agentConfig struct {
	DeviceID   string `json:"device_id"`
	ClientID   string `json:"client_id"`
	UserID     string `json:"user_id"`
	Bearer     string `json:"bearer"`
	Name       string `json:"name"`
	HostID     string `json:"machine_id,omitempty"`
	ICEPolicy  string `json:"ice_policy,omitempty"`
	AllowPower bool   `json:"allow_power"`
	// Empty components means the full feature set used by releases before 0.2.0.
	Components string `json:"components,omitempty"`
	LowMemory  bool   `json:"low_memory,omitempty"`
}

var runtimeConfig agentConfig

func loadAgentConfig(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg agentConfig
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.DeviceID == "" || cfg.ClientID == "" || cfg.UserID == "" || cfg.Bearer == "" {
		return fmt.Errorf("device is not logged in; run the login command")
	}
	if _, err := parseComponents(cfg.Components); err != nil {
		return err
	}
	runtimeConfig = cfg
	linuxBearer = cfg.Bearer
	return nil
}

func readConfig(path string) (agentConfig, error) {
	var cfg agentConfig
	b, e := os.ReadFile(path)
	if e != nil {
		return cfg, e
	}
	e = json.Unmarshal(b, &cfg)
	return cfg, e
}
func writeConfig(path string, cfg agentConfig) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if e := os.Chmod(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".config-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
