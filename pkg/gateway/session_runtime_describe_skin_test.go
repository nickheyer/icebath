package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Authority over a real store whose profile id is the state database's home, as the local source label cases build it
func newDescribeAuthority(t *testing.T) (context.Context, *SessionAuthority) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	runner := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](),
		profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a, err := InitializeSessionAuthority(ctx, runner, filepath.Dir(db.Path()), "test", db, false)
	if err != nil {
		t.Fatal(err)
	}
	runner.launch = a
	t.Setenv("ICEBATH_HOME", a.ProfileID)
	return ctx, a
}

// runtime.describe reply of an owner connection as a plain JSON tree
func describeReply(t *testing.T, ctx context.Context, a *SessionAuthority) map[string]any {
	t.Helper()
	conn, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	described, err := conn.Dispatch(ctx, pyjson.ObjectOf("id", int64(0), "method", "runtime.describe", "params", pyjson.NewObject()))
	if err != nil {
		t.Fatal(err)
	}
	return wireReply(t, described)
}

func TestRuntimeDescribeCarriesTheProfilesResolvedSkin(t *testing.T) {
	ctx, a := newDescribeAuthority(t)
	if err := os.WriteFile(filepath.Join(a.ProfileID, "config.yaml"), []byte("display:\n  skin: ares\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	described := describeReply(t, ctx, a)
	result, ok := described["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", described)
	}
	skin, _ := result["skin"].(map[string]any)
	if skin["name"] != "ares" {
		t.Fatalf("skin %v", skin["name"])
	}
	if colors, _ := skin["colors"].(map[string]any); len(colors) == 0 {
		t.Fatalf("colors %v", skin["colors"])
	}
	if branding, _ := skin["branding"].(map[string]any); len(branding) == 0 {
		t.Fatalf("branding %v", skin["branding"])
	}
	if invalid := tuigateway.CanonicalMethods["runtime.describe"].Result.ModelValidate(result); invalid != nil {
		t.Fatal(invalid)
	}
}

func TestRuntimeDescribeAdvertisesClarifyBatch(t *testing.T) {
	ctx, a := newDescribeAuthority(t)
	result, _ := describeReply(t, ctx, a)["result"].(map[string]any)
	capabilities, _ := result["capabilities"].([]any)
	if !slices.Contains(capabilities, any("clarify-batch-v1")) {
		t.Fatalf("capabilities %v", result["capabilities"])
	}
	if !slices.Contains(tuigateway.CanonicalMethods["prompt.submit"].Params.ModelFields(), "clarify_batch") {
		t.Fatal("prompt.submit declares no clarify_batch")
	}
}
