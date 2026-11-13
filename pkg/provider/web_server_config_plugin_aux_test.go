package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Writes a directory plugin whose register makes the one given context call
func assignWritePlugin(t *testing.T, h, name, call string) {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(`{"name": "`+name+`", "version": "0.1.0", "description": "`+name+` probe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("def register(ctx):\n    ctx."+call+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Home holding the given plugins enabled and its own compression model
func assignWritePluginHome(t *testing.T, h string, plugins map[string]string, compressionModel string) {
	t.Helper()
	var enabled []string
	for name, call := range plugins {
		assignWritePlugin(t, h, name, call)
		enabled = append(enabled, `"`+name+`"`)
	}
	doc := `{"plugins": {"isolation": "host", "enabled": [` + strings.Join(enabled, ", ") + `]}, ` +
		`"auxiliary": {"compression": {"provider": "openrouter", "model": "` + compressionModel + `"}}}`
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	assignSidePlugin  = "register_auxiliary_task('side_task', display_name='Side model', description='side model for side', inherit_from='compression')"
	assignAlphaPlugin = "register_auxiliary_task('alpha_task', display_name='Alpha side model', description='side model for alpha_plugin', defaults={'timeout': 7})"
)

func TestConfigProfileScopeBindsTheNamedProfileHomeAndSecrets(t *testing.T) {
	resetLaunchPolicy(t)
	root := t.TempDir()
	process := filepath.Join(root, ".icebath")
	named := filepath.Join(process, "profiles", "b")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{process, named} {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(named, ".env"), []byte("B_ONLY_KEY=from-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, process)
	ctx := context.Background()
	for _, current := range []string{"", "  ", "current", "CURRENT"} {
		sctx, scoped, err := ConfigProfileScope(ctx, current)
		if err != nil || scoped != "" || home.Override(sctx) != "" || MultiplexProcess() {
			t.Fatalf("%q: scoped %q override %q err %v", current, scoped, home.Override(sctx), err)
		}
		if scope := SecretScopeFrom(sctx); scope == nil || scope.ProfileHome != process {
			t.Fatalf("%q: secret scope %+v", current, scope)
		}
	}
	sctx, scoped, err := ConfigProfileScope(ctx, " b ")
	if err != nil || scoped != named || home.Dir(sctx) != named {
		t.Fatalf("named: scoped %q dir %q err %v", scoped, home.Dir(sctx), err)
	}
	if !MultiplexProcess() {
		t.Fatal("serving a second profile must activate multi profile hosting")
	}
	if scope := SecretScopeFrom(sctx); scope == nil || scope.ProfileHome != named || scope.Mapping["B_ONLY_KEY"] != "from-b" {
		t.Fatalf("named secret scope %+v", scope)
	}
	nested, scoped, err := ConfigProfileScope(sctx, "")
	if err != nil || scoped != named || home.Dir(nested) != named {
		t.Fatalf("nested current: scoped %q dir %q err %v", scoped, home.Dir(nested), err)
	}
	if _, _, err := ConfigProfileScope(ctx, "Not Valid!"); lmStatusOf(err) != 400 {
		t.Fatalf("invalid name %v", err)
	}
	_, _, err = ConfigProfileScope(ctx, "missing")
	if lmStatusOf(err) != 404 || err.(*HTTPException).Detail != "Profile 'missing' does not exist." {
		t.Fatalf("missing profile %v", err)
	}
}

// Auxiliary listing rows of a profile keyed by task
func assignRows(t *testing.T, profile string) map[string]map[string]any {
	t.Helper()
	listing, err := GetAuxiliaryModels(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, row := range listing["tasks"].([]any) {
		rows[row.(map[string]any)["task"].(string)] = row.(map[string]any)
	}
	return rows
}

// Model the side task resolves to under a profile
func assignResolvedModel(t *testing.T, profile string) any {
	t.Helper()
	sctx, _, err := ConfigProfileScope(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := auxGetAuxiliaryTaskConfig(sctx, "side_task")
	if err != nil {
		t.Fatal(err)
	}
	return resolved["model"]
}

// Applies one auxiliary assignment inside the named profile scope
func assignInProfile(t *testing.T, profile, provider, model, task string) {
	t.Helper()
	sctx, _, err := ConfigProfileScope(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyModelAssignmentSync(sctx, "auxiliary", provider, model, task, "", "", AuxReasoningEffort{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPFailureMapsConfigRefusalsAndHidesTheRest(t *testing.T) {
	detail := "Failed to read auxiliary config"
	cases := []struct {
		err    error
		status int
		text   string
	}{
		{httpException(404, "Profile 'x' does not exist."), 404, "Profile 'x' does not exist."},
		{config.NewConfigLockedError("model.default", "group", ""), 403, "model.default is locked by the group level"},
		{config.NewConfigValueError("too big", "config_body_too_large"), 413, "too big"},
		{config.NewConfigValueError("bad value", ""), 400, "bad value"},
		{config.NewConfigWriteError("raced", "config_version_conflict"), 409, "raced"},
		{config.NewConfigWriteError("disk full", ""), 500, detail},
		{errors.New("boom"), 500, detail},
	}
	for _, c := range cases {
		got, ok := httpFailure(c.err, "GET /api/model/auxiliary failed", 500, "", &detail).(*HTTPException)
		if !ok || got.StatusCode != c.status || got.Detail != c.text {
			t.Fatalf("%v -> %v", c.err, got)
		}
	}
	if got := httpFailure(errors.New("boom"), "x failed", 502, "Could not read", nil).(*HTTPException); got.Detail != "Could not read: boom" {
		t.Fatalf("prefix %v", got)
	}
}
