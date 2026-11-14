package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// tests/tools/test_mcp_session_scope.py session_tool_scope contract, current_session_tool_scope and _session_slot_key
func TestSessionToolScopeReturnsTheBoundValueAndKeysReadsLikeWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	if CurrentSessionToolScope(ctx) != "" || SessionToolScope(ctx) != "" {
		t.Fatal("unbound context carries a session scope")
	}
	bound := WithSessionToolScope(ctx, "editor-session:a")
	if got := CurrentSessionToolScope(bound); got != "editor-session:a" {
		t.Fatalf("current scope %q", got)
	}
	if got, want := SessionToolScope(bound), home.NormalizeScope("editor-session:a"); got != want || !filepath.IsAbs(got) {
		t.Fatalf("slot key %q want %q", got, want)
	}
	if got := CurrentSessionToolScope(WithSessionToolScope(bound, "")); got != "" {
		t.Fatalf("inner unbound scope %q", got)
	}
	r := NewRegistry()
	if err := r.RegisterScoped("editor-session:a", registerDef("mcp__owned_a__echo", "mcp-owned_a", nil)); err != nil {
		t.Fatal(err)
	}
	if r.Lookup(bound, "", "mcp__owned_a__echo") == nil {
		t.Fatal("overlay registered under the raw scope is invisible to its own session")
	}
	if r.Lookup(ctx, "", "mcp__owned_a__echo") != nil || r.Lookup(WithSessionToolScope(ctx, "editor-session:b"), "", "mcp__owned_a__echo") != nil {
		t.Fatal("session overlay leaked outside its scope")
	}
	base := t.TempDir()
	scope := filepath.Join(base, "link", "sess")
	later := WithSessionToolScope(ctx, scope)
	before := SessionToolScope(later)
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	after := SessionToolScope(later)
	if after == before || after != home.NormalizeScope(scope) || CurrentSessionToolScope(later) != scope {
		t.Fatalf("slot key not recomputed per read: before %q after %q", before, after)
	}
}

// Deregister and restore_registration judge toolset survivors through _merged_tools, which adds the home and session overlays
func TestToolsetSurvivorsIncludeTheHomeAndSessionOverlays(t *testing.T) {
	profile := t.TempDir()
	session := t.TempDir()
	ctx := home.WithOverride(context.Background(), profile)
	check := func(*Env) bool { return true }
	for _, tc := range []struct {
		name    string
		overlay string
		ctx     context.Context
		keeps   bool
	}{
		{"home overlay", profile, ctx, true},
		{"session overlay", session, WithSessionToolScope(context.Background(), session), true},
		{"unrelated overlay", session, ctx, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			global := registerDef("ts_global", "shared-ts", nil)
			global.Check = check
			if err := r.RegisterScoped("", global); err != nil {
				t.Fatal(err)
			}
			if err := r.RegisterScoped(tc.overlay, registerDef("ts_overlay", "shared-ts", nil)); err != nil {
				t.Fatal(err)
			}
			if err := r.DeregisterIn(tc.ctx, "ts_global", "", ""); err != nil {
				t.Fatal(err)
			}
			if _, kept := r.ToolsetChecks()["shared-ts"]; kept != tc.keeps {
				t.Fatalf("toolset check kept %v", kept)
			}
		})
	}
	r := NewRegistry()
	first := func(*Env) bool { return false }
	current := registerDef("restored", "restore-ts", nil)
	if err := r.RegisterScoped("", current); err != nil {
		t.Fatal(err)
	}
	installed := r.SnapshotRegistration("", "restored")
	overlay := registerDef("restore_overlay", "restore-ts", nil)
	overlay.Check = first
	if err := r.RegisterScoped(profile, overlay); err != nil {
		t.Fatal(err)
	}
	if !r.RestoreRegistrationIn(ctx, "restored", installed, nil, "") {
		t.Fatal("restore refused the current entry")
	}
	got, ok := r.ToolsetChecks()["restore-ts"]
	if !ok || got(nil) {
		t.Fatal("restore did not rebuild the check from the home overlay survivor")
	}
}

// tools/registry.py _normalize_handler_result over every result shape a hosted handler can return
func TestNormalizeHandlerResultMatchesTheRegistryContract(t *testing.T) {
	env, rec := recordingEnv()
	if got := NormalizeHandlerResult(env, "t", `{"ok": true}`); got != `{"ok": true}` {
		t.Fatalf("str %q", got)
	}
	envelope := map[string]any{"_multimodal": true, "content": []any{map[string]any{"type": "text", "text": "hi"}}}
	if got := NormalizeHandlerResult(env, "t", envelope); got != `{"_multimodal": true, "content": [{"text": "hi", "type": "text"}]}` {
		t.Fatalf("envelope %q", got)
	}
	ordered := pyjson.NewObject()
	ordered.Set("_multimodal", true)
	ordered.Set("content", []any{})
	ordered.Set("text_summary", "s")
	if got := NormalizeHandlerResult(env, "t", ordered); got != `{"_multimodal": true, "content": [], "text_summary": "s"}` {
		t.Fatalf("ordered envelope %q", got)
	}
	notEnvelope := pyjson.NewObject()
	notEnvelope.Set("_multimodal", true)
	notEnvelope.Set("content", "text")
	for _, tc := range []struct {
		result any
		typ    string
	}{
		{map[string]any{"ok": true}, "dict"}, {map[string]any{"_multimodal": 1, "content": []any{}}, "dict"},
		{notEnvelope, "dict"}, {nil, "NoneType"}, {int64(3), "int"}, {1.5, "float"}, {true, "bool"}, {[]any{"a"}, "list"},
	} {
		want := `{"error": "Tool handler returned unsupported result type: ` + tc.typ + `", "error_type": "tool_result_contract", "tool": "t", "result_type": "` + tc.typ + `"}`
		if got := NormalizeHandlerResult(env, "t", tc.result); got != want {
			t.Fatalf("%#v gave %q", tc.result, got)
		}
		if !slices.Contains(rec.messages(), "Tool t handler returned unsupported result type: "+tc.typ) {
			t.Fatalf("no error log for %s in %v", tc.typ, rec.messages())
		}
	}
}
