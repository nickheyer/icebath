package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Checkout carrying the reference pyproject under icebath names, gates reread from it until the test ends
func svRealPyproject(t *testing.T) string {
	t.Helper()
	text := string(readFile(t, filepath.Join(peReferences(t, "pyproject.toml"), "pyproject.toml")))
	text = strings.ReplaceAll(strings.ReplaceAll(text, "[tool.hermes", "[tool.icebath"), "hermes-agent", "icebath-agent")
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "pyproject.toml"), []byte(text), 0o666)
	peSwap(t, &repoRoot, func() string { return repo })
	svGates(t, nil)
	return repo
}

// Replaces the cached gate table until the test ends, nil rereading the checkout pyproject
func svGates(t *testing.T, gates map[string]string) {
	t.Helper()
	platformGatesCache.Lock()
	saved := platformGatesCache.gates
	platformGatesCache.gates = gates
	platformGatesCache.Unlock()
	t.Cleanup(func() {
		platformGatesCache.Lock()
		platformGatesCache.gates = saved
		platformGatesCache.Unlock()
	})
}

// Full marker environment of the host interpreter, what packaging's default environment reports
func svHostMarkers(t *testing.T) map[string]string {
	t.Helper()
	env, err := targetEnvironment(context.Background(), peHostPython(t))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Points the core tree's venv interpreter at the host Python, whose marker environment gates fall back on
func svHostInterpreter(t *testing.T, repo string) map[string]string {
	t.Helper()
	peSwap(t, &repoRoot, func() string { return repo })
	python := VenvPython(filepath.Join(repo, "venv"))
	sttMkdir(t, filepath.Dir(python))
	if err := os.Symlink(peHostPython(t), python); err != nil {
		t.Fatal(err)
	}
	return svHostMarkers(t)
}

// Anchors importable exactly when listed, the sys.modules fakes of the Python tests
func svImportable(t *testing.T, present map[string]bool) {
	t.Helper()
	peSwap(t, &anchorImportable, func(_ context.Context, anchor string) bool { return present[anchor] })
}

// Records every client sync's extras instead of syncing
func svSynced(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	peSwap(t, &clientSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		*calls = append(*calls, append([]string{}, opts.Extras...))
		return nil
	})
	return calls
}

func TestSVExtraSupportedUngatedExtraIsTrue(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	if ok, err := extraSupported(context.Background(), "no-such-gate-for-this-one", nil, nil); err != nil || !ok {
		t.Fatalf("ungated extra gave %v %v", ok, err)
	}
}

func TestSVEnsureImportRaisesOnGatedOffExtra(t *testing.T) {
	tmp := isolateHome(t)
	defaults := svHostInterpreter(t, sttMkdir(t, filepath.Join(tmp, "repo")))
	svGates(t, map[string]string{"gated-extra": "python_full_version < '" + defaults["python_full_version"] + "'"})
	svImportable(t, nil)
	synced := svSynced(t)
	err := EnsureImport(context.Background(), "gated-extra")
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "not supported on this platform") {
		t.Fatalf("gated extra gave %v", err)
	}
	if len(*synced) != 0 {
		t.Fatalf("synced %v", *synced)
	}
}

func TestSVSyncRefusesPythonGatedExtraBeforeTouchingEnvironment(t *testing.T) {
	tmp := isolateHome(t)
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	defaults := svHostInterpreter(t, repo)
	svGates(t, map[string]string{"unavailable-engine": "python_full_version < '" + defaults["python_full_version"] + "'"})
	svImportable(t, map[string]bool{"unavailable_engine": true})
	ctx := context.Background()
	if ok, _ := extraSupported(ctx, "unavailable-engine", nil, nil); !ok {
		t.Fatal("an importable anchor beats the gate")
	}
	if ok, _ := extraSupported(ctx, "unavailable-engine", nil, func(string) bool { return false }); ok {
		t.Fatal("a managed graph cannot borrow the caller's anchors")
	}
	refuse := func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
		t.Fatal("unsupported request reached dependency environment machinery")
		return nil, nil
	}
	pubSwapVenv(t, &pubVenv{prepare: refuse, stamp: func(context.Context, []string, []MemberDir) (string, error) {
		t.Fatal("unsupported request reached dependency environment machinery")
		return "", nil
	}})
	err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"unavailable-engine"}, Explicit: true})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "not supported by this Python/platform") {
		t.Fatalf("gated sync gave %v", err)
	}
	if _, err := os.Stat(filepath.Join(InstallStateDir(ctx, repo), "environments")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused sync touched the environment store: %v", err)
	}
}

