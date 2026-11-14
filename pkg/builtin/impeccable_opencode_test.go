package builtin

import (
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const opencodeSampleDescription = "Use when the user wants to design, redesign, shape, critique, audit, polish, clarify, distill, harden, optimize, adapt, animate, colorize, extract, or otherwise improve a frontend interface."

var opencodeFrontmatterRe = regexp.MustCompile(`^---\n([\s\S]*?)\n---`)

// Dist directory and command file path after the opencode transform of the skills
func opencodeTransform(t *testing.T, skills []impeccableSkill) (string, string) {
	t.Helper()
	config, _ := impeccableProviderByName("opencode")
	dist := filepath.Join(t.TempDir(), "test-tmp-opencode-commands")
	if err := config.transform(skills, dist, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	return dist, filepath.Join(dist, config.Provider, config.ConfigDir, "commands", "impeccable.md")
}

func opencodeSampleSkill() []impeccableSkill {
	return []impeccableSkill{{Name: "impeccable", Description: opencodeSampleDescription, Body: "# Impeccable\n\nSkill body here.",
		References: []impeccableFile{}, Scripts: []impeccableScript{}, Agents: []impeccableAgent{}}}
}

func opencodeCommandFrontmatter(t *testing.T, content string) string {
	t.Helper()
	fm := opencodeFrontmatterRe.FindStringSubmatch(content)
	if fm == nil {
		t.Fatalf("command has no frontmatter\n%s", content)
	}
	return fm[1]
}

func TestOpencodeCommandsBridgeEmitsOpencodeCommandsImpeccableMdAlongsideTheSkill(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	wantExists(t, commandPath)
}

func TestOpencodeCommandsBridgeCommandFrontmatterUsesOnlyFieldsOpenCodeRecognises(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	allowed := []string{"description", "agent", "model", "variant", "subtask"}
	for _, line := range strings.Split(opencodeCommandFrontmatter(t, readTestFile(t, commandPath)), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if key := strings.Split(line, ":")[0]; !slices.Contains(allowed, key) {
			t.Errorf("command frontmatter key %q", key)
		}
	}
}

func TestOpencodeCommandsBridgeCommandDescriptionMirrorsTheSkillDescriptionExactly(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	found := false
	for _, line := range strings.Split(opencodeCommandFrontmatter(t, readTestFile(t, commandPath)), "\n") {
		if !strings.HasPrefix(line, "description:") {
			continue
		}
		found = true
		value := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		if m := regexp.MustCompile(`^"(.*)"$`).FindStringSubmatch(value); m != nil {
			value = m[1]
		}
		if value != opencodeSampleDescription {
			t.Fatalf("description %q", value)
		}
		break
	}
	if !found {
		t.Fatal("command frontmatter has no description")
	}
}

func TestOpencodeCommandsBridgeCommandBodyDelegatesToTheImpeccableSkill(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	body := regexp.MustCompile(`^---\n[\s\S]*?\n---\n`).ReplaceAllString(readTestFile(t, commandPath), "")
	wantContains(t, body, "skill({", `name: "impeccable"`, "Setup", "Commands", "$ARGUMENTS")
}

func TestOpencodeCommandsBridgeCommandDeclaresAgentBuildAndSubtaskTrue(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	content := readTestFile(t, commandPath)
	if !regexp.MustCompile(`(?m)^agent: build$`).MatchString(content) || !regexp.MustCompile(`(?m)^subtask: true$`).MatchString(content) {
		t.Fatalf("command\n%s", content)
	}
}

func TestOpencodeCommandsBridgeDoesNotEmitClaudeOnlyFrontmatterFieldsOnTheCommand(t *testing.T) {
	_, commandPath := opencodeTransform(t, opencodeSampleSkill())
	fm := opencodeCommandFrontmatter(t, readTestFile(t, commandPath))
	for _, key := range []string{"version", "user-invocable", "argument-hint", "allowed-tools"} {
		if regexp.MustCompile(`(?m)^` + key + `:`).MatchString(fm) {
			t.Errorf("command frontmatter carries %s\n%s", key, fm)
		}
	}
}

func TestOpencodeCommandsBridgeEmitsNoCommandWhenTheSkillIsEmpty(t *testing.T) {
	_, commandPath := opencodeTransform(t, nil)
	if pathExists(filepath.Dir(commandPath)) {
		t.Fatal("commands directory written for an empty skill list")
	}
}

func TestOpencodeCommandsBridgeKeepsEmittingTheSkillAlongsideTheCommand(t *testing.T) {
	dist, commandPath := opencodeTransform(t, opencodeSampleSkill())
	wantExists(t, filepath.Join(dist, "opencode/.opencode/skills/impeccable/SKILL.md"), commandPath)
}
