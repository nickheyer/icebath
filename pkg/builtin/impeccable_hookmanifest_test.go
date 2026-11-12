package builtin

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// A built manifest read back as parsed JSON so numbers and nesting compare like a file does
func hookManifest(t *testing.T, built *pyjson.Object) any {
	t.Helper()
	if built == nil {
		t.Fatal("no manifest built")
	}
	parsed, err := jsonParse(jsonStringify(built, ""))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func repoJSON(t *testing.T, rel string) any {
	t.Helper()
	parsed, err := jsonParse(readTestFile(t, impeccableRepoPath(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return parsed
}

// Member access along object keys and array indexes, undefined when a step is missing
func jget(v any, steps ...any) any {
	for _, step := range steps {
		switch s := step.(type) {
		case string:
			obj, ok := v.(*pyjson.Object)
			if !ok {
				return jsUndefined
			}
			got, has := obj.Get(s)
			if !has {
				return jsUndefined
			}
			v = got
		case int:
			list, ok := v.([]any)
			if !ok || s >= len(list) {
				return jsUndefined
			}
			v = list[s]
		}
	}
	return v
}

func jkeys(v any) []string {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil
	}
	return jsObjectKeys(obj.Keys)
}

// assert.deepEqual of two parsed JSON values, object key order ignored
func jsonDeepEqual(a, b any) bool {
	switch x := a.(type) {
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, k := range x.Keys {
			xv, _ := x.Get(k)
			yv, has := y.Get(k)
			if !has || !jsonDeepEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonDeepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return jsonStrictEqual(a, b)
}

func wantJSON(t *testing.T, got any, want any) {
	t.Helper()
	if n, ok := want.(int); ok {
		want = int64(n)
	}
	if !jsonStrictEqual(got, want) {
		t.Fatalf("got %s, want %v", jsonStringify(got, ""), want)
	}
}

func wantUndefined(t *testing.T, got any) {
	t.Helper()
	if got != jsUndefined {
		t.Fatalf("got %s, want undefined", jsonStringify(got, ""))
	}
}

func expectHookCommand(t *testing.T, command any, expectedScriptsDir, verb string) {
	t.Helper()
	text, ok := command.(string)
	if !ok {
		t.Fatalf("command %v is not a string", command)
	}
	launcher := expectedScriptsDir + "/impeccable"
	if !strings.Contains(text, launcher) {
		t.Fatalf("missing %s in %s", launcher, text)
	}
	if !regexp.MustCompile(`^\[ ! -f "[^"]*/impeccable" \] \|\| "[^"]*/impeccable" ` + verb + `$`).MatchString(text) {
		t.Fatalf("missing existence guard around the launcher in %s", text)
	}
	if strings.Contains(text, "node ") || strings.Contains(text, ".mjs") {
		t.Fatalf("hook command depends on node: %s", text)
	}
}

func expectWindowsHookCommand(t *testing.T, command any, expectedScriptsDir string) {
	t.Helper()
	launcher := `"` + strings.ReplaceAll(expectedScriptsDir, "/", `\`) + `\impeccable.cmd"`
	if command != "cmd /c if exist "+launcher+" "+launcher+" hook" {
		t.Fatalf("windows command %v", command)
	}
}

// Every object under manifest.hooks that carries a command or bash string
func hookEntries(manifest any) []*pyjson.Object {
	var out []*pyjson.Object
	var walk func(value any)
	walk = func(value any) {
		switch x := value.(type) {
		case []any:
			for _, item := range x {
				walk(item)
			}
		case *pyjson.Object:
			if isString(jget(x, "command")) || isString(jget(x, "bash")) {
				out = append(out, x)
			}
			for _, k := range x.Keys {
				item, _ := x.Get(k)
				walk(item)
			}
		}
	}
	walk(jget(manifest, "hooks"))
	return out
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func TestHookManifestBuildersBuildsGeminiNativeSessionAndCompletionHooksWithMillisecondTimeouts(t *testing.T) {
	manifest := hookManifest(t, buildGeminiHooksManifest())
	if keys := jkeys(jget(manifest, "hooks")); !slices.Equal(keys, []string{"BeforeTool", "AfterAgent"}) {
		t.Fatalf("events %v", keys)
	}
	wantJSON(t, jget(manifest, "hooks", "BeforeTool", 0, "matcher"), "^run_shell_command$")
	wantJSON(t, jget(manifest, "hooks", "BeforeTool", 0, "hooks", 0, "timeout"), 5000)
	wantJSON(t, jget(manifest, "hooks", "AfterAgent", 0, "hooks", 0, "timeout"), 30000)
	launcher := "$GEMINI_PROJECT_DIR/.gemini/skills/impeccable/scripts/impeccable"
	for _, event := range []string{"BeforeTool", "AfterAgent"} {
		wantJSON(t, jget(manifest, "hooks", event, 0, "hooks", 0, "command"), "[ ! -f "+launcher+" ] || "+launcher+" hook")
	}
}

func TestHookManifestBuildersBuildsClaudeProjectSettingsForTheRealDetectorHook(t *testing.T) {
	manifest := hookManifest(t, buildClaudeSettingsManifest())
	group := jget(manifest, "hooks", "PostToolUse", 0)
	handler := jget(group, "hooks", 0)
	wantJSON(t, jget(group, "matcher"), "Edit|Write")
	if description, _ := jget(manifest, "description").(string); description == "" || strings.Contains(description, "MultiEdit") {
		t.Fatalf("description %q", description)
	}
	wantJSON(t, jget(handler, "type"), "command")
	wantJSON(t, jget(handler, "timeout"), 5)
	wantJSON(t, jget(handler, "statusMessage"), "Checking UI changes")
	expectHookCommand(t, jget(handler, "command"), ".claude/skills/impeccable/scripts", "hook")
	wantContains(t, jget(handler, "command").(string), "${CLAUDE_PROJECT_DIR}")
	wantUndefined(t, jget(handler, "args"))
	expectHookCommand(t, jget(manifest, "hooks", "SessionStart", 0, "hooks", 0, "command"), ".claude/skills/impeccable/scripts", "hook")
	wantJSON(t, jget(manifest, "hooks", "SessionStart", 0, "hooks", 0, "timeout"), 5)

	stop := jget(manifest, "hooks", "Stop", 0, "hooks", 0)
	wantUndefined(t, jget(manifest, "hooks", "Stop", 0, "matcher"))
	wantJSON(t, jget(stop, "timeout"), 30)
	wantJSON(t, jget(stop, "statusMessage"), "Design deep pass")
	expectHookCommand(t, jget(stop, "command"), ".claude/skills/impeccable/scripts", "hook")
}

func TestHookManifestBuildersBuildsCodexProjectLocalHooksForTheRealDetectorHook(t *testing.T) {
	manifest := hookManifest(t, buildCodexHooksManifest(".codex"))
	wantUndefined(t, jget(manifest, "description"))
	group := jget(manifest, "hooks", "PostToolUse", 0)
	handler := jget(group, "hooks", 0)
	wantJSON(t, jget(group, "matcher"), "Edit|Write|apply_patch")
	wantJSON(t, jget(handler, "type"), "command")
	wantJSON(t, jget(handler, "timeout"), 5)
	wantJSON(t, jget(handler, "statusMessage"), "Checking UI changes")
	expectHookCommand(t, jget(handler, "command"), ".codex/skills/impeccable/scripts", "hook")
	wantLacks(t, jget(handler, "command").(string), "git rev-parse --show-toplevel", "${PLUGIN_ROOT}")
	wantUndefined(t, jget(manifest, "hooks", "SessionStart"))

	stop := jget(manifest, "hooks", "Stop", 0, "hooks", 0)
	wantJSON(t, jget(stop, "timeout"), 30)
	expectHookCommand(t, jget(stop, "command"), ".codex/skills/impeccable/scripts", "hook")
	expectWindowsHookCommand(t, jget(handler, "commandWindows"), ".codex/skills/impeccable/scripts")
	expectWindowsHookCommand(t, jget(stop, "commandWindows"), ".codex/skills/impeccable/scripts")
}

func TestHookManifestBuildersDerivesTheCodexHookPayloadPathFromTheInstallDir(t *testing.T) {
	codexDir := hookManifest(t, buildCodexHooksManifest(".codex"))
	expectHookCommand(t, jget(codexDir, "hooks", "PostToolUse", 0, "hooks", 0, "command"), ".codex/skills/impeccable/scripts", "hook")
	expectHookCommand(t, jget(codexDir, "hooks", "Stop", 0, "hooks", 0, "command"), ".codex/skills/impeccable/scripts", "hook")

	agentsDir := hookManifest(t, buildCodexHooksManifest(".agents"))
	expectHookCommand(t, jget(agentsDir, "hooks", "PostToolUse", 0, "hooks", 0, "command"), ".agents/skills/impeccable/scripts", "hook")
	expectHookCommand(t, jget(agentsDir, "hooks", "Stop", 0, "hooks", 0, "command"), ".agents/skills/impeccable/scripts", "hook")
	wantLacks(t, jget(agentsDir, "hooks", "PostToolUse", 0, "hooks", 0, "command").(string), ".codex/skills")

	expectHookCommand(t, jget(hookManifest(t, hooksJSONFor("codex", ".agents")), "hooks", "PostToolUse", 0, "hooks", 0, "command"), ".agents/skills/impeccable/scripts", "hook")
	expectHookCommand(t, jget(hookManifest(t, hooksJSONFor("codex", "")), "hooks", "PostToolUse", 0, "hooks", 0, "command"), ".codex/skills/impeccable/scripts", "hook")
}

func TestHookManifestBuildersBuildsOneCursorPostEditHookOnPostToolUse(t *testing.T) {
	manifest := hookManifest(t, buildCursorHooksManifest(cursorHookScripts))
	afterEdit := jget(manifest, "hooks", "postToolUse", 0)
	wantJSON(t, jget(manifest, "version"), 1)
	if keys := jkeys(jget(manifest, "hooks")); !slices.Equal(keys, []string{"postToolUse"}) {
		t.Fatalf("events %v", keys)
	}
	for _, event := range []string{"preToolUse", "afterFileEdit", "stop", "sessionStart"} {
		wantUndefined(t, jget(manifest, "hooks", event))
	}
	wantJSON(t, jget(afterEdit, "matcher"), "^Write$")
	expectHookCommand(t, jget(afterEdit, "command"), ".cursor/skills/impeccable/scripts", "hook")
	wantJSON(t, jget(afterEdit, "timeout"), 5)
}

func TestHookManifestBuildersBuildsGitHubCopilotRepoLevelHooksForTheRealDetectorHook(t *testing.T) {
	manifest := hookManifest(t, buildGitHubHooksManifest())
	entry := jget(manifest, "hooks", "postToolUse", 0)
	wantJSON(t, jget(manifest, "version"), 1)
	if keys := jkeys(jget(manifest, "hooks")); len(keys) != 1 {
		t.Fatalf("events %v", keys)
	}
	wantJSON(t, jget(entry, "type"), "command")
	wantJSON(t, jget(entry, "matcher"), "edit|create|apply_patch")
	wantJSON(t, jget(entry, "timeoutSec"), 5)
	wantUndefined(t, jget(entry, "timeout"))
	wantUndefined(t, jget(entry, "command"))
	expectHookCommand(t, jget(entry, "bash"), ".github/skills/impeccable/scripts", "hook")
	wantContains(t, jget(entry, "bash").(string), "git rev-parse --show-toplevel")
	wantUndefined(t, jget(manifest, "hooks", "PostToolUse"))
	wantUndefined(t, jget(manifest, "hooks", "preToolUse"))
}

func TestHookManifestBuildersBuildsGrokBuildProjectHooksForTheRealDetectorHook(t *testing.T) {
	manifest := hookManifest(t, buildGrokHooksManifest())
	group := jget(manifest, "hooks", "PostToolUse", 0)
	handler := jget(group, "hooks", 0)
	wantJSON(t, jget(group, "matcher"), "Edit|Write|MultiEdit")
	wantJSON(t, jget(handler, "type"), "command")
	wantJSON(t, jget(handler, "timeout"), 5)
	wantJSON(t, jget(handler, "statusMessage"), "Checking UI changes")
	expectHookCommand(t, jget(handler, "command"), ".grok/skills/impeccable/scripts", "hook")
	wantLacks(t, jget(handler, "command").(string), "${CLAUDE_PROJECT_DIR}", "${GROK_PLUGIN_ROOT}")
	wantUndefined(t, jget(manifest, "hooks", "SessionStart"))

	stop := jget(manifest, "hooks", "Stop", 0, "hooks", 0)
	wantJSON(t, jget(stop, "timeout"), 30)
	wantJSON(t, jget(stop, "statusMessage"), "Design deep pass")
	expectHookCommand(t, jget(stop, "command"), ".grok/skills/impeccable/scripts", "hook")
}

func TestHookManifestBuildersGivesTheClaudePluginManifestWhatVSCodeNeedsOnEveryHandler(t *testing.T) {
	manifest := hookManifest(t, buildClaudePluginHooksManifest())
	launcher := "${CLAUDE_PLUGIN_ROOT}/skills/impeccable/scripts/impeccable"
	windowsForm := `cmd /c if exist ${CLAUDE_PLUGIN_ROOT}\skills\impeccable\scripts\impeccable.cmd ` +
		`${CLAUDE_PLUGIN_ROOT}\skills\impeccable\scripts\impeccable.cmd hook`
	for _, event := range []string{"SessionStart", "PostToolUse", "Stop"} {
		handler := jget(manifest, "hooks", event, 0, "hooks", 0)
		wantJSON(t, jget(handler, "command"), `[ ! -f "`+launcher+`" ] || sh "`+launcher+`" hook`)
		wantJSON(t, jget(handler, "windows"), windowsForm)
		if !jsonDeepEqual(jget(handler, "env"), pyjson.ObjectOf("IMPECCABLE_HOOK_HARNESS", "vscode")) {
			t.Fatalf("%s env %s", event, jsonStringify(jget(handler, "env"), ""))
		}
	}
}

func TestHookManifestBuildersEmitsCommandWindowsOnlyForCodexShapedManifests(t *testing.T) {
	withWindows := []any{hookManifest(t, buildCodexHooksManifest(".codex")), hookManifest(t, buildCodexPluginHooksManifest())}
	without := []any{hookManifest(t, buildClaudeSettingsManifest()), hookManifest(t, buildClaudePluginHooksManifest()),
		hookManifest(t, buildCursorHooksManifest(cursorHookScripts)), hookManifest(t, buildGitHubHooksManifest()), hookManifest(t, buildGrokHooksManifest())}
	for _, manifest := range withWindows {
		entries := hookEntries(manifest)
		if len(entries) == 0 {
			t.Fatal("manifest holds no hook entries")
		}
		for _, entry := range entries {
			if text, ok := jget(entry, "commandWindows").(string); !ok || !strings.Contains(text, "impeccable.cmd") {
				t.Fatalf("missing commandWindows in %s", jsonStringify(entry, ""))
			}
		}
	}
	for _, manifest := range without {
		for _, entry := range hookEntries(manifest) {
			if jget(entry, "commandWindows") != jsUndefined {
				t.Fatalf("unexpected commandWindows in %s", jsonStringify(entry, ""))
			}
		}
	}
	nodeEra := regexp.MustCompile(`node|systemMessage|node-unsupported`)
	for _, manifest := range append(withWindows, without...) {
		for _, entry := range hookEntries(manifest) {
			for _, key := range []string{"command", "bash"} {
				if text, ok := jget(entry, key).(string); ok && nodeEra.MatchString(text) {
					t.Fatalf("Node-era fragment in %s", text)
				}
			}
		}
	}
}

func TestHookManifestBuildersRoutesSupportedHookBuildersAndLeavesOtherProvidersAlone(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "cursor", "github", "grok", "gemini"} {
		if hooksJSONFor(provider, "") == nil {
			t.Errorf("no manifest for %s", provider)
		}
	}
	for _, provider := range []string{"omp", "devin", "hermes", "unrecognized"} {
		if hooksJSONFor(provider, "") != nil {
			t.Errorf("manifest built for %s", provider)
		}
	}
}

func TestHookManifestBuildersEmitsTheHermesDesignHookPluginVerbatimUnderHermesPlugins(t *testing.T) {
	hermes, _ := impeccableProviderByName("hermes")
	if hermes.EmitHooks != "" {
		t.Fatalf("hermes emits hooks %q", hermes.EmitHooks)
	}
	distDir := t.TempDir()
	if err := hermes.transform(readSourceSkills(t, impeccablePayloadRoot), distDir, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(distDir, "hermes/.hermes/plugins/impeccable-design-hook")
	if names := dirNames(t, pluginDir); !slices.Equal(names, []string{"__init__.py", "plugin.yaml"}) {
		t.Fatalf("plugin files %v", names)
	}
	for _, name := range []string{"__init__.py", "plugin.yaml"} {
		if readTestFile(t, filepath.Join(pluginDir, name)) != readTestFile(t, impeccableRepoPath("skill/hermes-plugin/"+name)) {
			t.Errorf("%s differs from skill/hermes-plugin", name)
		}
	}
	manifest := readTestFile(t, filepath.Join(pluginDir, "plugin.yaml"))
	if !regexp.MustCompile(`(?m)^name: impeccable-design-hook$`).MatchString(manifest) ||
		!regexp.MustCompile(`(?m)^provides_hooks:\n  - transform_tool_result\n  - pre_verify$`).MatchString(manifest) {
		t.Fatalf("plugin.yaml\n%s", manifest)
	}
}

// Skips like upstream while the tracked provider output still predates the launcher
func requireSyncedProviderOutput(t *testing.T) {
	t.Helper()
	if !pathExists(filepath.Join(impeccableRepoRoot, ".claude/skills/impeccable/scripts/impeccable")) {
		t.Skip("generated provider output not yet synced (bun run build:release on main)")
	}
}

func repoPath(rel string) string {
	return impeccableRepoPath(rel)
}

func TestGeneratedHookArtifactsInRepoExistsAndIsValidJSON(t *testing.T) {
	requireSyncedProviderOutput(t)
	for _, rel := range []string{".claude/settings.json", ".cursor/hooks.json", ".codex/hooks.json", ".github/hooks/impeccable.json"} {
		t.Run(rel, func(t *testing.T) {
			if !pathExists(repoPath(rel)) {
				t.Fatalf("%s missing - did you forget bun run build?", rel)
			}
			repoJSON(t, rel)
		})
	}
}

func TestGeneratedHookArtifactsInRepoRootHookManifestsExactlyMatchTheHookBuilders(t *testing.T) {
	requireSyncedProviderOutput(t)
	if !jsonDeepEqual(repoJSON(t, ".claude/settings.json"), hookManifest(t, buildClaudeSettingsManifest())) {
		t.Error(".claude/settings.json differs from the builder")
	}
	cursor := repoJSON(t, ".cursor/hooks.json")
	if !jsonTruthy(jsonOptionalProp(jsonOptionalProp(cursor, "hooks"), "preToolUse")) && !jsonDeepEqual(cursor, hookManifest(t, buildCursorHooksManifest(cursorHookScripts))) {
		t.Error(".cursor/hooks.json differs from the builder")
	}
	if !jsonDeepEqual(repoJSON(t, ".codex/hooks.json"), hookManifest(t, buildCodexHooksManifest(".codex"))) {
		t.Error(".codex/hooks.json differs from the builder")
	}
	if !jsonDeepEqual(repoJSON(t, ".github/hooks/impeccable.json"), hookManifest(t, buildGitHubHooksManifest())) {
		t.Error(".github/hooks/impeccable.json differs from the builder")
	}
}

func TestGeneratedHookArtifactsInRepoClaudeProjectSettingsReferenceTheLauncherInClaudeSkills(t *testing.T) {
	requireSyncedProviderOutput(t)
	manifest := repoJSON(t, ".claude/settings.json")
	expectHookCommand(t, jget(manifest, "hooks", "PostToolUse", 0, "hooks", 0, "command"), ".claude/skills/impeccable/scripts", "hook")
	wantExists(t, repoPath(".claude/skills/impeccable/scripts"))
}

func TestGeneratedHookArtifactsInRepoCursorProjectHooksReferenceTheLauncherInCursorSkills(t *testing.T) {
	requireSyncedProviderOutput(t)
	manifest := repoJSON(t, ".cursor/hooks.json")
	events := jkeys(jget(manifest, "hooks"))
	if len(events) != 1 || !slices.Contains([]string{"postToolUse", "preToolUse"}, events[0]) {
		t.Fatalf("events %v", events)
	}
	verb := "hook-before-edit"
	if events[0] == "postToolUse" {
		verb = "hook"
	}
	expectHookCommand(t, jget(manifest, "hooks", events[0], 0, "command"), ".cursor/skills/impeccable/scripts", verb)
	wantExists(t, repoPath(".cursor/skills/impeccable/scripts/impeccable"))
	if pathExists(repoPath(".cursor/skills/impeccable/scripts/hook-before-edit.mjs")) {
		t.Fatal("hook-before-edit.mjs still shipped")
	}
}

func TestGeneratedHookArtifactsInRepoCodexProjectHooksReferenceTheLauncherInTheCodexSkillPayload(t *testing.T) {
	requireSyncedProviderOutput(t)
	manifest := repoJSON(t, ".codex/hooks.json")
	command := jget(manifest, "hooks", "PostToolUse", 0, "hooks", 0, "command")
	expectHookCommand(t, command, ".codex/skills/impeccable/scripts", "hook")
	wantLacks(t, command.(string), ".agents/skills")
	wantExists(t, repoPath(".agents/skills/impeccable/SKILL.md"), repoPath(".agents/skills/impeccable/scripts"))
}

func TestGeneratedHookArtifactsInRepoGitHubCopilotRepoHooksReferenceTheLauncherInTheGithubSkillPayload(t *testing.T) {
	requireSyncedProviderOutput(t)
	entry := jget(repoJSON(t, ".github/hooks/impeccable.json"), "hooks", "postToolUse", 0)
	wantJSON(t, jget(entry, "matcher"), "edit|create|apply_patch")
	expectHookCommand(t, jget(entry, "bash"), ".github/skills/impeccable/scripts", "hook")
	wantExists(t, repoPath(".github/skills/impeccable/SKILL.md"), repoPath(".github/skills/impeccable/scripts"))
}

func TestGeneratedHookArtifactsInRepoDoesNotGenerateProbeScriptsIntoProviderSkillPayloads(t *testing.T) {
	requireSyncedProviderOutput(t)
	for _, providerDir := range []string{".claude", ".cursor", ".agents", "plugin"} {
		if pathExists(impeccableRepoPath(providerDir + "/skills/impeccable/scripts/hook-probe.mjs")) {
			t.Errorf("%s still has hook-probe.mjs", providerDir)
		}
	}
}

func TestGeneratedHookArtifactsInRepoDoesNotGenerateStaleCodexHookPackagingArtifacts(t *testing.T) {
	requireSyncedProviderOutput(t)
	for _, rel := range []string{".claude/hooks/hooks.json", ".agents/hooks", ".agents/plugins/marketplace.json", "plugin/.codex-plugin", "plugin/assets", "plugin-codex"} {
		if pathExists(repoPath(rel)) {
			t.Errorf("%s should not exist", rel)
		}
	}
}

func TestGeneratedHookArtifactsInRepoPackagesTheClaudeDesignHookInThePluginViaPluginRootPaths(t *testing.T) {
	requireSyncedProviderOutput(t)
	if !pathExists(repoPath("plugin/hooks/hooks.json")) {
		t.Fatal("plugin/hooks/hooks.json missing - did you forget bun run build:release?")
	}
	manifest := repoJSON(t, "plugin/hooks/hooks.json")
	if !jsonDeepEqual(manifest, hookManifest(t, buildClaudePluginHooksManifest())) {
		t.Fatal("plugin/hooks/hooks.json differs from the builder")
	}
	wantUndefined(t, jget(manifest, "description"))
	handler := jget(manifest, "hooks", "PostToolUse", 0, "hooks", 0)
	wantJSON(t, jget(manifest, "hooks", "PostToolUse", 0, "matcher"), "Edit|Write")
	command, _ := jget(handler, "command").(string)
	if !strings.Contains(command, "${CLAUDE_PLUGIN_ROOT}") || strings.Contains(command, "${CLAUDE_PROJECT_DIR}") {
		t.Fatalf("plugin hook command %s", command)
	}
	stop := jget(manifest, "hooks", "Stop", 0, "hooks", 0)
	wantJSON(t, jget(stop, "timeout"), 30)
	wantJSON(t, jget(stop, "command"), command)
	wantExists(t, repoPath("plugin/skills/impeccable/scripts"))
}

func TestGeneratedHookArtifactsInRepoGeneratedSkillPayloadsShipTheExecutableLauncherAndNoNodeScripts(t *testing.T) {
	requireSyncedProviderOutput(t)
	for _, scriptDir := range []string{".claude/skills/impeccable/scripts", ".cursor/skills/impeccable/scripts", ".agents/skills/impeccable/scripts", "plugin/skills/impeccable/scripts"} {
		abs := repoPath(scriptDir)
		info, err := os.Stat(filepath.Join(abs, "impeccable"))
		if err != nil {
			t.Fatalf("launcher missing in %s", scriptDir)
		}
		if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			t.Errorf("launcher not executable in %s", scriptDir)
		}
		wantExists(t, filepath.Join(abs, "impeccable.cmd"), filepath.Join(abs, "VERSION"))
		if pathExists(filepath.Join(abs, "bin")) {
			t.Errorf("%s must stay launcher-only in git; binaries ship only in IMPECCABLE_BUNDLE_ENGINE=1 release zips", scriptDir)
		}
		for _, name := range dirNames(t, abs) {
			if strings.HasSuffix(name, ".mjs") || name == "detector" || name == "lib" {
				t.Errorf("Node-era file %s still in %s", name, scriptDir)
			}
		}
	}
}
