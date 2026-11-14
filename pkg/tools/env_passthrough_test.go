package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func passthroughEnv(t *testing.T, cfg config.Tree) *Env {
	t.Helper()
	ClearEnvPassthrough()
	ResetEnvPassthroughConfigCache()
	BundledPlatformSecretEnvs = func() []string { return []string{"TEST_PLATFORM_TOKEN"} }
	ConfigEnvTableKeys = func() []string { return []string{"TEST_GATEWAY_SECRET", "TEST_GATEWAY_URL"} }
	RegisteredAdapterSecretEnv = func(env *Env) map[string]bool { return map[string]bool{} }
	t.Cleanup(func() {
		ClearEnvPassthrough()
		ResetEnvPassthroughConfigCache()
	})
	if cfg == nil {
		cfg = config.Defaults()
	}
	return &Env{Home: t.TempDir(), Config: cfg}
}

func TestPassthroughSkipsEmpty(t *testing.T) {
	env := passthroughEnv(t, nil)
	RegisterEnvPassthrough(env, []string{"", "  ", "VALID_KEY"})
	if !IsEnvPassthrough(env, "VALID_KEY") || IsEnvPassthrough(env, "") {
		t.Fatal("registration")
	}
}

func TestConfigPassthrough(t *testing.T) {
	env := passthroughEnv(t, config.Tree{"terminal": map[string]any{"env_passthrough": []any{"MY_CUSTOM_KEY", "ANOTHER_TOKEN"}}})
	if !IsEnvPassthrough(env, "MY_CUSTOM_KEY") || !IsEnvPassthrough(env, "ANOTHER_TOKEN") || IsEnvPassthrough(env, "UNRELATED_VAR") {
		t.Fatal("config allowlist")
	}
	env = passthroughEnv(t, config.Tree{"terminal": map[string]any{"env_passthrough": []any{"CONFIG_KEY"}}})
	RegisterEnvPassthrough(env, []string{"SKILL_KEY"})
	all := AllPassthrough(env)
	if !all["CONFIG_KEY"] || !all["SKILL_KEY"] {
		t.Fatalf("union %v", all)
	}
}

func TestProfileScopedResolution(t *testing.T) {
	env := passthroughEnv(t, nil)
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	secretsPath := filepath.Join(env.Home, ".env")
	os.WriteFile(secretsPath, []byte("SERVICE_TOKEN=profile-b\n"), 0o600)
	env.Secrets = config.NewSecrets(env.Home)
	fallback := "profile-a"
	got, err := ResolvePassthroughValue(env, "SERVICE_TOKEN", &fallback)
	if err != nil || got == nil || *got != "profile-b" {
		t.Fatalf("scope override: %v %v", got, err)
	}
	got, err = ResolvePassthroughValue(env, "OTHER_TOKEN", &fallback)
	if err != nil || got != nil {
		t.Fatalf("missing scoped read fell back: %v %v", got, err)
	}
	env.Secrets = nil
	if _, err := ResolvePassthroughValue(env, "SERVICE_TOKEN", &fallback); err != ErrUnscopedSecret {
		t.Fatalf("unscoped multiplex read did not fail closed: %v", err)
	}
	SetMultiplexActive(false)
	single := passthroughEnv(t, nil)
	got, err = ResolvePassthroughValue(single, "SERVICE_TOKEN", &fallback)
	if err != nil || got == nil || *got != "profile-a" {
		t.Fatal("single profile fallback")
	}
	SetMultiplexActive(true)
	env.Secrets = config.NewSecrets(env.Home)
	explicit := "/explicit"
	got, _ = ResolvePassthroughValue(env, "TERMINAL_CWD", &explicit)
	if got == nil || *got != "/explicit" {
		t.Fatal("global override")
	}
}

func TestAdapterSecretEnvKeepsOnlyCoreDeclaredMessagingSecrets(t *testing.T) {
	passthroughEnv(t, nil)
	config.OptionalEnvVars.Set("CHATX_INJECTED_SIGNING_SECRET", config.EnvVar{Password: true, Category: "messaging"})
	t.Cleanup(func() { config.OptionalEnvVars.Delete("CHATX_INJECTED_SIGNING_SECRET") })
	names := buildAdapterSecretEnv()
	if names["CHATX_INJECTED_SIGNING_SECRET"] {
		t.Fatal("a manifest injected messaging secret joined the process wide adapter secrets")
	}
	core, err := config.CoreDeclaredEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range config.OptionalEnvVars.Items() {
		if core[item.Name] && item.Entry.Category == "messaging" && item.Entry.Password {
			found = true
			if !names[strings.ToUpper(item.Name)] {
				t.Fatalf("core messaging secret %s missing", item.Name)
			}
		}
	}
	if !found {
		t.Fatal("no core messaging secret declared")
	}
}

