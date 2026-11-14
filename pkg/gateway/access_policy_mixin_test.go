package gateway

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMixinHostWithoutPrefixIsRejectedAtClassCreation(t *testing.T) {
	_, err := NewOwnAccessPolicyMixin("Host", "")
	if err == nil || !strings.Contains(err.Error(), "TypeError") || !strings.Contains(err.Error(), "ALLOW_ALL_ENV_PREFIX") {
		t.Fatalf("err %v", err)
	}
}

func TestPlatformPrefixEnvNameIsScopedAndFailClosed(t *testing.T) {
	host := func() *OwnAccessPolicyMixin {
		p, err := NewOwnAccessPolicyMixin("Host", "DEMO")
		if err != nil {
			t.Fatal(err)
		}
		p.DMPolicy, p.GroupPolicy = "open", "open"
		return p
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	t.Setenv("DEMO_ALLOW_ALL_USERS", "true")
	if names := host().AllowAllEnvNames(); !slices.Equal(names, []string{"GATEWAY_ALLOW_ALL_USERS", "DEMO_ALLOW_ALL_USERS"}) {
		t.Fatalf("names %v", names)
	}
	secondary := provider.WithSecretScope(context.Background(), map[string]string{}, "")
	if allowed, err := host().IsDMAllowed(secondary, "anyone"); err != nil || allowed {
		t.Fatalf("secondary profile borrowed the default opt in: %v %v", allowed, err)
	}
	optedIn := provider.WithSecretScope(context.Background(), map[string]string{"DEMO_ALLOW_ALL_USERS": "yes"}, "")
	if allowed, err := host().IsDMAllowed(optedIn, "anyone"); err != nil || !allowed {
		t.Fatalf("scoped opt in refused: %v %v", allowed, err)
	}
}
