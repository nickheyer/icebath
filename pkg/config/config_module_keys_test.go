package config

import (
	"sort"
	"strings"
	"testing"
)

// Runs the body under each env name policy and restores the host policy afterwards
func withEnvPolicy(t *testing.T, windows bool, body func()) {
	t.Helper()
	prev := envPolicyWindows
	envPolicyWindows = windows
	defer func() { envPolicyWindows = prev }()
	body()
}

func TestEnvWriteDenylistWindowsPolicyDeniesMixedCaseExecNames(t *testing.T) {
	for _, key := range []string{"Ld_Preload", "Git_Config_Parameters"} {
		t.Run(key, func(t *testing.T) {
			h, _, _ := moduleHome(t, "")
			if err := EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			withEnvPolicy(t, true, func() {
				err := SaveEnvValueContext(t.Context(), h, key, "1")
				if err == nil || !strings.Contains(err.Error(), "denylist") {
					t.Fatalf("%s written: %v", key, err)
				}
			})
		})
	}
}

func TestEnvWriteDenylistWindowsPolicyNamesAreCaseInsensitive(t *testing.T) {
	for _, tc := range [][2]string{
		{"Path", "PATH"},
		{"Icebath_Yolo_Mode", "ICEBATH_YOLO_MODE"},
		{"Icebath_Optional_Mcps", "ICEBATH_OPTIONAL_MCPS"},
		{"Icebath_Copilot_Acp_Command", "ICEBATH_COPILOT_ACP_COMMAND"},
		{"Icebath_Copilot_Acp_Args", "ICEBATH_COPILOT_ACP_ARGS"},
	} {
		withEnvPolicy(t, true, func() {
			if got := envVarPolicyName(tc[0]); got != tc[1] {
				t.Fatalf("%s -> %s, want %s", tc[0], got, tc[1])
			}
		})
	}
}

func TestEnvWriteDenylistPosixPolicyNamesRemainCaseSensitive(t *testing.T) {
	withEnvPolicy(t, false, func() {
		if got := envVarPolicyName("Path"); got != "Path" {
			t.Fatalf("Path -> %s", got)
		}
	})
}

func TestEnvWriteDenylistWindowsEnvAssignmentMatchingIsCaseInsensitive(t *testing.T) {
	for _, prefix := range []string{"", "export "} {
		line := prefix + "Path=C:\\Windows\\System32\n"
		withEnvPolicy(t, true, func() {
			if !envLineDefines(line, "PATH") {
				t.Fatalf("windows policy missed %q", line)
			}
		})
		withEnvPolicy(t, false, func() {
			if envLineDefines(line, "PATH") {
				t.Fatalf("posix policy matched %q", line)
			}
		})
	}
}

func TestEnvVarPolicyNameUppercasesLikePythonStr(t *testing.T) {
	withEnvPolicy(t, true, func() {
		if got := envVarPolicyName("stra\u00dfe"); got != "STRASSE" {
			t.Fatalf("policy name %q", got)
		}
	})
}

func TestConfigNormalizationDoesNotOverwriteUserValuesSaveConfigDoesNotInjectMaxTurnsWhenUnset(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	moduleDump(t, path, map[string]any{"_config_version": latestConfigVersion(), "memory": map[string]any{"user_char_limit": 2200}})
	moduleSave(t, h, moduleLoad(t, h))
	raw := moduleYAML(t, path)
	if agent, _ := raw["agent"].(map[string]any); agent != nil {
		if _, has := agent["max_turns"]; has {
			t.Fatalf("max_turns injected: %v", raw["agent"])
		}
	}
	if !pyEq(Tree(raw).Get("memory.user_char_limit"), 2200) {
		t.Fatalf("memory %v", raw["memory"])
	}
}

