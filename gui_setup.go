package main

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Migrate the former single fixed-target connection into ordinary rules once.
// The marker is written after the rule document, so a crash is idempotent.
func (g *guiServer) migrateMappings() error {
	c, e := g.connectionConfig()
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if c.Version == 2 && e == nil {
		return nil
	}
	old := c.Version == 1 && e == nil
	if old {
		for _, path := range []string{g.connectionPath(), g.opts.rules} {
			if raw, err := os.ReadFile(path); err == nil {
				backup := path + ".before-port-mappings"
				if _, err = os.Stat(backup); os.IsNotExist(err) {
					if err = guiWriteBytes(backup, raw, 0600); err != nil {
						return err
					}
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}

	if e = updateRules(g.opts.rules, func(doc *ruleDocument) error {
		for i := range doc.Rules {
			if doc.Rules[i].Protocol == "" {
				doc.Rules[i].Protocol = "tcp+udp"
			}
			if doc.Rules[i].AllowLAN == nil {
				lan := doc.Rules[i].lanAllowed()
				doc.Rules[i].AllowLAN = &lan
			}
		}
		if old && c.Device != "" {
			id := "legacy-port-mapping"
			found := false
			_, legacyPort, _ := net.SplitHostPort(c.Listen)
			for _, r := range doc.Rules {
				_, port, _ := net.SplitHostPort(r.Listen)
				if r.Device == c.Device && r.Target == "127.0.0.1:28443" && port == legacyPort {
					found = true
					break
				}
			}
			for suffix := 0; ; suffix++ {
				collision := false
				for _, r := range doc.Rules {
					if r.ID == id {
						if r.Device == c.Device && r.Listen == c.Listen && r.Target == "127.0.0.1:28443" {
							found = true
						}
						collision = true
						break
					}
				}
				if found || !collision {
					break
				}
				id = fmt.Sprintf("legacy-port-mapping-%d", suffix+1)
			}
			if !found {
				h, port, _ := net.SplitHostPort(c.Listen)
				lan := !net.ParseIP(h).IsLoopback()
				doc.Rules = append(doc.Rules, forwardRule{ID: id, Name: "端口转发 " + port, Device: c.Device, Listen: c.Listen, Target: "127.0.0.1:28443", Enabled: c.Enabled, Protocol: "tcp+udp", AllowLAN: &lan})
			}
		}
		return nil
	}); e != nil {
		return e
	}
	c.Version = 2
	c.Mode = "ports"
	c.Device = ""
	c.Listen = ""
	c.AllowClient = ""
	c.Enabled = false
	return atomicControllerJSON(g.connectionPath(), c)
}
func (g *guiServer) startSavedMappings() {
	if e := g.startConnection(); e != nil {
		g.connection.mu.Lock()
		g.connection.lastError = e.Error()
		g.connection.mu.Unlock()
	}
}

func guiFingerprint(path string) (string, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return "absent", nil
	}
	if e != nil {
		return "", fmt.Errorf("身份文件无法读取")
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h), nil
}

// Canonicalize existing parents too, so two not-yet-created files cannot be
// assigned to both identities through relative paths or directory symlinks.
func guiCanonicalPath(path string) (string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	var tail []string
	for {
		if _, e = os.Lstat(p); e == nil {
			resolved, e := filepath.EvalSymlinks(p)
			if e != nil {
				return "", e
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(e) {
			return "", e
		}
		tail = append(tail, filepath.Base(p))
		next := filepath.Dir(p)
		if next == p {
			return "", fmt.Errorf("配置路径无法解析")
		}
		p = next
	}
}

func guiWriteBytes(path string, data []byte, mode os.FileMode) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".uulink-save-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}
