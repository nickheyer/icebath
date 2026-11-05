package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	registryColdRoleEnv      = "ICEBATH_REGISTRY_COLD_ROLE"
	registryColdResultPrefix = "REGISTRY_COLD_RESULT "
	registryColdBackendKind  = "registrycoldrefuse"
)

// Config backend that records and refuses every read, the double for a backend nothing may reach
type refusingConfigBackend struct {
	config.FileBackend
	log *configReadLog
}

type configReadLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *configReadLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.calls...)
}

func (b refusingConfigBackend) refuse(what string) error {
	b.log.mu.Lock()
	b.log.calls = append(b.log.calls, what)
	b.log.mu.Unlock()
	return &config.ConfigBackendUnavailable{Message: "this backend refuses every read"}
}

func (b refusingConfigBackend) Name() string { return registryColdBackendKind }
func (b refusingConfigBackend) ReadUserDoc(_ context.Context, h string) (any, error) {
	return nil, b.refuse("ReadUserDoc " + h)
}
func (b refusingConfigBackend) ReadUserDocReadonly(_ context.Context, h string) (any, error) {
	return nil, b.refuse("ReadUserDocReadonly " + h)
}
func (b refusingConfigBackend) Version(_ context.Context, h string) ([]any, error) {
	return nil, b.refuse("Version " + h)
}
func (b refusingConfigBackend) CacheVersion(_ context.Context, h string, _ []any) ([]any, error) {
	return nil, b.refuse("CacheVersion " + h)
}
func (b refusingConfigBackend) Exists(_ context.Context, h string) (bool, error) {
	return false, b.refuse("Exists " + h)
}
func (b refusingConfigBackend) ProbeReadable(_ context.Context, h string) error {
	return b.refuse("ProbeReadable " + h)
}

// Selects the refusing backend for the rest of the helper process and returns its read log
func refuseConfigReads(t *testing.T) *configReadLog {
	t.Helper()
	log := &configReadLog{}
	config.RegisterConfigBackend(registryColdBackendKind, func() config.ConfigBackend { return refusingConfigBackend{log: log} })
	t.Setenv(config.BackendEnv, registryColdBackendKind)
	config.ResetCache()
	return log
}

// Runs one role of the cold process helper in a fresh test binary and returns the JSON it printed
func runRegistryColdProcess(t *testing.T, h, role string) map[string]any {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryColdProcessHelper$", "-test.v=false")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, config.BackendEnv+"=") && !strings.HasPrefix(kv, "ICEBATH_HOME=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, registryColdRoleEnv+"="+role, "ICEBATH_HOME="+h)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cold %s process failed: %v\n%s\n%s", role, err, out, stderr.String())
	}
	var result map[string]any
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), registryColdResultPrefix); ok {
			if err := json.Unmarshal([]byte(line), &result); err != nil {
				t.Fatalf("cold %s result: %v", role, err)
			}
		}
	}
	if result == nil {
		t.Fatalf("cold %s process printed no result\n%s", role, out)
	}
	return result
}

// Entry point of the cold process roles, idle unless the parent test started this binary with a role
func TestRegistryColdProcessHelper(t *testing.T) {
	role := os.Getenv(registryColdRoleEnv)
	if role == "" {
		return
	}
	if regDiscovered.Load() || authRegistryReady.Load() {
		t.Fatal("the helper process must start before discovery and before the auth registry exists")
	}
	var result map[string]any
	switch role {
	case "discovery":
		result = coldDiscoveryCompletion(t)
	case "safe", "ordinary":
		result = coldSafeWorkerCatalog(t, role)
	default:
		t.Fatalf("unknown cold role %q", role)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(registryColdResultPrefix + string(raw))
}

// Discovery under a refusing backend with an entry point registration and an unscanned home plugin
func coldDiscoveryCompletion(t *testing.T) map[string]any {
	ctx := context.Background()
	loads := fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"homeplug": func(string) ([]Profile, error) {
			return []Profile{{Name: "homeplug", AuthType: AuthTypeAPIKey, KeyEnv: []string{"HOMEPLUG_API_KEY"}}}, nil
		},
	})
	reads := refuseConfigReads(t)
	FillProviderEnvVars()
	runDiscoveryStepsFn = func(context.Context) {
		RegisterProvider(Profile{Name: "epplug", AuthType: AuthTypeAPIKey, KeyEnv: []string{"EPPLUG_API_KEY"}})
	}
	discoverProviders(ctx)
	_, ep := config.OptionalEnvVars["EPPLUG_API_KEY"]
	_, homeVar := config.OptionalEnvVars["HOMEPLUG_API_KEY"]
	return map[string]any{"reads": reads.list(), "ep": ep, "home": homeVar, "scanned": append([]string{}, *loads...)}
}

