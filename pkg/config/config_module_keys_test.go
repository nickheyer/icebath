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
	moduleDump(t, path, map[string]any{"memory": map[string]any{"user_char_limit": 2200}})
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

func TestAbsentProviderKeyIsNotInjected(t *testing.T) {
	h, _, _ := moduleHome(t, "model:\n  default: deepseek-flash\n")
	if _, has := moduleLoad(t, h).Map("model")["provider"]; has {
		t.Fatal("provider injected")
	}
}
