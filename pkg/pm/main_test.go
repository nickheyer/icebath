package pm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Root whose own commands a child process republishes, printing the published and expected counts
const republishChild = "ICEBATH_PM_REPUBLISH_CHILD"

// Marks a child process that stays alive until its stdin closes
const sleepChild = "ICEBATH_PM_SLEEP_CHILD"

// Dispatches the child modes the launcher cases run the test binary in before the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if root := os.Getenv(republishChild); root != "" {
		written, err := EnsureInstallLaunchers(context.Background(), root, root)
		fmt.Printf("published %d %d\n", len(written), len(windowsBinLaunchers))
		if err != nil || len(written) != len(windowsBinLaunchers) {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if root := os.Getenv(publicationProbe); root != "" {
		os.Exit(publicationProbeMain(root))
	}
	if project := os.Getenv(bootLeaseChild); project != "" {
		os.Exit(bootLeaseChildMain(project))
	}
	if os.Getenv(launchSelectionChild) != "" {
		os.Exit(launchSelectionChildMain())
	}
	if os.Getenv(sleepChild) != "" {
		fmt.Println("ready")
		_, _ = io.ReadAll(os.Stdin)
		os.Exit(0)
	}
	InteractiveAppRunning = func() bool { return false }
	references, err := os.MkdirTemp("", "icebath-pm-references-")
	if err == nil {
		peReferences = references
		var archive []byte
		if archive, err = os.ReadFile(filepath.Join("testdata", "hermes.tar.zst")); err == nil {
			err = tarzst.Extract(archive, references)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(references)
	os.Exit(code)
}