func TestSVDeclaredExtraGatesMatchDependencySelection(t *testing.T) {
	isolateHome(t)
	repo := svRealPyproject(t)
	defaults := svHostMarkers(t)
	data, err := loadPyproject(repo)
	if err != nil {
		t.Fatal(err)
	}
	optional := wsAt(data, "project", "optional-dependencies").(*OrderedObject)
	gates := wsAt(data, "tool", "icebath", "extras-platforms").(*OrderedObject)
	targets := [][3]string{{"linux", "Linux", "x86_64"}, {"linux", "Linux", "aarch64"}, {"darwin", "Darwin", "x86_64"},
		{"darwin", "Darwin", "arm64"}, {"win32", "Windows", "AMD64"}, {"win32", "Windows", "ARM64"}, {"android", "Android", "aarch64"}}
	for _, target := range targets {
		for _, python := range []string{"3.12", "3.13", "3.14"} {
			environment := map[string]string{}
			for k, v := range defaults {
				environment[k] = v
			}
			environment["sys_platform"], environment["platform_system"], environment["platform_machine"] = target[0], target[1], target[2]
			environment["python_version"], environment["python_full_version"] = python, python+".0"
			for _, extra := range gates.Keys {
				selected := false
				for _, text := range wsStrings(optional.Values[extra]) {
					req, err := ParseRequirement(text)
					if err != nil {
						t.Fatal(err)
					}
					ok := len(req.Marker) == 0
					if !ok {
						if ok, err = evaluateMarker(req.Marker, nil, environment); err != nil {
							t.Fatal(err)
						}
					}
					selected = selected || ok
				}
				got, err := extraSupported(context.Background(), extra, environment, func(string) bool { return false })
				if err != nil || got != selected {
					t.Fatalf("%s %s %s %s gate %v, dependency selection %v (%v)", extra, target[0], target[2], python, got, selected, err)
				}
			}
		}
	}
}

func TestSVFasterWhisperTargetsAreGated(t *testing.T) {
	isolateHome(t)
	svRealPyproject(t)
	targets := map[string]map[string]string{
		"win32-arm64": {"sys_platform": "win32", "platform_system": "Windows", "platform_machine": "ARM64", "os_name": "nt"},
		"darwin-x64":  {"sys_platform": "darwin", "platform_system": "Darwin", "platform_machine": "x86_64", "os_name": "posix"},
		"linux-x64":   {"sys_platform": "linux", "platform_system": "Linux", "platform_machine": "x86_64", "os_name": "posix"},
	}
	got := map[string]bool{}
	for name, environment := range targets {
		got[name], _ = extraSupported(context.Background(), "stt-whisper", environment, func(string) bool { return false })
	}
	if want := map[string]bool{"win32-arm64": false, "darwin-x64": false, "linux-x64": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supported %v", got)
	}
}

func TestSVAvailableMissingModule(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	if ok, err := Available(context.Background(), "no-such-extra-anywhere"); ok || err != nil {
		t.Fatalf("missing extra gave %v %v", ok, err)
	}
}

func TestSVAvailableCountsImportedFakes(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, map[string]bool{"microsoft_teams.apps": true})
	if ok, err := Available(context.Background(), "teams"); !ok || err != nil {
		t.Fatalf("teams gave %v %v", ok, err)
	}
}

