package gatewayrun

import (
	"context"
	"testing"
)

func TestInitRegistriesBuildsPerProfileHookRegistries(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r := allocGatewayRunner()
	if err := r.initRegistriesAndClocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.hooks == nil || r.hooks.byHome == nil {
		t.Fatalf("hooks = %#v", r.hooks)
	}
}
