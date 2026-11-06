package googlemeet

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	DispatchMeetBotEntry()
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