// Env catalog of a worker bound safe or ordinary, with the names only bundled plugin profiles declare
func coldSafeWorkerCatalog(t *testing.T, role string) map[string]any {
	var err error
	if role == "safe" {
		_, err = config.BindSafeWorkerPolicy(true, true, map[string]any{})
	} else {
		_, err = config.BindWorkerConfig(map[string]any{})
	}
	if err != nil {
		t.Fatal(err)
	}
	FillProviderEnvVars()
	static := staticDeclaredEnvNames()
	declared := []string{}
	for _, p := range bundledSource() {
		if p.AuthType != AuthTypeAPIKey {
			continue
		}
		for _, v := range profileEnvVars(p) {
			if !static[v] && !slices.Contains(declared, v) {
				declared = append(declared, v)
			}
		}
	}
	present := []string{}
	for _, v := range declared {
		if _, ok := config.OptionalEnvVars[v]; ok {
			present = append(present, v)
		}
	}
	return map[string]any{"bundled": len(BundledProfiles()), "declared": declared, "present": present}
}

func TestColdDiscoveryCompletionReadsNoConfigAndScansNoHomePlugins(t *testing.T) {
	h := t.TempDir()
	writeProviderPluginDir(t, h, "homeplug")
	out := runRegistryColdProcess(t, h, "discovery")
	if reads, _ := out["reads"].([]any); len(reads) != 0 {
		t.Fatalf("discovery completion read config: %v", reads)
	}
	if out["ep"] != true || out["home"] != false {
		t.Fatalf("entry point var published %v, home plugin var published %v", out["ep"], out["home"])
	}
	if scanned, _ := out["scanned"].([]any); len(scanned) != 0 {
		t.Fatalf("home plugins were scanned: %v", scanned)
	}
}

func TestColdSafeWorkerCatalogExecutesNoPluginCode(t *testing.T) {
	for _, mode := range []string{"safe", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			out := runRegistryColdProcess(t, t.TempDir(), mode)
			declared, _ := out["declared"].([]any)
			present, _ := out["present"].([]any)
			if len(declared) == 0 {
				t.Fatal("no env name is declared only by bundled plugin profiles")
			}
			if mode == "safe" {
				if out["bundled"] != float64(0) || len(present) != 0 {
					t.Fatalf("safe worker loaded %v bundled profiles and published %v", out["bundled"], present)
				}
				return
			}
			if out["bundled"] == float64(0) || len(present) != len(declared) {
				t.Fatalf("ordinary worker loaded %v bundled profiles and published %v of %v", out["bundled"], present, declared)
			}
		})
	}
}

func TestBundledProfilesMatchDiscoveryAndLeaveTheRegistryAlone(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	identity := func() map[string]*Profile {
		regMu.Lock()
		defer regMu.Unlock()
		out := make(map[string]*Profile, len(regProfiles))
		for k, v := range regProfiles {
			out[k] = v
		}
		return out
	}
	before, gen := identity(), RegistryGeneration()
	bundled := map[string]Profile{}
	for _, p := range BundledProfiles() {
		bundled[p.Name] = p
	}
	after := identity()
	if len(after) != len(before) || RegistryGeneration() != gen {
		t.Fatal("collecting bundled profiles changed the registry")
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("collecting bundled profiles replaced %s", k)
		}
	}
	var discovered []string
	for name := range before {
		if ProviderSource(ctx, name) == "bundled" {
			discovered = append(discovered, name)
		}
	}
	if len(discovered) == 0 {
		t.Fatal("discovery registered no bundled profile")
	}
	for _, name := range discovered {
		b, ok := bundled[name]
		if !ok {
			t.Fatalf("discovered bundled %s is missing from the bundled catalog", name)
		}
		if !reflect.DeepEqual(profileEnvVars(b), profileEnvVars(*before[name])) {
			t.Fatalf("%s env vars %v, registry %v", name, profileEnvVars(b), profileEnvVars(*before[name]))
		}
	}
}

