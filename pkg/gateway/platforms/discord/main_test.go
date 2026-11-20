package discord

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home, err := os.MkdirTemp("", "icebath-discord-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