func TestConfigNormalizationDoesNotOverwriteUserValuesNormalizeMaxTurnsDoesNotInjectDefault(t *testing.T) {
	result, err := normalizeMaxTurns(map[string]any{"_config_version": latestConfigVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if agent, _ := result["agent"].(map[string]any); agent != nil {
		if _, has := agent["max_turns"]; has {
			t.Fatalf("max_turns injected: %v", result)
		}
	}
}

func TestDefaultConfigKanbanBlockNotDroppedByDuplicateKey(t *testing.T) {
	kanban := Defaults().Map("kanban")
	if kanban["auto_subscribe_on_create"] != true {
		t.Fatalf("auto_subscribe_on_create %v", kanban["auto_subscribe_on_create"])
	}
	for _, key := range []string{"dispatch_in_gateway", "auto_decompose"} {
		if _, has := kanban[key]; !has {
			t.Fatalf("kanban lacks %s", key)
		}
	}
}

func TestDefaultConfigHasNoDuplicateTopLevelKeys(t *testing.T) {
	declared := defaultsKeyOrder[""]
	seen := map[string]int{}
	for _, k := range declared {
		seen[k]++
	}
	var dupes []string
	for k, n := range seen {
		if n > 1 {
			dupes = append(dupes, k)
		}
	}
	sort.Strings(dupes)
	if len(dupes) != 0 {
		t.Fatalf("duplicate default config keys: %v", dupes)
	}
	literal := defaultsLiteral()
	if seen["model"] != 1 || seen["kanban"] != 1 || len(seen) != len(literal) {
		t.Fatalf("declared order names %d keys, the literal %d", len(seen), len(literal))
	}
	for k := range literal {
		if seen[k] != 1 {
			t.Fatalf("%s is in the literal and not in the declared order", k)
		}
	}
}

func TestGatewayMultiplexKeysAreRecognizedConfigKeys(t *testing.T) {
	src := pathCommandSources()
	if _, has := Defaults().Map("gateway")["auto_migrate"]; has {
		t.Fatal("gateway.auto_migrate is a default")
	}
	for _, key := range []string{"gateway.multiplex_profiles", "gateway.profile_routes", "gateway.auto_multiplex_migration"} {
		if known, suggestion := ValidateConfigKey(key, src); !known || suggestion != "" {
			t.Fatalf("%s known %v suggestion %q", key, known, suggestion)
		}
	}
	if known, suggestion := ValidateConfigKey("gateway.auto_migrate", src); known || suggestion != "gateway.auto_multiplex_migration" {
		t.Fatalf("gateway.auto_migrate known %v suggestion %q", known, suggestion)
	}
}

func TestEmptyDictDefaultSectionsAreOpenContainers(t *testing.T) {
	src := pathCommandSources()
	defaults := Defaults()
	for _, path := range []string{"compression.model_thresholds", "terminal.docker_env"} {
		if m, ok := defaults.Get(path).(map[string]any); !ok || len(m) != 0 {
			t.Fatalf("%s default %v", path, defaults.Get(path))
		}
	}
	for _, key := range []string{"compression.model_thresholds.gpt-5", "terminal.docker_env.FOO", "lsp.servers.python.command", "auxiliary.vision.extra_body.reasoning"} {
		if known, suggestion := ValidateConfigKey(key, src); !known || suggestion != "" {
			t.Fatalf("%s known %v suggestion %q", key, known, suggestion)
		}
	}
	if known, suggestion := ValidateConfigKey("compression.model_threshold.gpt-5", src); known || suggestion != "compression.model_thresholds" {
		t.Fatalf("typo known %v suggestion %q", known, suggestion)
	}
}

func TestLspRootPolicyKeysAreRecognizedAndOffByDefault(t *testing.T) {
	src := pathCommandSources()
	for _, key := range []string{"lsp.warmup_timeout", "lsp.broken_retry_seconds", "lsp.exclude_roots"} {
		if known, suggestion := ValidateConfigKey(key, src); !known || suggestion != "" {
			t.Fatalf("%s known %v suggestion %q", key, known, suggestion)
		}
	}
}

func TestSaveConfigExplicitPathAuthoritySaveConfigOnIntactFilePreservesExplicitDefaults(t *testing.T) {
	h, path, _ := moduleHome(t, "model:\n  provider: test/p\nskills:\n  write_approval: true\n")
	cfg := moduleLoad(t, h)
	cfg["model"] = "test/other"
	moduleSave(t, h, cfg)
	saved := moduleYAML(t, path)
	if saved["model"] != "test/other" || Tree(saved).Get("skills.write_approval") != true {
		t.Fatalf("saved %v", saved)
	}
}

func TestSaveConfigExplicitPathAuthoritySaveSurvivesCachedRawReadReturningEmpty(t *testing.T) {
	defaults := Defaults()
	chosen := map[string]any{}
	for _, k := range MappingKeys(defaults) {
		section, ok := defaults[k].(map[string]any)
		if !ok || len(section) == 0 || k == "agent" {
			continue
		}
		first := MappingKeys(section)[0]
		switch section[first].(type) {
		case map[string]any, []any:
			continue
		}
		chosen[k] = map[string]any{first: section[first]}
	}
	if len(chosen) < 10 {
		t.Fatalf("only %d sections chosen", len(chosen))
	}
	h, path, _ := moduleHome(t, "")
	moduleDump(t, path, chosen)
	cfg := moduleLoad(t, h)
	sig, err := ConfigCacheVersion(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	rawConfigCacheStore(path, sig, map[string]any{})
	if got, err := ReadRawConfig(t.Context(), h); err != nil || len(got) != 0 {
		t.Fatalf("cached raw read %v %v, want the planted empty mapping", got, err)
	}
	moduleSave(t, h, cfg)
	saved := moduleYAML(t, path)
	var lost []string
	for k := range chosen {
		if _, has := saved[k]; !has {
			lost = append(lost, k)
		}
	}
	sort.Strings(lost)
	if len(lost) != 0 {
		t.Fatalf("sections lost by the save: %v", lost)
	}
}

func TestSaveConfigPartialWritePreservationPersistMigrationWritesFullReadRawConfig(t *testing.T) {
	body := "_config_version: 30\nmodel:\n  default: deepseek-v4-pro\n  provider: deepseek\nagent:\n  max_turns: 60\n" +
		"platforms:\n  feishu:\n    enabled: true\n    extra:\n      app_id: cli_xxx\n      app_secret: xxx\n"
	h, path, _ := caseHome(t, body, "")
	cfg, err := ReadRawConfig(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := cfg["agent"].(map[string]any)
	agent["verify_on_stop"] = false
	cfg["_config_version"] = 32
	if err := persistMigration(t.Context(), h, cfg); err != nil {
		t.Fatal(err)
	}
	raw := Tree(moduleYAML(t, path))
	if raw.Get("platforms.feishu.extra.app_id") != "cli_xxx" {
		t.Fatalf("platforms %v", raw["platforms"])
	}
	if _, has := raw.Map("agent")["verify_on_stop"]; has {
		t.Fatalf("default written: %v", raw["agent"])
	}
	if !pyEq(raw.Get("agent.max_turns"), 60) || !pyEq(raw["_config_version"], 32) {
		t.Fatalf("raw %v", raw)
	}
}

func TestUnquotedNumericProviderLoadsAsString(t *testing.T) {
	for _, scalar := range []string{"2", "0", "2.0"} {
		h, _, _ := moduleHome(t, "model:\n  default: deepseek-flash\n  provider: "+scalar+"\n")
		if got := moduleLoad(t, h).Get("model.provider"); got != scalar {
			t.Fatalf("provider %#v, want the string %q", got, scalar)
		}
	}
}

func TestAbsentProviderKeyIsNotInjected(t *testing.T) {
	h, _, _ := moduleHome(t, "model:\n  default: deepseek-flash\n")
	if _, has := moduleLoad(t, h).Map("model")["provider"]; has {
		t.Fatal("provider injected")
	}
}
