package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var tvKnown = map[string]bool{"icebath-cli": true, "icebath-telegram": true, "icebath-discord": true, "icebath-whatsapp": true, "discord": true, "terminal": true, "web": true}

func tvIsValid(name string) bool { return tvKnown[name] }

func tvAny(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		all := true
		for _, p := range parts {
			if !strings.Contains(w, p) {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

func tvOrdered(pairs ...any) *pyjson.Object {
	o := &pyjson.Object{}
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func tvValidate(v any) []string {
	return ValidatePlatformToolsetsWith(v, tvIsValid, ToolsetAllowedForPlatform)
}

func TestToolsetValidation38798CorruptionWarnsAndSuggestsCorrectName(t *testing.T) {
	warnings := ValidatePlatformToolsets(map[string]any{"cli": []any{"hermes"}}, tvIsValid)
	var unknown []string
	for _, w := range warnings {
		if strings.Contains(w, "unknown toolset 'hermes'") {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) != 1 || !strings.Contains(unknown[0], "did you mean 'icebath-cli'?") || !tvAny(warnings, "zero valid toolsets") {
		t.Fatalf("warnings %q", warnings)
	}
}

func TestToolsetValidationMixedValidAndInvalidFlagsOnlyTheInvalid(t *testing.T) {
	w := tvValidate(tvOrdered("cli", []any{"icebath-cli"}, "discord", []any{"bogus"}))
	if tvAny(w, "zero valid toolsets") || !tvAny(w, "platform 'discord'", "unknown toolset 'bogus'") || !tvAny(w, "platform 'discord'", "no valid toolsets") {
		t.Fatalf("warnings %q", w)
	}
}

func TestToolsetValidationEmptyLists(t *testing.T) {
	empties := func(w []string) []string {
		var out []string
		for _, s := range w {
			if strings.Contains(s, "empty toolset list") {
				out = append(out, s)
			}
		}
		return out
	}
	w := tvValidate(tvOrdered("cli", []any{}, "telegram", []any{"icebath-telegram"}, "discord", []any{"icebath-discord"}))
	e := empties(w)
	if len(e) != 1 || !strings.Contains(e[0], "platform 'cli'") || !strings.Contains(e[0], "no tools") || strings.Contains(e[0], "telegram") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("one empty platform: %q", w)
	}
	w = tvValidate(tvOrdered("cli", []any{}, "discord", []any{}, "telegram", []any{"icebath-telegram"}))
	if e := empties(w); len(e) != 2 || !tvAny(e, "platform 'cli'") || !tvAny(e, "platform 'discord'") {
		t.Fatalf("two empty platforms: %q", w)
	}
	w = tvValidate(tvOrdered("cli", []any{}, "discord", []any{"bogus"}))
	if !tvAny(w, "empty toolset list", "platform 'cli'") || !tvAny(w, "unknown toolset 'bogus'") || !tvAny(w, "zero valid toolsets") {
		t.Fatalf("empty does not mask unknown: %q", w)
	}
}

func TestToolsetValidationNullAndScalarValuesFallBack(t *testing.T) {
	w := tvValidate(tvOrdered("cli", nil, "telegram", []any{"icebath-telegram"}))
	if !tvAny(w, "platform 'cli'", "null toolset value", "falling back to 'icebath-cli'") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("null: %q", w)
	}
	w = tvValidate(tvOrdered("whatsapp_cloud", nil, "telegram", []any{"icebath-telegram"}))
	if !tvAny(w, "platform 'whatsapp_cloud'", "falling back to 'icebath-whatsapp'") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("alias: %q", w)
	}
	w = tvValidate(tvOrdered("cli", "bogus", "telegram", []any{"icebath-telegram"}))
	if !tvAny(w, "platform 'cli'", "invalid toolset value 'bogus'", "falling back to 'icebath-cli'") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("scalar: %q", w)
	}
	for raw, name := range map[any]string{5: "int", 1.5: "float", true: "bool"} {
		if w := tvValidate(map[string]any{"cli": raw}); !tvAny(w, "has invalid "+name+" toolset value — falling back to 'icebath-cli'. Run `icebath tools` to configure explicitly.") {
			t.Fatalf("%v: %q", raw, w)
		}
	}
	if w := tvValidate(map[string]any{"cli": map[string]any{}}); !tvAny(w, "invalid dict toolset value") {
		t.Fatalf("dict: %q", w)
	}
}

func TestToolsetValidationListLiteralStringIsValidatedAsTheListItEncodes(t *testing.T) {
	if w := tvValidate(map[string]any{"cli": `["web", "terminal"]`}); len(w) != 0 {
		t.Fatalf("valid literal: %q", w)
	}
	w := tvValidate(map[string]any{"cli": `["web", "nope"]`})
	if !tvAny(w, "platform 'cli' references unknown toolset 'nope'") || tvAny(w, "falling back") {
		t.Fatalf("literal: %q", w)
	}
}

func TestToolsetValidationPlatformRestrictedToolsetWarns(t *testing.T) {
	w := tvValidate(tvOrdered("telegram", []any{"discord"}, "cli", []any{"icebath-cli"}))
	if !tvAny(w, "platform 'telegram'", "toolset 'discord'", "not available") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("warnings %q", w)
	}
}