func TestPassthroughCannotOverrideProviderBlocklist(t *testing.T) {
	env := passthroughEnv(t, nil)
	blocked := "OPENAI_API_KEY"
	if !ProviderEnvBlocklist()[blocked] || !ProviderEnvBlocklist()["TEST_PLATFORM_TOKEN"] || !ProviderEnvBlocklist()["TEST_GATEWAY_SECRET"] || ProviderEnvBlocklist()["TEST_GATEWAY_URL"] {
		t.Fatalf("blocklist composition %v", ProviderEnvBlocklist())
	}
	RegisterEnvPassthrough(env, []string{blocked})
	if IsEnvPassthrough(env, blocked) {
		t.Fatal("provider credential registered")
	}
	for _, v := range []string{"openai_api_key", "OpenAi_Api_Key", "anthropic_api_key", "Aws_Bearer_Token_Bedrock"} {
		RegisterEnvPassthrough(env, []string{v})
		if IsEnvPassthrough(env, v) {
			t.Fatalf("%s accepted", v)
		}
	}
	env = passthroughEnv(t, config.Tree{"terminal": map[string]any{"env_passthrough": []any{"openai_api_key", "MY_OWN_KEY"}}})
	if IsEnvPassthrough(env, "openai_api_key") || !IsEnvPassthrough(env, "MY_OWN_KEY") {
		t.Fatal("config variant")
	}
	RegisterEnvPassthrough(env, []string{"AUXILIARY_VISION_API_KEY", "AUXILIARY_VISION_MODEL", "GATEWAY_RELAY_X_SECRET", "GATEWAY_RELAY_URL", "TENOR_API_KEY"})
	if IsEnvPassthrough(env, "AUXILIARY_VISION_API_KEY") || IsEnvPassthrough(env, "GATEWAY_RELAY_X_SECRET") {
		t.Fatal("internal dynamic secret accepted")
	}
	if !IsEnvPassthrough(env, "AUXILIARY_VISION_MODEL") || !IsEnvPassthrough(env, "GATEWAY_RELAY_URL") || !IsEnvPassthrough(env, "TENOR_API_KEY") {
		t.Fatal("non secret routing or third party key refused")
	}
	os.Setenv("ICEBATH_MULTIPLEX_ACTIVE", "")
	additions := ScopedPassthroughAdditions(env, map[string]bool{})
	if len(additions) != 0 {
		t.Fatal("additions without a scope")
	}
}

func TestMissingRoutedSecretUnderMultiplexReadsProfileFileOnly(t *testing.T) {
	env := passthroughEnv(t, nil)
	RegisterEnvPassthrough(env, []string{"SERVICE_TOKEN"})
	t.Setenv("SERVICE_TOKEN", "token-for-default")
	os.WriteFile(filepath.Join(env.Home, ".env"), []byte("OTHER=1\n"), 0o600)
	env.Secrets = config.NewSecrets(env.Home)
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	fallback := "token-for-default"
	got, err := ResolvePassthroughValue(env, "SERVICE_TOKEN", &fallback)
	if err != nil || got != nil {
		t.Fatalf("missing routed secret leaked %v %v", got, err)
	}
	if adds := ScopedPassthroughAdditions(env, map[string]bool{}); len(adds) != 0 {
		t.Fatalf("scoped additions leaked the process value %v", adds)
	}
	os.WriteFile(filepath.Join(env.Home, ".env"), []byte("SERVICE_TOKEN=routed\n"), 0o600)
	if adds := ScopedPassthroughAdditions(env, map[string]bool{}); adds["SERVICE_TOKEN"] != "routed" {
		t.Fatalf("scoped additions %v", adds)
	}
	if adds := ScopedPassthroughAdditions(env, map[string]bool{"SERVICE_TOKEN": true}); len(adds) != 0 {
		t.Fatalf("present name added %v", adds)
	}
	env.Secrets = nil
	got, err = ResolvePassthroughValue(env, "PATH", nil)
	if err != nil || got == nil || *got != os.Getenv("PATH") {
		t.Fatalf("unscoped global read %v %v", got, err)
	}
}

func TestSingleProfileScopeFallsBackToProcessEnvThenCallerValue(t *testing.T) {
	env := passthroughEnv(t, nil)
	t.Setenv("ICEBATH_HOME", env.Home)
	t.Setenv("SERVICE_TOKEN", "from-process")
	os.WriteFile(filepath.Join(env.Home, ".env"), []byte("OTHER=1\n"), 0o600)
	env.Secrets = config.NewSecrets(env.Home)
	fallback := "from-caller"
	got, err := ResolvePassthroughValue(env, "SERVICE_TOKEN", &fallback)
	if err != nil || got == nil || *got != "from-process" {
		t.Fatalf("own profile miss did not read the process env %v %v", got, err)
	}
	got, _ = ResolvePassthroughValue(env, "UNSET_TOKEN_XYZ", &fallback)
	if got == nil || *got != "from-caller" {
		t.Fatalf("own profile miss lost the caller value %v", got)
	}
	routed := passthroughEnv(t, nil)
	routed.Secrets = config.NewSecrets(routed.Home)
	got, _ = ResolvePassthroughValue(routed, "SERVICE_TOKEN", &fallback)
	if got == nil || *got != "from-caller" {
		t.Fatalf("routed profile miss read the launch process env %v", got)
	}
	routed.Secrets = nil
	got, err = ResolvePassthroughValue(routed, "SERVICE_TOKEN", &fallback)
	if err != nil || got == nil || *got != "from-caller" {
		t.Fatalf("unscoped single profile fallback %v %v", got, err)
	}
}
