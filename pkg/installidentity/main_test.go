package installidentity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

const raceHelperEnv = "ICEBATH_INSTALL_ID_RACE"

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if spec := os.Getenv(raceHelperEnv); spec != "" {
		os.Exit(raceFirstInstallID(spec))
	}
	os.Exit(m.Run())
}

// Waits up to ten seconds for path to exist
func awaitFile(path string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Child process body that mints a fixed identity, optionally after a start barrier or while holding the writer
func raceFirstInstallID(spec string) int {
	parts := strings.SplitN(spec, "|", 4)
	root, minted, startBarrier, held := parts[0], parts[1], parts[2], parts[3]
	if startBarrier != "" {
		if err := os.WriteFile(startBarrier+"."+minted, nil, 0o600); err != nil {
			return 2
		}
		peers, _ := filepath.Glob(startBarrier + ".*")
		for len(peers) < 2 {
			time.Sleep(time.Millisecond)
			peers, _ = filepath.Glob(startBarrier + ".*")
		}
	}
	mint := func() string {
		if held != "" {
			if err := os.WriteFile(held+".entered", nil, 0o600); err != nil {
				panic(err)
			}
			if !awaitFile(held + ".release") {
				panic("release never arrived")
			}
		}
		return minted
	}
	value, err := readOrCreate(context.Background(), root, mint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(value)
	return 0
}
