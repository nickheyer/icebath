package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestScopedEnvironGetUnscopedMultiplexPropagates(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("SOME_PROFILE_KEY", "launch-env")
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	_, _, err := config.ScopedEnvironGet(context.Background(), "SOME_PROFILE_KEY")
	var unscoped *UnscopedSecretError
	if !errors.As(err, &unscoped) {
		t.Fatalf("error %T %v", err, err)
	}
}

func TestScopedEnvironGetSingleProfileEnvRead(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("SOME_PROFILE_KEY", "own-env")
	v, ok, err := config.ScopedEnvironGet(context.Background(), "SOME_PROFILE_KEY")
	if err != nil || !ok || v != "own-env" {
		t.Fatalf("read %q %v %v", v, ok, err)
	}
}