func TestSVGoogleReadinessRequiresItsOauthImports(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	present := map[string]bool{"googleapiclient": true, "google.auth": true, "google_auth_httplib2": true}
	svImportable(t, present)
	ctx := context.Background()
	if ok, _ := Available(ctx, "google"); ok {
		t.Fatal("google ready without its oauth flow")
	}
	present["google_auth_oauthlib.flow"] = true
	if ok, _ := Available(ctx, "google"); !ok {
		t.Fatal("google not ready with every import")
	}
}

func TestSVAvailableUnknownExtraUsesUnderscoreGuess(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, map[string]bool{"some_new_thing": true})
	if ok, _ := Available(context.Background(), "some-new-thing"); !ok {
		t.Fatal("underscore guess missed")
	}
}

func TestSVEnsureImportNoopWhenAvailable(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, map[string]bool{"fal_client": true})
	synced := svSynced(t)
	if err := EnsureImport(context.Background(), "fal"); err != nil || len(*synced) != 0 {
		t.Fatalf("available extra gave %v, synced %v", err, *synced)
	}
}

func TestSVEnsureImportSyncsWhenMissing(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, nil)
	synced := svSynced(t)
	if err := EnsureImport(context.Background(), "fal"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*synced, [][]string{{"fal"}}) {
		t.Fatalf("synced %v", *synced)
	}
}

func TestSVEnsureImportRespectsTerminalDeclineWithoutInstalling(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, nil)
	synced := svSynced(t)
	peSwap(t, &promptTerminals, func() bool { return true })
	var prompts []string
	peSwap(t, &promptInput, func(text string) (string, error) {
		prompts = append(prompts, text)
		return "n", nil
	})
	err := EnsureImport(context.Background(), "fal")
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined prompt gave %v", err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "'fal' feature") || strings.Contains(prompts[0], "fal_client") {
		t.Fatalf("prompts %q", prompts)
	}
	if len(*synced) != 0 {
		t.Fatalf("synced %v", *synced)
	}
}

func TestSVEnsureImportPropagatesInstallError(t *testing.T) {
	isolateHome(t)
	svGates(t, nil)
	svImportable(t, nil)
	peSwap(t, &clientSyncVenv, func(context.Context, SyncVenvOptions) error {
		return NewInstallError("venv", "lazy installs are disabled", "")
	})
	var ie *InstallError
	if err := EnsureImport(context.Background(), "fal"); !errors.As(err, &ie) {
		t.Fatalf("install error became %v", err)
	}
}

func TestSVEnsureAndBindPreservesTargetOnFailure(t *testing.T) {
	for _, failure := range []string{"", "install", "import"} {
		t.Run("failure="+failure, func(t *testing.T) {
			isolateHome(t)
			svGates(t, nil)
			svImportable(t, map[string]bool{"fal_client": failure != "install"})
			peSwap(t, &clientSyncVenv, func(context.Context, SyncVenvOptions) error {
				return NewInstallError("venv", "install refused", "")
			})
			importer := func() (map[string]any, error) {
				if failure == "import" {
					return nil, &PyError{Class: "ImportError", Message: "still broken"}
				}
				return map[string]any{"NAME": 42}, nil
			}
			target := map[string]any{"existing": "kept"}
			ok, err := EnsureAndBind(context.Background(), "fal", importer, target)
			if err != nil || ok != (failure == "") {
				t.Fatalf("bound %v %v", ok, err)
			}
			want := map[string]any{"existing": "kept"}
			if failure == "" {
				want["NAME"] = 42
			}
			if !reflect.DeepEqual(target, want) {
				t.Fatalf("target %v", target)
			}
		})
	}
}