func TestToolsetValidationNullPluginPlatformUsesSyntheticDefault(t *testing.T) {
	const platform = "toolset_validation_plugin"
	saved := RegisteredPlatformNames
	t.Cleanup(func() { RegisteredPlatformNames = saved })
	RegisteredPlatformNames = func() []string { return []string{platform} }
	w := tvValidate(tvOrdered(platform, nil, "telegram", []any{"icebath-telegram"}))
	if len(Default.ResolveToolset(context.Background(), "", "icebath-"+platform)) == 0 {
		t.Fatal("synthetic bundle did not resolve")
	}
	if !tvAny(w, "platform '"+platform+"'", "falling back to 'icebath-"+platform+"'") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("warnings %q", w)
	}
}

func TestToolsetValidationAllInvalidAndPopulatedPlatforms(t *testing.T) {
	w := tvValidate(tvOrdered("cli", []any{"bogus"}, "telegram", []any{"icebath-telegram"}))
	if !tvAny(w, "unknown toolset 'bogus'") || !tvAny(w, "platform 'cli'", "no valid toolsets") || tvAny(w, "zero valid toolsets") {
		t.Fatalf("all invalid: %q", w)
	}
	if w := tvValidate(tvOrdered("cli", []any{"icebath-cli"}, "telegram", []any{"icebath-telegram"})); len(w) != 0 {
		t.Fatalf("populated: %q", w)
	}
}

func TestToolsetValidationOrderAndNonStringEntries(t *testing.T) {
	w := tvValidate(tvOrdered("telegram", []any{"bogus"}, "cli", []any{123, ""}))
	want := []string{
		"platform 'telegram' references unknown toolset 'bogus' — did you mean 'icebath-telegram'?",
		"platform 'telegram' has no valid toolsets configured — " + noToolsSuffix,
		"platform 'cli' has no valid toolsets configured — " + noToolsSuffix,
		"platform_toolsets resolves to zero valid toolsets — the agent will have no tools. Run `icebath tools` to reconfigure.",
	}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("got %q", w)
	}
	if w := tvValidate([]any{"cli"}); len(w) != 0 {
		t.Fatalf("non mapping: %q", w)
	}
}

var (
	tvTerminal = UnavailableToolset{Name: "terminal", Tools: []string{"terminal", "process_manage"}}
	tvWeb      = UnavailableToolset{Name: "web", EnvVars: []string{"EXA_API_KEY", "TAVILY_API_KEY"}, Tools: []string{"web_search"}}
	tvRL       = UnavailableToolset{Name: "rl", EnvVars: []string{"TINKER_API_KEY"}, Tools: []string{"rl_tool"}}
)

func TestToolAvailabilityWarningLines(t *testing.T) {
	text := strings.Join(ToolAvailabilityWarningLines([]UnavailableToolset{tvTerminal}, "Docker is installed but the Docker daemon is not running", "docker"), "\n")
	for _, part := range []string{"Terminal tool disabled", "'docker'", "Docker daemon is not running", "icebath doctor", "icebath setup terminal"} {
		if !strings.Contains(text, part) || strings.Contains(text, "TERMINAL_ENV") {
			t.Fatalf("terminal line %q lacks %q", text, part)
		}
	}
	if !tvAny(ToolAvailabilityWarningLines([]UnavailableToolset{tvTerminal}, "", "ssh"), "Terminal tool disabled", "'ssh'") {
		t.Fatal("terminal without reason")
	}
	text = strings.Join(ToolAvailabilityWarningLines([]UnavailableToolset{tvWeb}, "", "local"), "\n")
	if strings.Contains(text, "EXA_API_KEY") || !strings.Contains(text, "Web search is off") || !strings.Contains(text, "icebath setup tools") {
		t.Fatalf("web line %q", text)
	}
	got := ToolAvailabilityWarningLines([]UnavailableToolset{tvRL}, "", "local")
	want := []string{"[yellow]⚠️  Some tools disabled (missing API keys):[/]", "   [dim]• rl[/] [dim italic](TINKER_API_KEY)[/]", "[dim]   Run 'icebath setup tools' to configure[/]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generic %q", got)
	}
	if len(ToolAvailabilityWarningLines(nil, "", "local")) != 0 || len(ToolAvailabilityWarningLines([]UnavailableToolset{{Name: "honcho"}}, "", "local")) != 0 {
		t.Fatal("expected no lines")
	}
}

