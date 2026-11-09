package provider

import (
	"context"
	"errors"
	"os"
	"testing"
)

func openRouterMultiplex(t *testing.T) {
	t.Helper()
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
}

func TestOpenRouterCheckApiKeyScopedValueWins(t *testing.T) {
	os.Unsetenv("OPENROUTER_API_KEY")
	openRouterMultiplex(t)
	ctx := WithSecretScope(context.Background(), map[string]string{"OPENROUTER_API_KEY": "sk-or-scoped"}, "")
	ok, err := CheckOpenRouterAPIKey(ctx)
	if err != nil || !ok {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestOpenRouterCheckApiKeyScopedMissNoBorrow(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-other-profile")
	openRouterMultiplex(t)
	ctx := WithSecretScope(context.Background(), map[string]string{"UNRELATED": "x"}, "")
	ok, err := CheckOpenRouterAPIKey(ctx)
	if err != nil || ok {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestOpenRouterCheckApiKeyScopeFailureNeverBorrowsEnv(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-other-profile")
	openRouterMultiplex(t)
	boom := errors.New("resolver boom")
	saved := openRouterGetSecret
	openRouterGetSecret = func(ctx context.Context, name string) (string, bool, error) {
		if SecretScopeFrom(ctx).Mapping != nil {
			return "", false, boom
		}
		return GetSecret(ctx, name)
	}
	t.Cleanup(func() { openRouterGetSecret = saved })
	ctx := WithSecretScope(context.Background(), map[string]string{}, "")
	ok, err := CheckOpenRouterAPIKey(ctx)
	if !errors.Is(err, boom) || ok {
		t.Fatalf("got %v %v", ok, err)
	}
}