func TestSVEveryAnchorExtraExistsInPyproject(t *testing.T) {
	isolateHome(t)
	repo := svRealPyproject(t)
	data, err := loadPyproject(repo)
	if err != nil {
		t.Fatal(err)
	}
	declared, err := optionalDependencyNames(data)
	if err != nil {
		t.Fatal(err)
	}
	var orphans []string
	for extra := range Anchors {
		if !slices.Contains(declared, extra) {
			orphans = append(orphans, extra)
		}
	}
	if len(orphans) > 0 {
		slices.Sort(orphans)
		t.Fatalf("Anchors names extras pyproject does not declare: %v", orphans)
	}
}

// Points the features file at a temp runtime dir beside an isolated home, returning the temp root
func svRooted(t *testing.T) string {
	t.Helper()
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, sttMkdir(t, filepath.Join(tmp, "tools")))
	svGates(t, nil)
	return tmp
}

func TestSVFeaturesWriteThenReadRoundtrip(t *testing.T) {
	svRooted(t)
	ctx := context.Background()
	if got, err := readFeatures(ctx); got != nil || err != nil {
		t.Fatalf("absent file gave %v %v", got, err)
	}
	path, err := WriteFeatures(ctx, []string{"web", "acp", "web"}, "")
	if err != nil || !isRegularFile(path) {
		t.Fatalf("write gave %q %v", path, err)
	}
	if got, _ := readFeatures(ctx); !reflect.DeepEqual(got, []string{"acp", "web"}) {
		t.Fatalf("features %v", got)
	}
	writeFile(t, path, []byte("{ not json"), 0o666)
	if got, err := readFeatures(ctx); got != nil || err != nil {
		t.Fatalf("invalid file gave %v %v", got, err)
	}
}

func TestSVFeaturesPathInBundleUsesPayloadRoot(t *testing.T) {
	payload := sttMkdir(t, filepath.Join(svRooted(t), "payload"))
	if got, _ := FeaturesPath(context.Background(), payload); got != filepath.Join(payload, "enabled-features.json") {
		t.Fatalf("path %q", got)
	}
}

func TestSVSyncVenvRefusesOutsideFrozenExtras(t *testing.T) {
	svRooted(t)
	ctx := context.Background()
	if _, err := WriteFeatures(ctx, []string{"web", "acp"}, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDisableLazyInstalls, "1")
	err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"slack"}, Explicit: true})
	var ie *InstallError
	if !errors.As(err, &ie) || !(strings.Contains(err.Error(), "frozen") || strings.Contains(err.Error(), "outside")) {
		t.Fatalf("frozen refusal gave %v", err)
	}
	saved := LatestReceipt(ctx)
	refusal, _ := saved["refusal"].(map[string]any)
	if refusal["code"] != "lazy-install" || saved["outcome"] != "failed" || pyrepr.Str(saved["exit_code"]) == "0" {
		t.Fatalf("receipt %v", saved)
	}
	steps, _ := saved["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("receipt without steps %v", saved)
	}
	last := steps[len(steps)-1].(map[string]any)
	detail := pyrepr.Str(last["detail"])
	if last["ok"] != false || !strings.Contains(detail, "slack") || !strings.Contains(detail, "icebath pm install") {
		t.Fatalf("last step %v", last)
	}
}

func TestSVSyncVenvAllowsFrozenExtrasWhenLazyOff(t *testing.T) {
	tmp := svRooted(t)
	ctx := context.Background()
	if _, err := WriteFeatures(ctx, []string{"web"}, ""); err != nil {
		t.Fatal(err)
	}
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	peSwap(t, &repoRoot, func() string { return repo })
	environment := sttMkdir(t, filepath.Join(InstallStateDir(ctx, repo), "environments", "frozen", "venv"))
	writeFile(t, filepath.Join(environment, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, repo), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", "stamp", []string{"web"}, environment, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDisableLazyInstalls, "1")
	pubSwapVenv(t, &pubVenv{
		stamp: func(context.Context, []string, []MemberDir) (string, error) { return "stamp", nil },
		prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
			t.Fatal("current frozen environment rebuilt")
			return nil, nil
		},
	})
	if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(environment, "pyvenv.cfg")); err != nil {
		t.Fatal(err)
	}
	err = syncVenv(ctx, SyncVenvOptions{Extras: []string{"web"}})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "lazy installs are disabled") {
		t.Fatalf("vanished environment gave %v", err)
	}
}

