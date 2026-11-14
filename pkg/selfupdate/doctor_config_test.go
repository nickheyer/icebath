package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears every credential and cloud variable of the process and points the user home at a sandbox
func doctorHermeticEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_TOKEN") || strings.HasSuffix(name, "_BASE_URL") ||
			strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "AZURE_") || name == "FAL_KEY" || name == "ICEBATH_MAX_ITERATIONS" {
			doctorUnsetEnv(t, name)
		}
	}
	doctorUnsetEnv(t, doctorProxyEnvKeys...)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("HOME", t.TempDir())
}

// Doctor over a fresh home holding exactly the given config.yaml and .env text
func doctorRawHome(t *testing.T, configText, envText string) (*Doctor, *bytes.Buffer) {
	t.Helper()
	doctorHermeticEnv(t)
	d, out := doctorTestDoctor(t, "")
	if configText != "" {
		if err := os.WriteFile(filepath.Join(d.Home, "config.yaml"), []byte(configText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if envText != "" {
		if err := os.WriteFile(filepath.Join(d.Home, ".env"), []byte(envText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config.ResetCache()
	config.InvalidateEnvCache()
	return d, out
}

// Runs the doctor walk over the given checks and returns what it printed with its exit status
func doctorWalk(t *testing.T, d *Doctor, out *bytes.Buffer, ns map[string]any, entries ...doctorCheckEntry) (string, int) {
	t.Helper()
	code, err := runDoctor(context.Background(), d, ns, entries)
	if err != nil {
		t.Fatalf("doctor run failed: %v\n%s", err, out.String())
	}
	return out.String(), code
}

var doctorConfigFileEntry = doctorCheckEntry{"", checkConfigFile}

func TestProviderEnvDetectionDetectsOpenaiApiKey(t *testing.T) {
	if !doctorHasProviderEnvConfig("OPENAI_BASE_URL=http://localhost:1234/v1\nOPENAI_API_KEY=***") {
		t.Fatal("provider settings were not detected")
	}
}

func TestProviderEnvDetectionReturnsFalseWhenNoProviderSettings(t *testing.T) {
	if doctorHasProviderEnvConfig("TERMINAL_ENV=local\n") {
		t.Fatal("provider settings detected in a file without any")
	}
}

func TestDoctorEnvFileEncodingDoctorReadsEnvAsUtf8EvenWhenLocaleIsNotUtf8(t *testing.T) {
	t.Setenv("LANG", "zh_CN.GBK")
	t.Setenv("LC_ALL", "zh_CN.GBK")
	d, out := doctorRawHome(t, "", "OPENAI_API_KEY=sk-test  # em-dash here — should not crash\n")
	if _, err := checkEnvFile(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "API key or custom endpoint configured") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestDoctorEnvFileEncodingDoctorReadsInvalidUtf8EnvViaLatin1Fallback(t *testing.T) {
	d, out := doctorRawHome(t, "", "OPENAI_API_KEY=sk-test\xff\n")
	f, err := checkEnvFile(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "API key or custom endpoint configured") || len(f.Issues) != 0 {
		t.Fatalf("out = %q issues = %q", out.String(), f.Issues)
	}
}

func TestRunDoctorAcceptsNamedProviderFromProvidersSection(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: volcengine-plan\n  default: doubao-seed-2.0-code\n"+
		"providers:\n  volcengine-plan:\n    name: volcengine-plan\n    base_url: https://ark.cn-beijing.volces.com/api/coding/v3\n"+
		"    default_model: doubao-seed-2.0-code\n    models:\n      doubao-seed-2.0-code: {}\n", "")
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	if !strings.Contains(out, "/config.yaml exists") || strings.Contains(out, "model.provider 'volcengine-plan' is not a recognised provider") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunDoctorAcceptsStableKeyWhenProviderNameDiffers(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: custom:local-127.0.0.1:11434\n  default: qwen3.5:9b\n"+
		"providers:\n  local-127.0.0.1:11434:\n    name: Local Ollama\n    base_url: http://127.0.0.1:11434/v1\n    default_model: qwen3.5:9b\n", "")
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	if !strings.Contains(out, "/config.yaml exists") ||
		strings.Contains(out, "model.provider 'custom:local-127.0.0.1:11434' is not a recognised provider") ||
		strings.Contains(out, "model.provider 'custom:local-127.0.0.1:11434' is unknown") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunDoctorAcceptsBareCustomProvider(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: custom\n  default: local-model\n  base_url: http://localhost:8000/v1\n", "")
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	if !strings.Contains(out, "/config.yaml exists") || strings.Contains(out, "model.provider 'custom' is not a recognised provider") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunDoctorFlagsMissingCredentialsForActiveOpenrouterProvider(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: openrouter\n  default: openai/gpt-4.1-mini\n", "")
	out, code := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	if !strings.Contains(out, "model.provider 'openrouter' is set but no API key is configured") || code != 1 {
		t.Fatalf("code = %d out = %q", code, out)
	}
}

func TestRunDoctorAcceptsHermesProviderIdsThatCatalogAliases(t *testing.T) {
	for _, c := range [][2]string{
		{"ai-gateway", "anthropic/claude-sonnet-4.6"}, {"opencode-zen", "anthropic/claude-sonnet-4.6"},
		{"kilocode", "anthropic/claude-sonnet-4.6"}, {"kimi-coding", "kimi-k2"},
		{"nvidia", "qwen/qwen3.5-122b-a10b"}, {"moa", "anthropic/claude-sonnet-4.6"},
	} {
		t.Run(c[0], func(t *testing.T) {
			d, buf := doctorRawHome(t, "model:\n  provider: "+c[0]+"\n  default: "+c[1]+"\n", "")
			out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
			if !strings.Contains(out, "/config.yaml exists") || strings.Contains(out, "model.provider '"+c[0]+"' is not a recognised provider") ||
				strings.Contains(out, "model.provider '"+c[0]+"' is unknown") {
				t.Fatalf("out = %q", out)
			}
			switch c[0] {
			case "ai-gateway", "opencode-zen", "kilocode", "nvidia":
				if strings.Contains(out, "model.default '"+c[1]+"' uses a vendor/model slug but provider is '"+c[0]+"'") {
					t.Fatalf("out = %q", out)
				}
			}
		})
	}
}

func TestRunDoctorAcceptsVendorSlugsForNamedCustomProvider(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: custom:hpc-ai\n  default: deepseek/deepseek-v4-flash\n"+
		"custom_providers:\n  - name: hpc-ai\n    base_url: https://hpc-ai.example/v1\n    api_key: test-key\n", "")
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	for _, unwanted := range []string{
		"model.provider 'custom:hpc-ai' is not a recognised provider", "model.provider 'custom:hpc-ai' is unknown",
		"model.default 'deepseek/deepseek-v4-flash' uses a vendor/model slug but provider is 'custom:hpc-ai'",
		"Either set model.provider to 'openrouter', or drop the vendor prefix.",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("%q in %q", unwanted, out)
		}
	}
	if !strings.Contains(out, "/config.yaml exists") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunDoctorVendorSlugPolicyForOpenaiApiEndpoint(t *testing.T) {
	for baseURL, expectsWarning := range map[string]bool{"http://localhost:20128/v1": false, "https://api.openai.com/v1": true} {
		t.Run(baseURL, func(t *testing.T) {
			d, buf := doctorRawHome(t, "model:\n  provider: openai-api\n  default: nvidia/z-ai/glm-5.2\n  base_url: "+baseURL+"\n", "")
			out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
			warning := "model.default 'nvidia/z-ai/glm-5.2' uses a vendor/model slug but provider is 'openai-api'"
			if strings.Contains(out, warning) != expectsWarning {
				t.Fatalf("warning present %v, want %v: %q", !expectsWarning, expectsWarning, out)
			}
		})
	}
}

func TestRunDoctorAcceptsKimiCodingCnProvider(t *testing.T) {
	d, buf := doctorRawHome(t, "model:\n  provider: kimi-coding-cn\n  default: kimi-k2.6\n", "")
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorConfigFileEntry)
	if !strings.Contains(out, "/config.yaml exists") || strings.Contains(out, "model.provider 'kimi-coding-cn' is not a recognised provider") {
		t.Fatalf("out = %q", out)
	}
}

// Runs the configuration checks over a home whose .env may carry a stale iteration budget
func doctorRunConfigSection(t *testing.T, fix bool, ghost, cfgTurns, processValue string) (string, string) {
	t.Helper()
	envText := "OPENAI_API_KEY=sk-test\n"
	if ghost != "" {
		envText += "ICEBATH_MAX_ITERATIONS=" + ghost + "\n"
	}
	d, buf := doctorRawHome(t, "agent:\n  max_turns: "+cfgTurns+"\n", envText)
	if processValue != "" {
		t.Setenv("ICEBATH_MAX_ITERATIONS", processValue)
	}
	out, _ := doctorWalk(t, d, buf, map[string]any{"fix": fix},
		doctorCheckEntry{"Configuration Files", checkEnvFile}, doctorConfigFileEntry, doctorCheckEntry{"", checkConfigDrift})
	return out, d.Home
}

func TestDoctorStaleMaxIterationsDriftDetectsDriftWarnOnly(t *testing.T) {
	out, h := doctorRunConfigSection(t, false, "90", "400", "400")
	if !strings.Contains(out, "ICEBATH_MAX_ITERATIONS=90") || !strings.Contains(out, "shadows") {
		t.Fatalf("out = %q", out)
	}
	if env, err := os.ReadFile(filepath.Join(h, ".env")); err != nil || !strings.Contains(string(env), "ICEBATH_MAX_ITERATIONS=90") {
		t.Fatalf(".env = %q %v", env, err)
	}
}

func TestDoctorStaleMaxIterationsDriftFixRemovesGhost(t *testing.T) {
	out, h := doctorRunConfigSection(t, true, "90", "400", "400")
	if !strings.Contains(out, "Removed stale ICEBATH_MAX_ITERATIONS") {
		t.Fatalf("out = %q", out)
	}
	env, err := os.ReadFile(filepath.Join(h, ".env"))
	if err != nil || strings.Contains(string(env), "ICEBATH_MAX_ITERATIONS") || !strings.Contains(string(env), "OPENAI_API_KEY=sk-test") {
		t.Fatalf(".env = %q %v", env, err)
	}
}

func TestDoctorStaleMaxIterationsDriftNoDriftWhenGhostAbsent(t *testing.T) {
	if out, _ := doctorRunConfigSection(t, false, "", "400", ""); strings.Contains(out, "shadows") {
		t.Fatalf("out = %q", out)
	}
}

// Runs the legacy custom providers drift step over a standalone config file
func doctorRunLegacyResidue(t *testing.T, yamlText string) (string, *Finding) {
	t.Helper()
	d, out := doctorRawHome(t, "", "")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &Finding{}
	if err := doctorDriftLegacyCustomProviders(context.Background(), d, f, false, cfg); err != nil {
		t.Fatal(err)
	}
	return out.String(), f
}

func TestDoctorLegacyCustomProvidersResidueOrphanEntryIsFlaggedWithRepairInstruction(t *testing.T) {
	out, f := doctorRunLegacyResidue(t, "custom_providers:\n  - name: Local (8283)\n    base_url: http://127.0.0.1:8283/v1\n"+
		"providers:\n  other:\n    api: http://127.0.0.1:8290/v1\n")
	if !strings.Contains(out, "Local (8283)") {
		t.Fatalf("out = %q", out)
	}
	if len(f.ManualIssues) == 0 || !strings.Contains(f.ManualIssues[0], "providers.<key>.api: http://127.0.0.1:8283/v1") {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
	if f.Fixed != 0 || len(f.Issues) != 0 {
		t.Fatalf("finding = %#v", f)
	}
}

func TestDoctorLegacyCustomProvidersResidueTwinAndScalarAreSilent(t *testing.T) {
	out, f := doctorRunLegacyResidue(t, "custom_providers:\n  - name: Local\n    base_url: http://127.0.0.1:8283/V1/\n"+
		"providers:\n  local:\n    api: http://127.0.0.1:8283/v1\n")
	if out != "" || len(f.ManualIssues) != 0 {
		t.Fatalf("out = %q finding = %#v", out, f)
	}
	out, f = doctorRunLegacyResidue(t, "custom_providers: oops\n")
	if out != "" || len(f.ManualIssues) != 0 {
		t.Fatalf("out = %q finding = %#v", out, f)
	}
}

func TestDoctorDeprecatedConfigAndEnvCollectDeprecatedEnvVarsIgnoresEmpty(t *testing.T) {
	for _, env := range []map[string]any{{"TERMINAL_CWD": "  "}, {}, nil} {
		if got := doctorCollectDeprecatedEnvVars(env); len(got) != 0 {
			t.Fatalf("env %v gave %v", env, got)
		}
	}
	if got := doctorCollectDeprecatedEnvVars(map[string]any{"TERMINAL_CWD": "/srv"}); len(got) != 1 || got[0][0] != "TERMINAL_CWD" {
		t.Fatalf("got %v", got)
	}
}

func TestDoctorReportsAuxiliaryBlocksThatDoNotResolve(t *testing.T) {
	d, _ := doctorRawHome(t, "", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	body := "auxiliary:\n  background_review:\n    provider: no-such-provider\n    model: m\n" +
		"  compression:\n    provider: openai\n    model: gpt-x\n    base_url: https://gateway.example/v1\n    api_key: gw\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var issues []string
	if err := doctorValidateAuxiliaryConfig(context.Background(), d, cfg, &issues); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0], "auxiliary.background_review") || !strings.Contains(issues[0], "no-such-provider") {
		t.Fatalf("issues = %q", issues)
	}
}

func TestCleanConfigPrintsNothing(t *testing.T) {
	d, out := doctorRawHome(t, "", "")
	doctorCheckChannelRecordHygiene(context.Background(), d)
	if out.String() != "" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestStaleRecordsWarnedWithReasonTexts(t *testing.T) {
	tmp := t.TempDir()
	gone, live := filepath.Join(tmp, "gone"), filepath.Join(tmp, "live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	zeros := strings.Repeat("0", 16)
	d, _ := doctorRawHome(t, "", "")
	out := &bytes.Buffer{}
	d.W = out
	body := "update:\n  installs:\n    deadbeefdeadbeef:\n      path: " + gone + "\n      channel: main\n" +
		"    \"" + zeros + "\":\n      path: " + live + "\n      channel: stable\n"
	if err := os.WriteFile(filepath.Join(d.Home, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	doctorCheckChannelRecordHygiene(context.Background(), d)
	for _, want := range []string{
		"Stale channel record: deadbeefdeadbeef", "safe to remove update.installs.deadbeefdeadbeef",
		"Stale channel record: " + zeros, "different install now",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from %q", want, out.String())
		}
	}
}

func TestUnreadableConfigDegradesToAWarning(t *testing.T) {
	doctorHermeticEnv(t)
	blocked := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	_, loadErr := config.LoadContext(context.Background(), blocked, nil)
	if loadErr == nil {
		t.Fatal("the config of an unusable home loaded")
	}
	out := &bytes.Buffer{}
	doctorCheckChannelRecordHygiene(context.Background(), &Doctor{W: out, Home: blocked, DisplayHome: blocked})
	if !strings.Contains(out.String(), "Channel-record hygiene unreadable") || !strings.Contains(out.String(), "("+loadErr.Error()+")") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestDoctorSilentWithNoManagedScope(t *testing.T) {
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "nope"))
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	out := &bytes.Buffer{}
	doctorManagedScopeCheck(out)
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("out = %q", out.String())
	}
}

func doctorFindingsMap(findings [][2]string) map[string]string {
	out := map[string]string{}
	for _, f := range findings {
		out[f[0]] = f[1]
	}
	return out
}

func TestDoctorReportsStaleRelayPluginKey(t *testing.T) {
	findings := doctorFindingsMap(doctorCollectRelayPluginCutoverFindings(
		map[string]any{"plugins": map[string]any{"enabled": []any{"observability/nemo_relay"}}}, map[string]any{}))
	replacement := findings["plugins.enabled: observability/nemo_relay"]
	if !strings.Contains(replacement, "remove it") || !strings.Contains(replacement, tools.RelayPluginsConfigEnv) {
		t.Fatalf("findings = %v", findings)
	}
}

func TestDoctorReportsLegacyExporterEnvWithoutNewConfig(t *testing.T) {
	doctorUnsetEnv(t, tools.RelayPluginsConfigEnv)
	findings := doctorFindingsMap(doctorCollectRelayPluginCutoverFindings(map[string]any{}, map[string]any{
		"ICEBATH_NEMO_RELAY_ATIF_ENABLED": "true", "ICEBATH_NEMO_RELAY_ATIF_EXPORT_TIMEOUT_S": "30"}))
	for _, name := range []string{"ICEBATH_NEMO_RELAY_ATIF_ENABLED", "ICEBATH_NEMO_RELAY_ATIF_EXPORT_TIMEOUT_S"} {
		if !strings.Contains(findings[name], "now ignored") || !strings.Contains(findings[name], tools.RelayPluginsConfigEnv) {
			t.Fatalf("findings = %v", findings)
		}
	}
}

func TestDoctorDoesNotWarnForLegacyEnvAfterNewConfigIsSelected(t *testing.T) {
	doctorUnsetEnv(t, tools.RelayPluginsConfigEnv)
	findings := doctorCollectRelayPluginCutoverFindings(map[string]any{}, map[string]any{
		tools.RelayPluginsConfigEnv: "/tmp/plugins.toml", "ICEBATH_NEMO_RELAY_ATIF_ENABLED": "true"})
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}
}

const doctorEnabledObservability = "version = 1\n\n[[components]]\nkind = \"observability\"\nenabled = true\n\n[components.config]\nversion = 4\n"

func TestDoctorListsTheFilesTheRuntimeLoads(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "discovered"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			userConfig := filepath.Join(tmp, "xdg", "nemo-relay", "plugins.toml")
			if err := os.MkdirAll(filepath.Dir(userConfig), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(userConfig, []byte(doctorEnabledObservability), 0o644); err != nil {
				t.Fatal(err)
			}
			d, out := doctorRawHome(t, "", "")
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
			t.Setenv("ProgramData", filepath.Join(tmp, "programdata"))
			doctorUnsetEnv(t, tools.RelayPluginsConfigEnv)
			if explicit {
				selected := filepath.Join(tmp, "selected.toml")
				if err := os.WriteFile(selected, []byte(doctorEnabledObservability), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv(tools.RelayPluginsConfigEnv, selected)
			}
			var loaded []string
			tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) {
				return &tools.NemoRelay{Plugin: &tools.NemoRelayPlugin{
					Validate: func(_ context.Context, _ map[string]any, additionalPluginsTOML string) (map[string]any, error) {
						loaded = []string{filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "nemo-relay", "plugins.toml")}
						if additionalPluginsTOML != "" {
							loaded = []string{additionalPluginsTOML}
						}
						paths := make([]any, 0, len(loaded))
						for _, p := range loaded {
							paths = append(paths, p)
						}
						return map[string]any{
							"config_paths":    paths,
							"config":          map[string]any{"diagnostics": []any{}},
							"dynamic_plugins": []any{},
							"resolved_config": map[string]any{"components": []any{map[string]any{"kind": "observability", "enabled": true}}},
						}, nil
					},
				}}, nil
			})
			t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
			f, err := checkRelayPlugins(context.Background(), d, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Relay plugins enabled") || len(f.ManualIssues) != 0 {
				t.Fatalf("out = %q finding = %#v", out.String(), f)
			}
			userLoaded := false
			for _, p := range loaded {
				userLoaded = userLoaded || p == userConfig
				if !strings.Contains(out.String(), p) {
					t.Fatalf("loaded file %s missing from %q", p, out.String())
				}
			}
			if userLoaded == explicit || (explicit && strings.Contains(out.String(), userConfig)) {
				t.Fatalf("user file loaded %v with explicit %v: %q", userLoaded, explicit, out.String())
			}
		})
	}
}

func TestCheckRelayPluginsWithoutABindingReportsThePlatform(t *testing.T) {
	d, out := doctorRawHome(t, "", "")
	tools.RegisterNemoRelayBinding(nil)
	f, err := checkRelayPlugins(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NeMo Relay is not available on this platform") || len(f.ManualIssues) != 0 {
		t.Fatalf("out = %q finding = %#v", out.String(), f)
	}
}

func TestCheckRelayPluginsReportsABindingWithoutAValidator(t *testing.T) {
	d, out := doctorRawHome(t, "", "")
	doctorUnsetEnv(t, tools.RelayPluginsConfigEnv)
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return &tools.NemoRelay{}, nil })
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
	f, err := checkRelayPlugins(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Relay plugin configuration could not be read") || !strings.Contains(out.String(), "(Icebath runs without Relay plugins)") {
		t.Fatalf("out = %q", out.String())
	}
	if len(f.ManualIssues) != 1 || f.ManualIssues[0] != "Fix the Relay plugin configuration shown under NeMo Relay Plugins." {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
}
