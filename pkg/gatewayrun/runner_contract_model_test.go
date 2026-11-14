package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Local desktop session created over a real runner's own authority, with its owner connection
type contractLocalSession struct {
	ctx       context.Context
	runner    *GatewayRunner
	authority *gateway.SessionAuthority
	owner     *gateway.AuthorityConnection
	sid       string
	live      *gateway.LiveSession
}

// Installs the prefill loaders cmd/icebath wires from the CLI config, a relative file read under the base dir
func installContractPrefillLoaders(t *testing.T) {
	t.Helper()
	prevResolve, prevLoad := gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages
	gateway.ResolvePrefillMessagesFile = func(ctx context.Context, cfg map[string]any) string {
		text, _ := cfg["prefill_messages_file"].(string)
		return strings.TrimSpace(text)
	}
	gateway.LoadPrefillMessages = func(ctx context.Context, filePath, baseDir string) []any {
		if filePath == "" {
			return []any{}
		}
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(baseDir, filePath)
		}
		raw, err := os.ReadFile(filePath)
		if err != nil {
			return []any{}
		}
		decoded, err := pyjson.Loads(string(raw))
		if err != nil {
			return []any{}
		}
		list, _ := pyjson.Plain(decoded).([]any)
		return list
	}
	t.Cleanup(func() { gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = prevResolve, prevLoad })
}

func newContractLocalSession(t *testing.T, configYAML string) *contractLocalSession {
	t.Helper()
	installContractPrefillLoaders(t)
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runner, err := NewGatewayRunner(ctx, gateway.DefaultGatewayConfig(ctx))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := gateway.InitializeSessionAuthority(ctx, runner, "default", "owner", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := gateway.NewAuthorityConnection(authority, picksTransport{}, map[string]any{"user_id": "human"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = owner.Close(ctx)
		runner.sessionStore.CloseAllDBHandles()
	})
	created, err := owner.Dispatch(ctx, pyjson.ObjectOf("id", "c", "method", "session.create", "params", pyjson.ObjectOf(
		"request_id", "desk", "source", "gui", "cwd", dir, "toolsets", []any{}, "model", "m")))
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := picksField(created, "result").Get("session_id")
	text, _ := sid.(string)
	live, ok := authority.Live(text)
	if !ok {
		t.Fatalf("created %s", picksDump(created))
	}
	return &contractLocalSession{ctx: ctx, runner: runner, authority: authority, owner: owner, sid: text, live: live}
}

// Session mutate on the local session with the fences of its stored row
func (s *contractLocalSession) mutate(t *testing.T, operation string, payload *pyjson.Object, requestID string) *pyjson.Object {
	t.Helper()
	row, err := s.authority.DB.GetSession(s.ctx, s.sid)
	if err != nil {
		t.Fatal(err)
	}
	fences := pyjson.NewObject()
	revision, _ := row.Get("runtime_revision")
	generation, _ := row.Get("runtime_generation")
	fences.Set("revision", revision)
	fences.Set("execution_generation", generation)
	response, err := s.owner.Dispatch(s.ctx, picksMutate(s.sid, fences, operation, payload, requestID))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// A committed local reasoning pick drops the route's typed reasoning override through the real runner
func TestLocalReasoningPickClearsTheTypedOverrideOnARealRunner(t *testing.T) {
	s := newContractLocalSession(t, "model:\n  default: m\nagent:\n  reasoning_effort: low\n")
	s.runner.SetSessionReasoningOverride(s.live.Route, map[string]any{"enabled": true, "effort": "xhigh"})
	typed, err := s.runner.ResolveSessionReasoningConfig(s.ctx, s.live.Source, s.live.Route, "")
	if err != nil || typed["effort"] != "xhigh" {
		t.Fatalf("typed %v %v", typed, err)
	}

	picked := s.mutate(t, "reasoning", pyjson.ObjectOf("effort", "high"), "pick")

	if effort, _ := picksField(picked, "result").Get("effort"); effort != "high" {
		t.Fatalf("picked %s", picksDump(picked))
	}
	if state := s.runner.PeekSessionState(s.live.Route); state != nil && state.ReasoningOverride() != nil {
		t.Fatalf("override kept %v", state.ReasoningOverride())
	}
	level, err := s.runner.ResolveSessionReasoningConfig(s.ctx, s.live.Source, s.live.Route, "")
	if err != nil || level["effort"] != "high" {
		t.Fatalf("level %v %v", level, err)
	}
}

// The policy tier reads a typed fast pick, an explicit normal and a cleared pick through the real runner's peek
func TestLocalPolicyTierReadsTheRealRunnersPeekedState(t *testing.T) {
	s := newContractLocalSession(t, "model:\n  default: m\nagent:\n  service_tier: fast\n")
	policy, err := gateway.SessionPolicyForSource(s.ctx, s.runner, s.live.Source)
	if err != nil || policy == nil {
		t.Fatalf("policy %v %v", policy, err)
	}
	tierText := func(tier *string) string {
		if tier == nil {
			return "<none>"
		}
		return *tier
	}
	if tier, err := policy.CurrentServiceTier(s.ctx, s.runner, s.live.Route); err != nil || tierText(tier) != "priority" {
		t.Fatalf("frozen tier %s %v", tierText(tier), err)
	}
	s.runner.SetSessionServiceTierOverride(s.live.Route, nil, false)
	if tier, err := policy.CurrentServiceTier(s.ctx, s.runner, s.live.Route); err != nil || tier != nil {
		t.Fatalf("explicit normal %s %v", tierText(tier), err)
	}
	fast := "priority"
	s.runner.SetSessionServiceTierOverride(s.live.Route, &fast, false)
	if tier, err := s.runner.ResolveSessionServiceTier(s.ctx, s.live.Source, s.live.Route); err != nil || tierText(tier) != "priority" {
		t.Fatalf("typed fast %s %v", tierText(tier), err)
	}
	s.runner.SetSessionServiceTierOverride(s.live.Route, nil, true)
	if tier, err := policy.CurrentServiceTier(s.ctx, s.runner, s.live.Route); err != nil || tierText(tier) != "priority" {
		t.Fatalf("cleared %s %v", tierText(tier), err)
	}
}