func TestSVLazySyncNeverCreatesTheFirstSelectionForAForeignInterpreter(t *testing.T) {
	tmp := svRooted(t)
	ctx := context.Background()
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	sttMkdir(t, filepath.Join(repo, "venv"))
	peSwap(t, &repoRoot, func() string { return repo })
	built := false
	pubSwapVenv(t, &pubVenv{
		stamp: func(context.Context, []string, []MemberDir) (string, error) { return "stamp", nil },
		prepare: func(ctx context.Context, _ []string, _ VenvApplyOptions) (*VenvGeneration, error) {
			if !built {
				t.Fatal("lazy sync built a generation for a foreign interpreter")
			}
			environment := filepath.Join(InstallStateDir(ctx, repo), "environments", "explicit", "venv")
			if err := os.MkdirAll(environment, 0o777); err != nil {
				return nil, err
			}
			return &VenvGeneration{Environment: environment}, os.WriteFile(filepath.Join(environment, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
		},
	})
	facts := EnvironmentRuntimeFactsPath(ctx, repo)
	if isRegularFile(facts) {
		t.Fatal("facts exist before any sync")
	}
	err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "not running from the install's dependency environment") {
		t.Fatalf("foreign lazy sync gave %v", err)
	}
	if isRegularFile(facts) {
		t.Fatal("a refused sync must not commit a selection")
	}
	built = true
	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(facts) {
		t.Fatal("explicit install created no selection")
	}
}

func TestAnchorImportableExport(t *testing.T) {
	present := map[string]bool{"faster_whisper": true}
	peSwap(t, &anchorImportable, func(_ context.Context, anchor string) bool { return present[anchor] })
	if !AnchorImportable(context.Background(), "faster_whisper") || AnchorImportable(context.Background(), "pilk") {
		t.Fatal("AnchorImportable does not answer the selected environment probe")
	}
}

func TestExtraSupportedExport(t *testing.T) {
	ctx := context.Background()
	got, err := ExtraSupported(ctx, "no-such-extra-for-export-test")
	want, werr := extraSupported(ctx, "no-such-extra-for-export-test", nil, nil)
	if got != want || (err == nil) != (werr == nil) {
		t.Fatalf("ExtraSupported = %v, %v; want %v, %v", got, err, want, werr)
	}
}

func TestExtraSupportedInAndGateSwaps(t *testing.T) {
	ctx := context.Background()
	savedGates := SwapPlatformGates(map[string]string{"gated-export-extra": "sys_platform == 'win32'"})
	savedImportable := SwapAnchorImportable(func(context.Context, string) bool { return false })
	t.Cleanup(func() {
		SwapPlatformGates(savedGates)
		SwapAnchorImportable(savedImportable)
	})
	for platform, want := range map[string]bool{"win32": true, "linux": false} {
		got, err := ExtraSupportedIn(ctx, "gated-export-extra", map[string]string{"sys_platform": platform}, func(string) bool { return false })
		if err != nil || got != want {
			t.Fatalf("%s: ExtraSupportedIn = %v, %v; want %v", platform, got, err, want)
		}
	}
	if got, err := ExtraSupportedIn(ctx, "gated-export-extra", map[string]string{"sys_platform": "linux"}, func(string) bool { return true }); err != nil || !got {
		t.Fatalf("an installed anchor set must beat the gate, got %v, %v", got, err)
	}
	if anchorImportable(ctx, "anything") {
		t.Fatal("swapped importability probe not installed")
	}
}
