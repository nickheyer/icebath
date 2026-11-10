package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
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

// Home a with the side and alpha plugins and profile b with side alone, hosts run by python3
func assignTwoProfileHomes(t *testing.T) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	root := t.TempDir()
	homeA := filepath.Join(root, ".icebath")
	homeB := filepath.Join(homeA, "profiles", "b")
	if err := os.MkdirAll(homeB, 0o755); err != nil {
		t.Fatal(err)
	}
	assignWritePluginHome(t, homeA, map[string]string{"side": assignSidePlugin, "alpha_plugin": assignAlphaPlugin}, "vendor/model-a")
	assignWritePluginHome(t, homeB, map[string]string{"side": assignSidePlugin}, "vendor/model-b")
	t.Cleanup(home.PatchUserHome(root))
	t.Setenv(home.EnvHome, homeA)
	t.Setenv("PYTHONPATH", "")
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(root, "empty-bundled"))
	savedSecrets := tools.ProfileSecrets
	tools.ProfileSecrets = ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = savedSecrets })
	t.Cleanup(plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil }))
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{BuiltinAuxTaskKeys: BuiltinAuxTaskKeys})
	plugins.ResetPluginManagersForTests()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		plugins.InstallServices(previous)
	})
	return homeA, homeB
}

func TestAssignmentResetAndStalePinsCoverPluginTasks(t *testing.T) {
	resetLaunchPolicy(t)
	assignTwoProfileHomes(t)
	saved := map[string]any{}
	savedSave := configSaveFn
	t.Cleanup(func() { configSaveFn = savedSave })
	configSaveFn = func(ctx context.Context, cfg map[string]any) error {
		for k, v := range cfg {
			saved[k] = v
		}
		return nil
	}
	ctx := context.Background()
	cfg := map[string]any{"auxiliary": map[string]any{}}
	aux := func() map[string]any { return cfg["auxiliary"].(map[string]any) }
	out, err := applyAuxAssignmentSync(ctx, cfg, "openrouter", "fast-model", "alpha_task", "", "", AuxReasoningEffort{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["tasks"], []string{"alpha_task"}) {
		t.Fatalf("tasks %v", out["tasks"])
	}
	if !reflect.DeepEqual(aux()["alpha_task"], map[string]any{"provider": "openrouter", "model": "fast-model"}) {
		t.Fatalf("alpha %v", aux()["alpha_task"])
	}
	if saved["auxiliary"].(map[string]any)["alpha_task"].(map[string]any)["model"] != "fast-model" {
		t.Fatalf("saved %v", saved["auxiliary"])
	}
	found := false
	for _, pin := range staleAuxPins(ctx, cfg, "nous") {
		found = found || reflect.DeepEqual(pin, map[string]any{"task": "alpha_task", "provider": "openrouter", "model": "fast-model"})
	}
	if !found {
		t.Fatalf("stale pins %v", staleAuxPins(ctx, cfg, "nous"))
	}
	if _, err := applyAuxAssignmentSync(ctx, cfg, "nous", "hermes-4", "", "", "", AuxReasoningEffort{}); err != nil {
		t.Fatal(err)
	}
	if aux()["alpha_task"].(map[string]any)["provider"] != "nous" {
		t.Fatalf("broadcast %v", aux()["alpha_task"])
	}
	if _, err := applyAuxAssignmentSync(ctx, cfg, "", "", "__reset__", "", "", AuxReasoningEffort{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(aux()["alpha_task"], map[string]any{"provider": "auto", "model": ""}) {
		t.Fatalf("reset %v", aux()["alpha_task"])
	}
	bctx, _, err := ConfigProfileScope(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyAuxAssignmentSync(bctx, map[string]any{"auxiliary": map[string]any{}}, "openrouter", "m", "side_task", "", "", AuxReasoningEffort{}); err != nil {
		t.Fatal(err)
	}
	_, err = applyAuxAssignmentSync(bctx, map[string]any{"auxiliary": map[string]any{}}, "openrouter", "m", "alpha_task", "", "", AuxReasoningEffort{})
	if lmStatusOf(err) != 400 || !strings.Contains(err.(*HTTPException).Detail, "unknown auxiliary task") {
		t.Fatalf("alpha in b %v", err)
	}
}

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

func TestListingIsPerProfileAndCarriesInheritance(t *testing.T) {
	resetLaunchPolicy(t)
	assignTwoProfileHomes(t)
	listing, err := GetAuxiliaryModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	rows := listing["tasks"].([]any)
	for i, slot := range auxTaskSlotsBuiltin {
		if rows[i].(map[string]any)["task"] != slot {
			t.Fatalf("row %d %v", i, rows[i])
		}
	}
	if _, has := rows[0].(map[string]any)["label"]; has {
		t.Fatalf("built in row labelled %v", rows[0])
	}
	if _, has := rows[0].(map[string]any)["plugin"]; has {
		t.Fatalf("built in row names a plugin %v", rows[0])
	}
	rowsA, rowsB := assignRows(t, ""), assignRows(t, "b")
	if _, has := rowsA["alpha_task"]; !has {
		t.Fatalf("alpha missing in a %v", rowsA)
	}
	if _, has := rowsB["alpha_task"]; has {
		t.Fatalf("alpha leaked into b %v", rowsB["alpha_task"])
	}
	sideA, sideB := rowsA["side_task"], rowsB["side_task"]
	if sideA["label"] != "Side model" || sideA["hint"] != "side model for side" || sideA["plugin"] != "side" {
		t.Fatalf("side row %v", sideA)
	}
	if sideA["inherit_from"] != "compression" || rowsA["alpha_task"]["inherit_from"] != nil {
		t.Fatalf("inheritance %v %v", sideA["inherit_from"], rowsA["alpha_task"]["inherit_from"])
	}
	if sideA["provider"] != "auto" {
		t.Fatalf("side provider %v", sideA["provider"])
	}
	if !reflect.DeepEqual(sideA["effective"], map[string]any{"provider": "openrouter", "model": "vendor/model-a", "base_url": ""}) {
		t.Fatalf("side effective %v", sideA["effective"])
	}
	if sideB["effective"].(map[string]any)["model"] != "vendor/model-b" {
		t.Fatalf("side effective in b %v", sideB["effective"])
	}
}

func TestPinPersistsPerProfileAndIsHonoredByResolution(t *testing.T) {
	resetLaunchPolicy(t)
	homeA, _ := assignTwoProfileHomes(t)
	assignInProfile(t, "b", "nous", "hermes-4", "side_task")
	if got := assignRows(t, "b")["side_task"]["provider"]; got != "nous" {
		t.Fatalf("b pin %v", got)
	}
	raw, err := os.ReadFile(filepath.Join(homeA, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "side_task") {
		t.Fatalf("pin leaked into a %s", raw)
	}
	if b, a := assignResolvedModel(t, "b"), assignResolvedModel(t, ""); b != "hermes-4" || a != "vendor/model-a" {
		t.Fatalf("resolution %v %v", b, a)
	}
	assignInProfile(t, "b", "auto", "", "side_task")
	if b := assignResolvedModel(t, "b"); b != "vendor/model-b" {
		t.Fatalf("follow base %v", b)
	}
	assignInProfile(t, "b", "openrouter", "vendor/model-b2", "compression")
	if b, a := assignResolvedModel(t, "b"), assignResolvedModel(t, ""); b != "vendor/model-b2" || a != "vendor/model-a" {
		t.Fatalf("base change %v %v", b, a)
	}
}

func TestPluginDiscoveryFailureLeavesBuiltinsWorking(t *testing.T) {
	resetLaunchPolicy(t)
	assignTwoProfileHomes(t)
	saved := pluginAuxiliaryTasksFn
	t.Cleanup(func() { pluginAuxiliaryTasksFn = saved })
	pluginAuxiliaryTasksFn = func(context.Context) []map[string]any { panic(errors.New("plugin scan exploded")) }
	if got := auxTaskSlots(context.Background()); !reflect.DeepEqual(got, auxTaskSlotsBuiltin) {
		t.Fatalf("slots %v", got)
	}
	listing, err := GetAuxiliaryModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for _, row := range listing["tasks"].([]any) {
		tasks = append(tasks, row.(map[string]any)["task"].(string))
	}
	if !reflect.DeepEqual(tasks, auxTaskSlotsBuiltin) {
		t.Fatalf("tasks %v", tasks)
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
