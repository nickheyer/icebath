package photon

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestAutoconfigureAccessFillsUnset(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, k := range []string{"PHOTON_ALLOWED_USERS", "PHOTON_HOME_CHANNEL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	ctx := context.Background()
	autoconfigureAccess(ctx, "+15551234567", io.Discard, io.Discard)
	config.InvalidateEnvCache()
	for _, k := range []string{"PHOTON_ALLOWED_USERS", "PHOTON_HOME_CHANNEL"} {
		if v, _, err := config.GetEnvValue(ctx, h, k); err != nil || v != "+15551234567" {
			t.Fatalf("%s = %q %v", k, v, err)
		}
	}
}
