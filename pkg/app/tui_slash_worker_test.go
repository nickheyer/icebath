package app

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSlashWorkerChildForwardsProviderToHermesCli(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SESSION_KEY", "")
	t.Setenv("ICEBATH_INTERACTIVE", "")
	t.Setenv("ICEBATH_QUIET", "")
	var built CLIOptions
	savedNew, savedPrepare := slashWorkerNewCLI, slashWorkerPrepare
	slashWorkerNewCLI = func(_ context.Context, o CLIOptions) (*CLI, error) {
		built = o
		return &CLI{}, nil
	}
	slashWorkerPrepare = func(context.Context, *tools.Env) {}
	t.Cleanup(func() { slashWorkerNewCLI, slashWorkerPrepare = savedNew, savedPrepare })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	savedStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = savedStdin
		_ = r.Close()
	})
	if code := slashWorkerMain(context.Background(), []string{"--session-key", "s", "--model", "m", "--provider", "moa"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if built.Provider != "moa" || built.Model != "m" {
		t.Fatalf("built %+v", built)
	}
}
