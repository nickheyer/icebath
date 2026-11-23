package provider

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigHooksReadTheRunHome(t *testing.T) {
	h, _ := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 2, "providers": map[string]any{}})
	hooks := tools.ToolsConfigHooks
	if hooks.CodexAuthStatus(context.Background(), h)["logged_in"] == true || hooks.XAIOAuthStatus(context.Background(), h)["logged_in"] == true {
		t.Fatal("empty store read as logged in")
	}
	if err := hooks.SaveCodexTokens(context.Background(), h, map[string]any{"access_token": "codex-a", "refresh_token": "codex-r"}, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	logins := poolRows(t, h, "openai-codex")
	if len(logins) != 1 || logins[0]["access_token"] != "codex-a" || logins[0]["refresh_token"] != "codex-r" || logins[0]["last_refresh"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("codex logins %v", logins)
	}
	t.Setenv("ICEBATH_NONINTERACTIVE", "yes")
	if !hooks.SetupIsNoninteractive() {
		t.Fatal("noninteractive env not read")
	}
	restore := WithSetupOutput(io.Discard)
	defer restore()
	prev := xaiOAuthDeviceCodeLoginFn
	t.Cleanup(func() { xaiOAuthDeviceCodeLoginFn = prev })
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		return map[string]any{"discovery": map[string]any{}}, nil
	}
	if hooks.XAIOAuthLoginFromSetup(context.Background(), h) {
		t.Fatal("login without tokens reported success")
	}
}
