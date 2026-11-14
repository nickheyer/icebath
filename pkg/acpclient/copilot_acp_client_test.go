package acpclient

import (
	"slices"
	"strings"
	"testing"
)

func TestCopilotDisabledReadsEnablementMeta(t *testing.T) {
	cases := []struct {
		entry any
		want  bool
	}{
		{map[string]any{"value": "m", "_meta": map[string]any{"copilotEnablement": " Disabled "}}, true},
		{map[string]any{"value": "m", "_meta": map[string]any{"copilotEnablement": "enabled"}}, false},
		{map[string]any{"value": "m", "_meta": nil}, false},
		{map[string]any{"value": "m"}, false},
	}
	for _, c := range cases {
		got, err := copilotDisabled(c.entry)
		if err != nil || got != c.want {
			t.Fatalf("%v: got %v %v", c.entry, got, err)
		}
	}
	if _, err := copilotDisabled(map[string]any{"_meta": "x"}); err == nil || !strings.Contains(err.Error(), "'str' object has no attribute 'get'") {
		t.Fatalf("non dict meta: %v", err)
	}
}

func TestCopilotEarlyExitNeedsProductAndMarker(t *testing.T) {
	banner := "gh-copilot: This extension HAS BEEN DEPRECATED"
	got, ok := copilotEarlyExitError(banner)
	if !ok || got != ghExtensionError+banner || !strings.HasPrefix(got, "Icebath ACP mode requires the NEW GitHub Copilot CLI") {
		t.Fatalf("got %q %v", got, ok)
	}
	for _, text := range []string{"copilot-cli has been deprecated", "gh-copilot started", ""} {
		if _, ok := copilotEarlyExitError(text); ok {
			t.Fatalf("%q diagnosed as the gh extension", text)
		}
	}
}

func TestCopilotVendorKeepsFormerDefaults(t *testing.T) {
	v := CopilotVendor()
	if v.DefaultCommand != "copilot" || !slices.Equal(v.DefaultArgs, []string{"--acp", "--stdio"}) || v.MarkerBaseURL != "acp://copilot" ||
		v.PlaceholderModel != "copilot-acp" || v.Label != "Copilot ACP" || v.AuthRequiredHint != nil ||
		!slices.Equal(v.CommandEnvVars, []string{"ICEBATH_COPILOT_ACP_COMMAND", "COPILOT_CLI_PATH"}) || v.ArgsEnvVar != "ICEBATH_COPILOT_ACP_ARGS" {
		t.Fatalf("vendor %+v", v)
	}
	if off, err := v.ModelEntryDisabled(map[string]any{"_meta": map[string]any{"copilotEnablement": "disabled"}}); !off || err != nil {
		t.Fatalf("filter %v %v", off, err)
	}
}

func TestCopilotResolveCommandAndArgs(t *testing.T) {
	t.Setenv("ICEBATH_COPILOT_ACP_COMMAND", "  ")
	t.Setenv("COPILOT_CLI_PATH", "")
	t.Setenv("ICEBATH_COPILOT_ACP_ARGS", "")
	if got := CopilotResolveCommand(); got != "copilot" {
		t.Fatalf("command %q", got)
	}
	if got, err := CopilotResolveArgs(); err != nil || !slices.Equal(got, []string{"--acp", "--stdio"}) {
		t.Fatalf("args %v %v", got, err)
	}
	t.Setenv("COPILOT_CLI_PATH", " /opt/copilot ")
	t.Setenv("ICEBATH_COPILOT_ACP_ARGS", `--acp --flag "a b"`)
	if got := CopilotResolveCommand(); got != "/opt/copilot" {
		t.Fatalf("command %q", got)
	}
	if got, err := CopilotResolveArgs(); err != nil || !slices.Equal(got, []string{"--acp", "--flag", "a b"}) {
		t.Fatalf("args %v %v", got, err)
	}
	t.Setenv("ICEBATH_COPILOT_ACP_COMMAND", "/usr/bin/new-copilot")
	if got := CopilotResolveCommand(); got != "/usr/bin/new-copilot" {
		t.Fatalf("command %q", got)
	}
}

func TestCopilotModelFiltersDropDisabledEntries(t *testing.T) {
	session := map[string]any{"sessionId": "s1", "configOptions": []any{map[string]any{"id": "model", "category": "model", "options": []any{
		map[string]any{"value": "m-a"}, map[string]any{"value": "m-off", "_meta": map[string]any{"copilotEnablement": "disabled"}}}}}}
	ids, err := CopilotSessionModelIDs(session)
	if err != nil || !slices.Equal(ids, []string{"m-a"}) {
		t.Fatalf("ids %v %v", ids, err)
	}
	if _, _, ok, err := CopilotModelSelectionRequest(session, "m-off"); ok || err != nil {
		t.Fatalf("disabled model selected %v", err)
	}
	if _, _, ok, err := CopilotModelSelectionRequest(session, "copilot-acp"); ok || err != nil {
		t.Fatalf("placeholder selected %v", err)
	}
	method, params, ok, err := CopilotModelSelectionRequest(session, " m-a ")
	if !ok || err != nil || method != "session/set_config_option" {
		t.Fatalf("method %q %v %v", method, ok, err)
	}
	if v, _ := params.Get("value"); v != "m-a" {
		t.Fatalf("params %v", params)
	}
	legacy := map[string]any{"models": map[string]any{"availableModels": []any{
		map[string]any{"modelId": "fast"}, map[string]any{"modelId": "gone", "_meta": map[string]any{"copilotEnablement": "DISABLED"}}}}}
	if ids, err := CopilotLegacySessionModelIDs(legacy); err != nil || !slices.Equal(ids, []string{"fast"}) {
		t.Fatalf("legacy %v %v", ids, err)
	}
	entries := []any{map[string]any{"value": "x"}, "skip", map[string]any{"value": "x"}, map[string]any{"value": "y", "_meta": map[string]any{"copilotEnablement": "disabled"}}}
	if ids, err := CopilotEnabledIDList(entries, "value"); err != nil || !slices.Equal(ids, []string{"x"}) {
		t.Fatalf("enabled %v %v", ids, err)
	}
	if _, err := CopilotEnabledIDList([]any{map[string]any{"value": "z", "_meta": []any{1}}}, "value"); err == nil {
		t.Fatal("non dict meta must raise")
	}
}
