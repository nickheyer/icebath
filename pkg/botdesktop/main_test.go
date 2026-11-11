package botdesktop

import (
	"fmt"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

const flockProbeEnv = "ICEBATH_BOTDESKTOP_FLOCK_PROBE"

// Runs the flock probe of the cross process slot case when asked, the suite otherwise
func TestMain(m *testing.M) {
	if path := os.Getenv(flockProbeEnv); path != "" {
		fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		if flockExclusiveNonBlocking(fh) != nil {
			fmt.Println("held")
		} else {
			fmt.Println("free")
		}
		os.Exit(0)
	}
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}
