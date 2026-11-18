package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestImpeccableClaudePluginHooksMapOntoDesignHook(t *testing.T) {
	raw, err := payloadFile("impeccable/plugin/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Description *string `json:"description"`
		Hooks       map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string  `json:"command"`
				Timeout float64 `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Description != nil {
		t.Fatalf("hooks.json %v %s", err, raw)
	}
	post, stop := manifest.Hooks["PostToolUse"][0], manifest.Hooks["Stop"][0].Hooks[0]
	handler := post.Hooks[0]
	if post.Matcher != "Edit|Write" || !strings.Contains(handler.Command, "${CLAUDE_PLUGIN_ROOT}") || strings.Contains(handler.Command, "${CLAUDE_PROJECT_DIR}") {
		t.Fatalf("PostToolUse %+v", post)
	}
	if stop.Command != handler.Command || time.Duration(stop.Timeout)*time.Second != impeccableStopTimeout {
		t.Fatalf("Stop %+v, design hook stop timeout %v", stop, impeccableStopTimeout)
	}
	var mapped []string
	for _, name := range hookToolNames {
		mapped = append(mapped, name)
	}
	sort.Strings(mapped)
	want := strings.Split(post.Matcher, "|")
	sort.Strings(want)
	if !slices.Equal(mapped, want) {
		t.Fatalf("transform_tool_result covers %v, the plugin matcher covers %v", mapped, want)
	}
}

func TestImpeccablePluginSkillShipsExecutableLauncherOnly(t *testing.T) {
	tree, err := materializeAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{tree.Payload("impeccable/plugin/skills/impeccable/scripts"), tree.Host("impeccable/skills/impeccable/scripts")} {
		info, err := os.Stat(filepath.Join(dir, "impeccable"))
		if err != nil || info.Mode()&0o111 == 0 {
			t.Errorf("launcher missing or not executable in %s: %v", dir, err)
		}
		for _, name := range []string{"impeccable.cmd", "VERSION"} {
			if !isFile(filepath.Join(dir, name)) {
				t.Errorf("%s missing in %s", name, dir)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "bin")); err == nil {
			t.Errorf("%s must stay launcher-only", dir)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mjs") || e.Name() == "detector" || e.Name() == "lib" {
				t.Errorf("Node-era file %s in %s", e.Name(), dir)
			}
		}
	}
}
