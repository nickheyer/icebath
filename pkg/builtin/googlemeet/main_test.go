package googlemeet

import (
	"context"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	DispatchMeetBotEntry(context.Background())
	os.Exit(m.Run())
}

// Isolated profile home for one test
func isolateHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir() + "/.icebath"
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}
