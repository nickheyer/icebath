package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Bin path npm lays down in an npx cache entry, the cmd launcher on Windows
func npxCacheBin(binDir, name string) string {
	target := filepath.Join(binDir, name)
	if runtime.GOOS == "windows" {
		target += ".cmd"
	}
	return target
}

func TestPreflightChecksOriginalPackageBeforeCacheAccess(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("npm_config_cache", cache)
			entry := filepath.Join(cache, "_npx", "abc123")
			os.MkdirAll(filepath.Join(entry, "node_modules", ".bin"), 0o755)
			os.MkdirAll(filepath.Join(entry, "node_modules", "mcp-linear"), 0o755)
			os.WriteFile(filepath.Join(entry, "package.json"), []byte(`{"dependencies": {"mcp-linear": "1"}}`), 0o644)
			os.WriteFile(filepath.Join(entry, "node_modules", "mcp-linear", "package.json"), []byte(`{"bin": {"mcp-linear": "i.js"}}`), 0o644)
			target := npxCacheBin(filepath.Join(entry, "node_modules", ".bin"), "mcp-linear")
			os.WriteFile(target, []byte("#!/usr/bin/env node\n"), 0o755)
			args := []string{"-y", "mcp-linear", "--port", "7"}
			var seen [][]string
			prevCheck, prevLookup := malwareCheck, npxCacheLookup
			t.Cleanup(func() { malwareCheck, npxCacheLookup = prevCheck, prevLookup })
			malwareCheck = func(command string, arguments []string) (string, error) {
				seen = append(seen, append([]string{command}, arguments...))
				if rejected {
					return "malicious package", nil
				}
				return "", nil
			}
			if rejected {
				npxCacheLookup = func([]string) (string, []string, bool, error) {
					t.Fatal("cache read before refusal")
					return "", nil, false, nil
				}
				if _, _, err := preflightStdioCommand(context.Background(), "server", "npx", args); err == nil || !strings.Contains(err.Error(), "malicious package") {
					t.Fatalf("got %v", err)
				}
			} else {
				cmd, rest, err := preflightStdioCommand(context.Background(), "server", "npx", args)
				if err != nil || cmd != target || !reflect.DeepEqual(rest, []string{"--port", "7"}) {
					t.Fatalf("got %q %v %v", cmd, rest, err)
				}
			}
			if !reflect.DeepEqual(seen, [][]string{{"npx", "-y", "mcp-linear", "--port", "7"}}) {
				t.Fatalf("malware check saw %v", seen)
			}
		})
	}
}

func TestWindowsSelectsLaunchersNeverTheShScript(t *testing.T) {
	win := npxBinCandidates("/c/bin", "mcp-linear", true)
	if !reflect.DeepEqual(win, []string{filepath.Join("/c/bin", "mcp-linear.cmd"), filepath.Join("/c/bin", "mcp-linear.exe")}) {
		t.Fatalf("windows candidates %v", win)
	}
	for _, c := range win {
		if strings.HasSuffix(c, "mcp-linear") {
			t.Fatal("sh script must not be a candidate")
		}
	}
	if got := npxBinCandidates("/bin", "mcp-linear", false); !reflect.DeepEqual(got, []string{filepath.Join("/bin", "mcp-linear")}) {
		t.Fatalf("posix candidates %v", got)
	}
}
