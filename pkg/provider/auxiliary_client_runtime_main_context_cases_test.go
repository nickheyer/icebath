package provider

import (
	"context"
	"testing"
)

var auxRuntimeMainFields = []string{"provider", "model", "base_url", "api_key", "api_mode", "auth_mode"}

// Runtime main field as the empty string when unbound
func auxRuntimeMainStr(ctx context.Context, field string) string {
	s, _ := auxRuntimeMainValue(ctx, field).(string)
	return s
}

func TestNoProcessGlobalRuntimeState(t *testing.T) {
	bound := SetRuntimeMain(context.Background(), "custom", "other-model", RuntimeMainOptions{BaseURL: "https://other.example/v1", APIKey: "sk-other"})
	if auxRuntimeMainStr(bound, "base_url") != "https://other.example/v1" {
		t.Fatalf("bound context lost its runtime")
	}
	var leaked []string
	for _, field := range auxRuntimeMainFields {
		if auxRuntimeMainValue(context.Background(), field) != nil && auxRuntimeMainStr(context.Background(), field) != "" {
			leaked = append(leaked, field)
		}
	}
	if len(leaked) != 0 || len(auxNormalizeMainRuntime(context.Background(), nil)) != 0 {
		t.Fatalf("process global runtime state %v", leaked)
	}
}

func TestABindingOnAnotherThreadNeverReachesThisOne(t *testing.T) {
	ctx := ClearRuntimeMain(auxMainTestHome(t, ""))
	bound := make(chan string, 1)
	go func() {
		other := SetRuntimeMain(ctx, "custom", "other-model", RuntimeMainOptions{BaseURL: "https://other.example/v1", APIKey: "sk-other"})
		bound <- auxRuntimeMainStr(other, "base_url")
	}()
	if got := <-bound; got != "https://other.example/v1" {
		t.Fatalf("other session saw %q", got)
	}
	if len(auxNormalizeMainRuntime(ctx, nil)) != 0 {
		t.Fatalf("runtime %v", auxNormalizeMainRuntime(ctx, nil))
	}
	if auxRuntimeMainStr(ctx, "base_url") != "" || auxRuntimeMainStr(ctx, "api_key") != "" {
		t.Fatalf("binding leaked base %q key %q", auxRuntimeMainStr(ctx, "base_url"), auxRuntimeMainStr(ctx, "api_key"))
	}
}

func TestScopedBindingIsVisibleInsideAndGoneAfter(t *testing.T) {
	ctx := ClearRuntimeMain(auxMainTestHome(t, ""))
	scoped, _ := ScopedRuntimeMain(ctx, map[string]any{"base_url": "https://scoped.example/v1", "api_key": "sk-scoped"})
	if auxRuntimeMainStr(scoped, "base_url") != "https://scoped.example/v1" {
		t.Fatalf("inside %q", auxRuntimeMainStr(scoped, "base_url"))
	}
	if auxRuntimeMainStr(ctx, "base_url") != "" {
		t.Fatalf("after %q", auxRuntimeMainStr(ctx, "base_url"))
	}
}