func TestFilterToEnabledToolsets(t *testing.T) {
	bundle := map[string][]string{"icebath-cli": {"terminal", "process_manage", "web_search", "read_file"}, "terminal": {"terminal", "process_manage"}}
	resolve := func(n string) ([]string, error) { return bundle[n], nil }
	names := func(items []UnavailableToolset) []string {
		out := []string{}
		for _, i := range items {
			out = append(out, i.Name)
		}
		return out
	}
	if got := names(FilterToEnabledToolsets([]UnavailableToolset{tvTerminal, tvWeb, tvRL}, []string{"icebath-cli"}, resolve)); !reflect.DeepEqual(got, []string{"terminal", "web"}) {
		t.Fatalf("composite %q", got)
	}
	if got := names(FilterToEnabledToolsets([]UnavailableToolset{tvTerminal, tvRL}, []string{"terminal"}, resolve)); !reflect.DeepEqual(got, []string{"terminal"}) {
		t.Fatalf("direct %q", got)
	}
	if got := FilterToEnabledToolsets([]UnavailableToolset{tvTerminal}, []string{"no-such-bundle"}, resolve); len(got) != 0 {
		t.Fatalf("unknown bundle %v", got)
	}
	if got := FilterToEnabledToolsets([]UnavailableToolset{tvTerminal, tvRL}, nil, resolve); !reflect.DeepEqual(got, []UnavailableToolset{tvTerminal, tvRL}) {
		t.Fatalf("empty selection %v", got)
	}
	boom := func(string) ([]string, error) { return nil, errors.New("registry not ready") }
	panics := func(string) ([]string, error) { panic("registry not ready") }
	if len(FilterToEnabledToolsets([]UnavailableToolset{tvTerminal}, []string{"icebath-cli"}, boom)) != 0 || len(FilterToEnabledToolsets([]UnavailableToolset{tvTerminal}, []string{"icebath-cli"}, panics)) != 0 {
		t.Fatal("resolver error must not be fatal")
	}
}

func TestMCPServerEnabledBoolishParity(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{{nil, true}, {true, true}, {false, false}, {0, false}, {1, true}, {0.0, false}, {1.5, true}, {"off", false}, {" No ", false}, {"0", false}, {"on", true}, {"", true}, {"n", true}, {"y", true}, {"maybe", true}, {[]any{}, true}}
	for _, c := range cases {
		if got := MCPServerEnabled(map[string]any{"enabled": c.v}); got != c.want {
			t.Fatalf("%#v: got %v", c.v, got)
		}
	}
	if !MCPServerEnabled(map[string]any{}) {
		t.Fatal("absent key is on")
	}
}

func TestToolsetDefinitionOrderCoversTable(t *testing.T) {
	names := orderedToolsetNames()
	if len(names) != len(staticToolsetNames()) || names[0] != "web" || names[len(names)-1] != "icebath-gateway" {
		t.Fatalf("order %v", names)
	}
}

func TestPortableMCPServerNamesSources(t *testing.T) {
	savedCat, savedGw := PluginToolsetCatalog, GatewayPluginToolsets
	t.Cleanup(func() { PluginToolsetCatalog, GatewayPluginToolsets = savedCat, savedGw })
	ctx := context.Background()
	GatewayPluginToolsets = nil
	PluginToolsetCatalog.PortableNowait = func(context.Context) map[string]bool { return map[string]bool{"cat": true} }
	if got := portableMCPServerNames(ctx); !got["cat"] {
		t.Fatalf("catalog %v", got)
	}
	GatewayPluginToolsets = func() ([]ConfigurableToolset, map[string]bool, bool) { return nil, map[string]bool{"proj": true}, true }
	if got := portableMCPServerNames(ctx); !got["proj"] || got["cat"] {
		t.Fatalf("projection %v", got)
	}
	GatewayPluginToolsets = func() ([]ConfigurableToolset, map[string]bool, bool) { panic("boom") }
	if got := portableMCPServerNames(ctx); len(got) != 0 {
		t.Fatalf("failure %v", got)
	}
}
