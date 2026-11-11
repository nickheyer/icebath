package builtin

import (
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Runs a case once per PROVIDERS entry that passes the filter, each against its own dist directory
func eachProvider(t *testing.T, filter func(impeccableProvider) bool, run func(t *testing.T, config impeccableProvider, dist string)) {
	t.Helper()
	ran := 0
	for _, config := range impeccableProviders {
		if filter != nil && !filter(config) {
			continue
		}
		ran++
		t.Run(config.Provider, func(t *testing.T) {
			run(t, config, filepath.Join(t.TempDir(), "test-tmp-providers"))
		})
	}
	if ran == 0 {
		t.Fatal("no provider matched the case")
	}
}

func hasField(field string) func(impeccableProvider) bool {
	return func(config impeccableProvider) bool { return slices.Contains(config.FrontmatterFields, field) }
}

func onlyProvider(key string) func(impeccableProvider) bool {
	return func(config impeccableProvider) bool { return config.Provider == key }
}

func providerSkillMd(t *testing.T, config impeccableProvider, dist, skillName string, version any, skills []impeccableSkill) string {
	t.Helper()
	if err := config.transform(skills, dist, version, io.Discard); err != nil {
		t.Fatal(err)
	}
	return readTestFile(t, filepath.Join(dist, config.Provider, config.ConfigDir, "skills", skillName, "SKILL.md"))
}

func providerToken(config impeccableProvider, token string) string {
	key := config.PlaceholderProvider
	if key == "" {
		key = config.Provider
	}
	return providerTokenTable[key][token]
}

func TestProviderShouldCreateCorrectDirectoryStructure(t *testing.T) {
	eachProvider(t, nil, func(t *testing.T, config impeccableProvider, dist string) {
		if err := config.transform(nil, dist, "", io.Discard); err != nil {
			t.Fatal(err)
		}
		wantExists(t, filepath.Join(dist, config.Provider, config.ConfigDir, "skills"))
	})
}

func TestProviderShouldReplaceModelPlaceholderCorrectly(t *testing.T) {
	eachProvider(t, nil, func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "Ask {{model}} for help."}})
		wantContains(t, content, "Ask "+providerToken(config, "model")+" for help.")
	})
}

func TestProviderShouldReplaceConfigFilePlaceholderCorrectly(t *testing.T) {
	eachProvider(t, nil, func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "See {{config_file}}."}})
		wantContains(t, content, "See "+providerToken(config, "config_file")+".")
	})
}

func TestProviderShouldCopyReferenceFiles(t *testing.T) {
	eachProvider(t, nil, func(t *testing.T, config impeccableProvider, dist string) {
		providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "Body",
			References: []impeccableFile{{Name: "ref", Content: "Ref content"}}}})
		wantExists(t, filepath.Join(dist, config.Provider, config.ConfigDir, "skills", "test", "reference", "ref.md"))
	})
}

func TestProviderOmpUsesPiStyleGuidancePlaceholdersRatherThanCursorFallbacks(t *testing.T) {
	eachProvider(t, onlyProvider("omp"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "Read {{config_file}} before asking {{model}}."}})
		wantContains(t, content, "Read AGENTS.md before asking the model.")
		wantLacks(t, content, ".cursorrules")
	})
}

func TestProviderDevinEmitsArgumentHintButNeitherUserInvocableNorAllowedTools(t *testing.T) {
	eachProvider(t, onlyProvider("devin"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "Ask {{model}}.",
			UserInvocable: true, ArgumentHint: "[target]", AllowedTools: []any{"Bash(npx impeccable *)"}}})
		wantContains(t, content, `argument-hint: "[target]"`, "Ask Devin.")
		wantLacks(t, content, "user-invocable", "allowed-tools")
	})
}

func TestProviderDshUsesDSHToolsAndResourcePathsWithoutInstallingUnsupportedHooksOrAgents(t *testing.T) {
	eachProvider(t, onlyProvider("dsh"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "impeccable", "", []impeccableSkill{{Name: "impeccable", Description: "Test", UserInvocable: true, AllowedTools: "Bash",
			Body:       "{{ask_instruction}} Run `{{scripts_path}}/impeccable context`.",
			References: []impeccableFile{{Name: "polish", Content: "Run `{{scripts_path}}/impeccable detect`."}}}})
		wantContains(t, content, "Ask with the ask_user_question tool", ".dsh/skills/impeccable/scripts/impeccable context")
		if fm, _ := parseImpeccableFrontmatter(content); fmHas(fm, "allowed-tools") {
			t.Fatalf("allowed-tools emitted: %+v", fm)
		}
		root := filepath.Join(dist, "dsh", ".dsh")
		wantContains(t, readTestFile(t, filepath.Join(root, "skills/impeccable/reference/polish.md")), ".dsh/skills/impeccable/scripts/impeccable detect")
		if names := dirNames(t, root); !slices.Equal(names, []string{"skills"}) {
			t.Fatalf(".dsh holds %v", names)
		}
		if config.EmitHooks != "" || config.AgentFormat != "" {
			t.Fatalf("dsh config emits hooks %q or agents %q", config.EmitHooks, config.AgentFormat)
		}
	})
}

