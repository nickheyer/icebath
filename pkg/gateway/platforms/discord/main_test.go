package discord

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Home every test starts in until it takes its own
var packageHome string

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home, err := os.MkdirTemp("", "icebath-discord-home")
	if err != nil {
		panic(err)
	}
	packageHome = home
	os.Setenv("ICEBATH_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
