package provider

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The explicit path lock writes its pid into the sibling lock file and reenters on the holding goroutine
func TestStoreLockPathLocksTheNamedStore(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere", "auth.json")
	s := NewStore(dir)
	unlock, err := s.LockPath(target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "elsewhere", "auth.lock"))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file %q %v", raw, err)
	}
	inner, err := s.LockPath(target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	inner()
	unlock()
}