func TestProviderShouldEmitSkillsVersionInGeneratedSkillFrontmatter(t *testing.T) {
	eachProvider(t, nil, func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "1.2.3-test", []impeccableSkill{{Name: "test", Description: "Test", Body: "Body"}})
		fm, _ := parseImpeccableFrontmatter(content)
		if config.Provider == "codex" || config.Provider == "agents" {
			if fmHas(fm, "version") {
				t.Fatalf("top level version emitted: %+v", fm)
			}
			wantContains(t, content, "metadata:\n  version: 1.2.3-test")
		} else if fmLookup(fm, "version") != "1.2.3-test" {
			t.Fatalf("version %v", fmLookup(fm, "version"))
		}
	})
}

func TestProviderShouldEmitUserInvocableForUserInvocableSkills(t *testing.T) {
	eachProvider(t, hasField("user-invocable"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, Body: "Body"}}))
		if fmLookup(fm, "user-invocable") != true {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldOmitUserInvocableForNonUserInvocableSkills(t *testing.T) {
	eachProvider(t, hasField("user-invocable"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Body: "Body"}}))
		if fmHas(fm, "user-invocable") {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldEmitArgumentHintForUserInvocableSkills(t *testing.T) {
	eachProvider(t, hasField("argument-hint"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, ArgumentHint: "[target]", Body: "Body"}}))
		if fmLookup(fm, "argument-hint") != "[target]" {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldNotEmitArgumentHintForNonUserInvocableSkills(t *testing.T) {
	eachProvider(t, hasField("argument-hint"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", ArgumentHint: "[target]", Body: "Body"}}))
		if fmHas(fm, "argument-hint") {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldEmitLicenseWhenPresent(t *testing.T) {
	eachProvider(t, hasField("license"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", License: "MIT", Body: "Body"}}))
		if fmLookup(fm, "license") != "MIT" {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldOmitLicenseWhenEmpty(t *testing.T) {
	eachProvider(t, hasField("license"), func(t *testing.T, config impeccableProvider, dist string) {
		fm, _ := parseImpeccableFrontmatter(providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", License: "", Body: "Body"}}))
		if fmHas(fm, "license") {
			t.Fatalf("%+v", fm)
		}
	})
}

func TestProviderShouldEmitCompatibilityWhenPresent(t *testing.T) {
	eachProvider(t, hasField("compatibility"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Compatibility: config.Provider, Body: "Body"}})
		wantContains(t, content, "compatibility: "+config.Provider)
	})
}

func TestProviderShouldEmitMetadataWhenPresent(t *testing.T) {
	eachProvider(t, hasField("metadata"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", Metadata: "some-metadata", Body: "Body"}})
		wantContains(t, content, "metadata: some-metadata")
	})
}

func TestProviderShouldEmitAllowedToolsWhenPresent(t *testing.T) {
	eachProvider(t, hasField("allowed-tools"), func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", AllowedTools: "Bash,Edit", Body: "Body"}})
		wantContains(t, content, "allowed-tools: Bash,Edit")
	})
}

func TestProviderShouldNotEmitFieldsOutsideAllowlist(t *testing.T) {
	allOptionalFields := []string{"user-invocable", "argument-hint", "license", "compatibility", "metadata", "allowed-tools"}
	excluded := func(config impeccableProvider) []string {
		var out []string
		for _, f := range allOptionalFields {
			if !slices.Contains(config.FrontmatterFields, f) {
				out = append(out, f)
			}
		}
		return out
	}
	eachProvider(t, func(config impeccableProvider) bool { return len(excluded(config)) > 0 }, func(t *testing.T, config impeccableProvider, dist string) {
		content := providerSkillMd(t, config, dist, "test", "", []impeccableSkill{{Name: "test", Description: "Test", UserInvocable: true, ArgumentHint: "[target]",
			License: "MIT", Compatibility: "all", Metadata: "meta", AllowedTools: "Bash", Body: "Body"}})
		var frontmatterLines []string
		inFrontmatter := false
		for _, line := range strings.Split(content, "\n") {
			if line == "---" {
				if inFrontmatter {
					break
				}
				inFrontmatter = true
				continue
			}
			if inFrontmatter {
				frontmatterLines = append(frontmatterLines, line)
			}
		}
		for _, field := range excluded(config) {
			if slices.ContainsFunc(frontmatterLines, func(l string) bool { return strings.HasPrefix(l, field+":") }) {
				t.Errorf("%s emitted outside the allowlist\n%s", field, content)
			}
		}
	})
}
