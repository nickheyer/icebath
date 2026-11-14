package raft

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func TestRaftConfigInteractiveSetupKeepsExistingProfileWhenNotReconfigured(t *testing.T) {
	h := t.TempDir()
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("RAFT_PROFILE=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("RAFT_PROFILE", "existing")
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	def := cliui.Default()
	savedIn, savedTTY := def.In, def.StdinTTY
	def.In, def.StdinTTY = strings.NewReader("n\n"), func() bool { return false }
	t.Cleanup(func() { def.In, def.StdinTTY = savedIn, savedTTY })
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil || string(raw) != "RAFT_PROFILE=existing\n" {
		t.Fatalf("env file %q %v", raw, err)
	}
	if os.Getenv("RAFT_PROFILE") != "existing" {
		t.Fatalf("RAFT_PROFILE %q", os.Getenv("RAFT_PROFILE"))
	}
}