func TestLateRegisteredProviderReachesPickerCatalog(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	SyncPluginProviderCatalog()
	catalogMu.Lock()
	providers, slugs, labels := append([]CanonicalProviderEntry(nil), canonicalProviders...), canonicalSlugs, providerLabels
	canonicalSlugs, providerLabels = map[string]bool{}, map[string]string{}
	for k, v := range slugs {
		canonicalSlugs[k] = v
	}
	for k, v := range labels {
		providerLabels[k] = v
	}
	catalogMu.Unlock()
	t.Cleanup(func() {
		catalogMu.Lock()
		canonicalProviders, canonicalSlugs, providerLabels = providers, slugs, labels
		catalogMu.Unlock()
	})
	const slug = "zz-late-plugin-provider"
	listed := func() bool {
		for _, r := range ListAvailableProviders(ctx) {
			if r.ID == slug {
				return true
			}
		}
		return false
	}
	if listed() {
		t.Fatal("the late provider is listed before it registers")
	}
	RegisterProvider(Profile{Name: slug, DisplayName: slug, Description: "late plugin (direct API)"})
	if !listed() {
		t.Fatal("a post discovery registration must reach the picker catalog")
	}
	catalogMu.Lock()
	label := providerLabels[slug]
	catalogMu.Unlock()
	if label != slug {
		t.Fatalf("label = %q", label)
	}
	if added := SyncPluginProviderCatalog(); added != 0 {
		t.Fatalf("a second sync added %d", added)
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	count := 0
	for _, p := range canonicalProviders {
		if p.Slug == slug {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("late provider listed %d times", count)
	}
}

// Runtime base URL and registry row fields of stepfun in a home whose plugin re-registers it
func stepfunOverrideView(t *testing.T, plugin Profile) (runtimeURL string, row Profile, gmiURL string) {
	t.Helper()
	snapshotProviderRegistry(t)
	h := t.TempDir()
	perHomePlugins(t, map[string]map[string]Profile{h: {"stepfun": plugin}})
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("STEPFUN_API_KEY", "sk-fixture")
	t.Setenv("STEPFUN_BASE_URL", "")
	os.Unsetenv("STEPFUN_BASE_URL")
	ctx := home.WithOverride(context.Background(), h)
	ListProviders(ctx)
	r := registryLookup(ctx, "stepfun")
	g := registryLookup(ctx, "gmi")
	if r == nil || g == nil {
		t.Fatalf("stepfun row %v gmi row %v", r, g)
	}
	route, err := Resolve(ctx, config.Tree(map[string]any{}), config.NewSecrets(h), h, NewStore(h), Overrides{Provider: "stepfun"})
	if err != nil {
		t.Fatal(err)
	}
	return route.BaseURL, *r, g.BaseURL
}

// Built in stepfun and gmi rows as a home without plugins reads them
func builtinStepfunAndGMI(t *testing.T) (stepfun Profile, gmiURL string) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), t.TempDir())
	r, g := registryLookup(ctx, "stepfun"), registryLookup(ctx, "gmi")
	if r == nil || g == nil {
		t.Fatal("built in stepfun or gmi row missing")
	}
	return *r, g.BaseURL
}

func TestUserPluginEndpointAndEnvVarsReachTheRuntime(t *testing.T) {
	runtimeURL, row, _ := stepfunOverrideView(t, Profile{Name: "stepfun", Aliases: []string{"step"}, AuthType: AuthTypeAPIKey,
		KeyEnv: []string{"STEPFUN_API_KEY", "STEPFUN_REGIONAL_BASE_URL"}, BaseURL: "https://api.stepfun.com/step_plan/v1"})
	if runtimeURL != "https://api.stepfun.com/step_plan/v1" {
		t.Fatalf("runtime base url = %q", runtimeURL)
	}
	if row.BaseURLEnv != "STEPFUN_REGIONAL_BASE_URL" || !reflect.DeepEqual(row.KeyEnv, []string{"STEPFUN_API_KEY"}) {
		t.Fatalf("row env = %v %q", row.KeyEnv, row.BaseURLEnv)
	}
}

func TestUserPluginDeclaringNoEndpointKeepsTheBuiltinRow(t *testing.T) {
	builtin, builtinGMI := builtinStepfunAndGMI(t)
	runtimeURL, row, gmiURL := stepfunOverrideView(t, Profile{Name: "stepfun", AuthType: AuthTypeAPIKey,
		KeyEnv: []string{"STEPFUN_API_KEY"}})
	if runtimeURL != builtin.BaseURL {
		t.Fatalf("runtime base url = %q, built in %q", runtimeURL, builtin.BaseURL)
	}
	if row.BaseURLEnv != "STEPFUN_BASE_URL" || gmiURL != builtinGMI {
		t.Fatalf("base url env %q, gmi %q want %q", row.BaseURLEnv, gmiURL, builtinGMI)
	}
}
