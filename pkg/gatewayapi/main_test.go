package gatewayapi

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	dir, err := os.MkdirTemp("", "gatewayapi-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
