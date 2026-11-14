package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Swaps the code skew probe for the test
func pinCodeSkew(t *testing.T, bootRev, diskRev string, skewed bool) {
	t.Helper()
	saved := detectCodeSkew
	detectCodeSkew = func() (string, string, bool) { return bootRev, diskRev, skewed }
	t.Cleanup(func() { detectCodeSkew = saved })
}

// Swaps the model options payload builder for the test
func pinModelOptionsBuilder(t *testing.T, build func(context.Context, provider.PickerConfigContext, bool, bool, bool) (map[string]any, error)) {
	t.Helper()
	saved := buildModelOptionsPayload
	buildModelOptionsPayload = build
	t.Cleanup(func() { buildModelOptionsPayload = saved })
}

// Error object of a response, failing when the call answered a result
func errorOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	errObj, isDict := resp["error"].(map[string]any)
	if !isDict {
		t.Fatalf("no error in %v", resp)
	}
	return errObj
}

func TestStaleModelOptionsRefusesInsteadOfBuilding(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pinCodeSkew(t, "abc1234567", "def4567890", true)
	builds := 0
	pinModelOptionsBuilder(t, func(context.Context, provider.PickerConfigContext, bool, bool, bool) (map[string]any, error) {
		builds++
		return nil, nil
	})
	errObj := errorOf(t, callMethod(t, "model.options", map[string]any{}))
	msg, _ := errObj["message"].(string)
	if errObj["code"] != 5098 || !strings.Contains(msg, "abc1234567") || !strings.Contains(msg, "def4567890") || !strings.Contains(strings.ToLower(msg), "restart") {
		t.Fatalf("error %v", errObj)
	}
	if builds != 0 {
		t.Fatalf("payload built %d times", builds)
	}
}

func TestStaleModelSaveKeyRefusesInsteadOfWriting(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	pinCodeSkew(t, "abc1234567", "def4567890", true)
	errObj := errorOf(t, callMethod(t, "model.save_key", map[string]any{"slug": "zai", "api_key": "sk-canary"}))
	if errObj["code"] != 5098 {
		t.Fatalf("error %v", errObj)
	}
	if raw, err := os.ReadFile(filepath.Join(h, ".env")); err == nil && strings.Contains(string(raw), "sk-canary") {
		t.Fatal("the stale process wrote the key")
	}
}

func TestFreshModelOptionsBuildsPayloadUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pinCodeSkew(t, "", "", false)
	expected := map[string]any{"providers": []any{}}
	pinModelOptionsBuilder(t, func(context.Context, provider.PickerConfigContext, bool, bool, bool) (map[string]any, error) {
		return expected, nil
	})
	if got := resultOf(t, callMethod(t, "model.options", map[string]any{})); !reflect.DeepEqual(got, expected) {
		t.Fatalf("result %v", got)
	}
}

func TestModelOptionsBindsRequestedProfileHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	profileDir := filepath.Join(root, "profiles", "fred-work")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	setLaunchHome(t, root)
	pinCodeSkew(t, "", "", false)
	seen := ""
	pinModelOptionsBuilder(t, func(ctx context.Context, _ provider.PickerConfigContext, _, _, _ bool) (map[string]any, error) {
		seen = home.Dir(ctx)
		return map[string]any{"providers": []any{}}, nil
	})
	got := resultOf(t, callMethod(t, "model.options", map[string]any{"profile": "fred-work"}))
	if !reflect.DeepEqual(got, map[string]any{"providers": []any{}}) {
		t.Fatalf("result %v", got)
	}
	if resolveLax(seen) != resolveLax(profileDir) {
		t.Fatalf("payload built under %q, want %q", seen, profileDir)
	}
}
