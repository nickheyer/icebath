package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Installs the CLI prefill resolver and loader the process wiring takes from pkg/app, which this package cannot import
func installPrefillLoaders(t *testing.T) {
	t.Helper()
	prevResolve, prevLoad := gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages
	t.Cleanup(func() { gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = prevResolve, prevLoad })
	gateway.ResolvePrefillMessagesFile = func(ctx context.Context, cfg map[string]any) string {
		if v := strings.TrimSpace(state.SessionEnv(ctx, "ICEBATH_PREFILL_MESSAGES_FILE", "")); v != "" {
			return v
		}
		if v, _ := cfg["prefill_messages_file"].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if agentCfg, ok := cfg["agent"].(map[string]any); ok {
			v, _ := agentCfg["prefill_messages_file"].(string)
			return strings.TrimSpace(v)
		}
		return ""
	}
	gateway.LoadPrefillMessages = func(ctx context.Context, filePath, baseDir string) []any {
		if filePath == "" {
			return []any{}
		}
		path := pypath.Expanduser(filePath)
		if !filepath.IsAbs(path) {
			if baseDir == "" {
				baseDir = home.Dir(ctx)
			}
			path = filepath.Join(baseDir, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return []any{}
		}
		data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
		if err != nil {
			return []any{}
		}
		list, ok := pyjson.Plain(data).([]any)
		if !ok {
			return []any{}
		}
		return list
	}
}

func TestCronResultForALocalOriginWithoutItsOwnerIsAnError(t *testing.T) {
	s, _ := newTestStore(t)
	installPrefillLoaders(t)
	ctx := home.WithOverride(context.Background(), s.Home())
	prev := gateway.SwapSessionSchedule(func(context.Context, *gateway.SessionAuthority, gateway.SessionRef) error { return nil })
	t.Cleanup(func() { gateway.SwapSessionSchedule(prev) })
	db, err := state.NewSessionDB(ctx, filepath.Join(s.Home(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	runner := progressStampRunner{store: gateway.NewSessionStore(ctx, filepath.Join(s.Home(), "sessions"), gateway.DefaultGatewayConfig(ctx), nil),
		adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}
	authority, err := gateway.InitializeSessionAuthority(ctx, runner, "default", "lc7", db, false)
	if err != nil {
		t.Fatal(err)
	}
	actor := gateway.Principal{Subject: "test-owner", ProfileID: "default",
		Capabilities: map[string]bool{"session:create": true, "session:submit": true, "session:read": true}, TransportID: "fixture"}
	ref, err := gateway.CreateLocalSession(ctx, authority, actor, map[string]any{"request_id": "lc7", "source": "cli", "model": "fixture", "toolsets": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	gateway.UnbindCronOwner(authority)
	recorded := resolveDeliveryTargets
	t.Cleanup(func() { resolveDeliveryTargets = recorded })
	resolveDeliveryTargets = realResolveDeliveryTargets
	job := &Job{ID: "j1", Name: "digest", Deliver: prompt("origin"), Origin: map[string]any{"platform": "local", "chat_id": ref.SessionID}}
	errText, err := realDeliverResult(s, ctx, job, "CRON_RESULT_j1", runner.adapters, nil, false)
	if err != nil || errText == nil || !strings.Contains(*errText, "local") {
		t.Fatalf("error = %v, %v", errText, err)
	}
	rows, err := db.ListSessionAdmissions(ctx, ref.SessionID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if payload, _ := row.Get("payload"); strings.Contains(pyrepr.Str(payload), "CRON_RESULT_j1") {
			t.Fatalf("admitted %v", payload)
		}
	}
}
