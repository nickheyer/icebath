package pm

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
)

// Root whose own commands a child process republishes, printing the published and expected counts
const republishChild = "ICEBATH_PM_REPUBLISH_CHILD"

// Marks a child process that stays alive until its stdin closes
const sleepChild = "ICEBATH_PM_SLEEP_CHILD"

// Dispatches the child modes the launcher cases run the test binary in before the suite
func TestMain(m *testing.M) {
	if root := os.Getenv(republishChild); root != "" {
		written, err := EnsureInstallLaunchers(context.Background(), root, root)
		fmt.Printf("published %d %d\n", len(written), len(windowsBinLaunchers))
		if err != nil || len(written) != len(windowsBinLaunchers) {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(sleepChild) != "" {
		fmt.Println("ready")
		_, _ = io.ReadAll(os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
