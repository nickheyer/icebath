package vsc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Forgets the settings in force so the next Current reads the file again
func resetConfig() {
	cfgMu.Lock()
	cfg, cfgErr, loaded = Config{}, nil, false
	cfgMu.Unlock()
}

func TestConfigRoundTrip(t *testing.T) {
	c := Default()
	if !c.Enabled || c.AutoInstall || c.Log != "" {
		t.Fatalf("defaults %+v", c)
	}
	for _, kv := range [][2]string{{"enabled", "false"}, {"auto_install", "true"}, {"log", "/tmp/acp.log"}} {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	v := c.Values()
	if v["enabled"] != "false" || v["auto_install"] != "true" || v["log"] != "/tmp/acp.log" {
		t.Fatalf("values %+v", v)
	}
	if err := c.Set("auto_install", ""); err != nil || c.AutoInstall {
		t.Fatalf("unset auto_install: %v %v", err, c.AutoInstall)
	}
	if err := c.Set("enabled", ""); err != nil || !c.Enabled {
		t.Fatalf("unset enabled: %v %v", err, c.Enabled)
	}
	for _, kv := range [][2]string{{"enabled", "maybe"}, {"nothing", "1"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Fatalf("set %s=%s passed", kv[0], kv[1])
		}
	}
}

func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, dir)
	resetConfig()
	defer resetConfig()
	if c := Current(); !c.Enabled || c.AutoInstall {
		t.Fatalf("no file gives %+v", c)
	}
	if err := LoadError(); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Enabled, c.Log = false, "~/acp.log"
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	if filepath.Dir(path) != filepath.Join(dir, "ext") {
		t.Fatalf("settings at %s", path)
	}
	resetConfig()
	if got := Current(); got.Enabled || got.Log != "~/acp.log" {
		t.Fatalf("saved then loaded %+v", got)
	}
	if err := os.WriteFile(path, []byte(`{"enabled": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetConfig()
	if got := Current(); got.AutoInstall {
		t.Fatalf("file without auto_install should keep it off, got %+v", got)
	}
	if err := os.WriteFile(path, []byte(`{"enabled": true, "auto_install": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetConfig()
	if got := Current(); !got.AutoInstall {
		t.Fatalf("file turning auto_install on should be read, got %+v", got)
	}
	if err := os.WriteFile(path, []byte(`{"enabled": `), 0o600); err != nil {
		t.Fatal(err)
	}
	resetConfig()
	if got := Current(); got != Default() {
		t.Fatalf("bad file should leave the defaults, got %+v", got)
	}
	if err := LoadError(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("load error %v", err)
	}
}

func TestSocketRequested(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	resetConfig()
	defer resetConfig()
	t.Setenv(EnvVar, "")
	if _, ok := SocketRequested(); ok {
		t.Fatal("unset variable named a socket")
	}
	t.Setenv(EnvVar, " /run/user/1/icebath.sock ")
	if path, ok := SocketRequested(); !ok || path != "/run/user/1/icebath.sock" {
		t.Fatalf("socket %q %v", path, ok)
	}
	c := Default()
	c.Enabled = false
	if err := Apply(c); err != nil {
		t.Fatal(err)
	}
	if _, ok := SocketRequested(); ok {
		t.Fatal("disabled bridge still requested")
	}
}

func TestTraceLog(t *testing.T) {
	dir := t.TempDir()
	l := &traceLog{}
	l.trace("in", []byte(`{"jsonrpc":"2.0"}`))
	l.infof("nothing %d", 1)
	first := filepath.Join(dir, "a.log")
	l.setPath(first)
	l.trace("out", []byte("line one\n"))
	l.infof("note %s", "two")
	second := filepath.Join(dir, "b.log")
	l.setPath(second)
	l.trace("in", []byte("line three"))
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(a)), "\n"); len(lines) != 2 || !strings.HasSuffix(lines[0], " out line one") || !strings.HasSuffix(lines[1], " info note two") {
		t.Fatalf("first log %q", a)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(b)), " in line three") {
		t.Fatalf("second log %q", b)
	}
	l.setPath("")
	l.trace("in", []byte("dropped"))
	if again, _ := os.ReadFile(second); string(again) != string(b) {
		t.Fatal("unset log kept writing")
	}
}
