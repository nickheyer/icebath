package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestExpansionUnderProfileScope(t *testing.T) {
	t.Run("test_scoped_ref_never_reads_another_profiles_environ", func(t *testing.T) {
		t.Setenv("MATRIX_ACCESS_TOKEN", "default-token")
		wasActive := MultiplexProcess()
		SetMultiplexActive(true)
		t.Cleanup(func() { SetMultiplexActive(wasActive) })
		expand := func(ctx context.Context, s string) string {
			return config.ExpandString(s, config.EnvRefLookupContext(ctx))
		}
		other := WithSecretScope(context.Background(), map[string]string{"OTHER_KEY": "x"}, "")
		if got := expand(other, "${MATRIX_ACCESS_TOKEN}"); got != "${MATRIX_ACCESS_TOKEN}" {
			t.Fatalf("bare ref %q", got)
		}
		if got := expand(other, "${env:MATRIX_ACCESS_TOKEN}"); got != "${env:MATRIX_ACCESS_TOKEN}" {
			t.Fatalf("env ref %q", got)
		}
		own := WithSecretScope(context.Background(), map[string]string{"MATRIX_ACCESS_TOKEN": "c-token"}, "")
		if got := expand(own, "${MATRIX_ACCESS_TOKEN}"); got != "c-token" {
			t.Fatalf("scoped value %q", got)
		}
		SetMultiplexActive(wasActive)
		if got := expand(context.Background(), "${MATRIX_ACCESS_TOKEN}"); got != "default-token" {
			t.Fatalf("unscoped %q", got)
		}
	})
}
