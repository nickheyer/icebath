package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/spf13/cobra"
)

func TestMemoryStatusAndSetupAreBound(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profiles", "work")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: honcho\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	c := NewCLI()
	for _, path := range []string{"memory", "memory setup", "memory status"} {
		for _, unbound := range c.Unbound() {
			if unbound == path {
				t.Fatalf("%s is unbound", path)
			}
		}
	}
	for _, run := range []struct {
		sub  string
		args []string
	}{{"status", nil}, {"", nil}, {"setup", []string{"honcho"}}} {
		out := &bytes.Buffer{}
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(out)
		cmd.SetErr(out)
		if err := runMemorySetup(cmd, run.sub, run.args); err != nil {
			t.Fatalf("%q: %v", run.sub, err)
		}
		if !strings.Contains(out.String(), "icebath -p work plugins install honcho") {
			t.Fatalf("%q output lacks the profile install command\n%s", run.sub, out.String())
		}
	}
}
