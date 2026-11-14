package builtin

import (
	"regexp"
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
