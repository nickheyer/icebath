package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes an executable shell script and returns its path
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveStdioCommandKeepsChildPathOrder(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	first, middle, later := filepath.Join(root, "first"), filepath.Join(root, "middle"), filepath.Join(root, "later")
	os.MkdirAll(first, 0o755)
	os.MkdirAll(middle, 0o755)
	tool := writeExecutable(t, later, "mytool")
	path := strings.Join([]string{first, middle, later}, string(os.PathListSeparator))
	command, env, err := resolveStdioCommand(context.Background(), "mytool", map[string]string{"PATH": path})
	if err != nil || command != tool || env["PATH"] != path {
		t.Fatalf("command %q PATH %q err %v", command, env["PATH"], err)
	}
}

func TestResolveStdioCommandSkipsUnknownCommands(t *testing.T) {
	isolateHome(t)
	command, _, err := resolveStdioCommand(context.Background(), "my-tool", map[string]string{"PATH": t.TempDir()})
	if err != nil || command != "my-tool" {
		t.Fatalf("command %q err %v", command, err)
	}
}

func TestResolveStdioCommandAbsentOrEmptyPathIsAMiss(t *testing.T) {
	isolateHome(t)
	parent := t.TempDir()
	writeExecutable(t, parent, "some-mcp-server")
	writeExecutable(t, parent, "other-mcp-server")
	t.Setenv("PATH", parent)
	if command, _, err := resolveStdioCommand(context.Background(), "some-mcp-server", map[string]string{"OTHER": "1"}); err != nil || command != "some-mcp-server" {
		t.Fatalf("absent PATH resolved %q err %v", command, err)
	}
	t.Chdir(t.TempDir())
	if command, _, err := resolveStdioCommand(context.Background(), "other-mcp-server", map[string]string{"PATH": ""}); err != nil || command != "other-mcp-server" {
		t.Fatalf("empty PATH resolved %q err %v", command, err)
	}
}

func TestConfigPathextLookupNeverTouchesParentEnviron(t *testing.T) {
	serverDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(serverDir, "server.cmd"), []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATHEXT", "")
	os.Unsetenv("PATHEXT")
	t.Setenv("PATH", serverDir)
	hit := firstUserWhichHit(context.Background(), "server", serverDir, map[string]string{"PATHEXT": ".cmd;.exe"}, true)
	if hit != filepath.Join(serverDir, "server.cmd") {
		t.Fatalf("hit %q", hit)
	}
	if _, set := os.LookupEnv("PATHEXT"); set {
		t.Fatal("PATHEXT written to the parent environment")
	}
}
