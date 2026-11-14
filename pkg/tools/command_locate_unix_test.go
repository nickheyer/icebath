//go:build unix

package tools

import (
	"path/filepath"
	"testing"
)

func TestLocateCommandReturnsThePathArgv(t *testing.T) {
	got := LocateCommand("sh")
	if len(got) != 1 || !filepath.IsAbs(got[0]) {
		t.Fatalf("sh argv %v", got)
	}
	if got := LocateCommand("icebath-no-such-command-zz"); got != nil {
		t.Fatalf("missing command argv %v", got)
	}
}
