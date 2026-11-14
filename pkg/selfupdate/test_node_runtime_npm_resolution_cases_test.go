//go:build !windows

package selfupdate

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestWindowsNpmPathRefusesWindowsShimsButNotNativeDataMounts(t *testing.T) {
	for _, p := range []string{"/mnt/c/Program Files/nodejs/npm", "/mnt/d/nodejs/npm", "C:\\nodejs\\npm.cmd"} {
		if !isWindowsNpmPath(p) {
			t.Fatalf("%q must count as Windows npm", p)
		}
	}
	for _, p := range []string{"/mnt/data/icebath/home/.local/bin/npm", "/usr/bin/npm"} {
		if isWindowsNpmPath(p) {
			t.Fatalf("%q must not count as Windows npm", p)
		}
	}
}

func TestResolveNodeRuntimeNpmNeverRescansPathPastAWindowsShim(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/mnt/c/Program Files/nodejs", "/mnt/data/node/bin"}, string(os.PathListSeparator)))
	prev := findNodeExecutable
	t.Cleanup(func() { findNodeExecutable = prev })
	findNodeExecutable = func(context.Context, string) (string, error) { return "/mnt/c/Program Files/nodejs/npm", nil }
	got, err := resolveNodeRuntimeNpm(context.Background())
	if err != nil || got != "" {
		t.Fatalf("resolved %q %v, want no npm", got, err)
	}
}
