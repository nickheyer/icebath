package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fresh home with a skins dir and the process skin state reset around the test
func skinHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	dir := filepath.Join(h, "skins")
	os.MkdirAll(dir, 0o755)
	reset := func() {
		skinStateMu.Lock()
		activeSkin, activeSkinName, activeSkinsByHome = nil, "default", map[string]skinEntry{}
		skinStateMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	return context.Background(), dir
}

func TestSetActiveSkin(t *testing.T) {
	ctx, _ := skinHome(t)
	if skin := SetActiveSkin(ctx, "ares"); skin.Name != "ares" {
		t.Fatal(skin.Name)
	}
	if GetActiveSkinName(ctx) != "ares" || GetActiveSkin(ctx).Name != "ares" {
		t.Fatal(GetActiveSkinName(ctx))
	}
}

func TestLoadUserSkinFromYAML(t *testing.T) {
	ctx, dir := skinHome(t)
	os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte("branding:\n  agent_name: Custom Agent\ncolors:\n  banner_title: '#FF0000'\ndescription: A custom test skin\nname: custom\ntool_prefix: \"\\u25B8\"\n"), 0o644)
	skin := LoadSkin(ctx, "custom")
	if skin.Name != "custom" || skin.GetColor("banner_title", "") != "#FF0000" || skin.GetBranding("agent_name", "") != "Custom Agent" {
		t.Fatal(skin)
	}
	if skin.ToolPrefix != "▸" || skin.GetColor("banner_border", "") != "#CD7F32" {
		t.Fatal(skin.ToolPrefix, skin.GetColor("banner_border", ""))
	}
}

func TestLoadUserSkinInvalidSectionTypesFallBackToDefaults(t *testing.T) {
	ctx, dir := skinHome(t)
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("branding:\n- also\n- invalid\ncolors:\n- not\n- a\n- mapping\nname: broken\nspinner: invalid\ntool_emojis:\n- invalid\ntool_prefix: '!'\n"), 0o644)
	skin := LoadSkin(ctx, "broken")
	if skin.Name != "broken" || skin.GetColor("banner_title", "") != "#FFD700" || skin.GetBranding("agent_name", "") != "Icebath Agent" {
		t.Fatal(skin.Name, skin.GetColor("banner_title", ""), skin.GetBranding("agent_name", ""))
	}
	if _, ok := skin.Spinner["waiting_faces"]; ok || len(skin.ToolEmojis) != 0 || skin.ToolPrefix != "!" {
		t.Fatal(skin.Spinner, skin.ToolEmojis, skin.ToolPrefix)
	}
}

func TestListSkinsIncludesUserSkins(t *testing.T) {
	ctx, dir := skinHome(t)
	os.WriteFile(filepath.Join(dir, "pirate.yaml"), []byte("description: Arr matey\nname: pirate\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "shadow.yaml"), []byte("description: shadows ares\nname: ares\n"), 0o644)
	skins := ListSkins(ctx)
	if skins[0].Name != "default" || skins[0].Source != "builtin" {
		t.Fatal(skins[0])
	}
	found, ares := false, 0
	for _, s := range skins {
		if s.Name == "pirate" {
			found = s.Source == "user" && s.Description == "Arr matey"
		}
		if s.Name == "ares" {
			ares++
		}
	}
	if !found || ares != 1 {
		t.Fatal(skins)
	}
}

func TestUserSkinCustomCSS(t *testing.T) {
	ctx, dir := skinHome(t)
	write := func(css string) *SkinConfig {
		body := "colors:\n  background: '#101010'\nname: styled\n"
		if css != "" {
			body += "customCSS: " + css + "\n"
		}
		os.WriteFile(filepath.Join(dir, "styled.yaml"), []byte(body), 0o644)
		return LoadSkin(ctx, "styled")
	}
	if got := write("'.chat-input { font-size: 16px; }'").CustomCSS; got != ".chat-input { font-size: 16px; }" {
		t.Fatal(got)
	}
	if got := write(`"\n  .status-bar { background: black; }  \n"`).CustomCSS; got != ".status-bar { background: black; }" {
		t.Fatal(got)
	}
	if got := write("").CustomCSS; got != "" {
		t.Fatal(got)
	}
	if got := write(strings.Repeat("x", 40000)).CustomCSS; len(got) != 32768 {
		t.Fatal(len(got))
	}
	if LoadSkin(ctx, "default").CustomCSS != "" || LoadSkin(ctx, "mono").CustomCSS != "" {
		t.Fatal("built in skins carry no css")
	}
}

func TestUnknownSkinFallsBackToDefault(t *testing.T) {
	ctx, _ := skinHome(t)
	if skin := LoadSkin(ctx, "no-such-skin"); skin.Name != "default" {
		t.Fatal(skin.Name)
	}
	if wings := LoadSkin(ctx, "ares").GetSpinnerWings(); len(wings) == 0 || wings[0] != [2]string{"⟪⚔", "⚔⟫"} {
		t.Fatal(wings)
	}
}

func TestPromptToolkitStyleOverridesCoverTUIClasses(t *testing.T) {
	ctx, _ := skinHome(t)
	SetActiveSkin(ctx, "ares")
	overrides := GetPromptToolkitStyleOverrides(ctx)
	for _, cls := range []string{"input-area", "placeholder", "prompt", "prompt-working", "hint", "status-bar", "status-bar-strong",
		"status-bar-dim", "status-bar-good", "status-bar-warn", "status-bar-bad", "status-bar-critical", "input-rule", "image-badge",
		"completion-menu", "completion-menu.completion", "completion-menu.completion.current", "completion-menu.meta.completion",
		"completion-menu.meta.completion.current", "voice-status", "voice-status-recording", "clarify-border", "clarify-title",
		"clarify-question", "clarify-choice", "clarify-selected", "clarify-active-other", "clarify-countdown", "sudo-prompt",
		"sudo-border", "sudo-title", "sudo-text", "approval-border", "approval-title", "approval-desc", "approval-cmd",
		"approval-choice", "approval-selected"} {
		if _, ok := overrides[cls]; !ok {
			t.Fatal(cls)
		}
	}
}

func TestPromptToolkitStyleOverridesUseSkinColors(t *testing.T) {
	ctx, _ := skinHome(t)
	SetActiveSkin(ctx, "ares")
	skin := GetActiveSkin(ctx)
	c := func(k string) string { return skin.GetColor(k, "") }
	o := GetPromptToolkitStyleOverrides(ctx)
	for cls, want := range map[string]string{
		"prompt": c("prompt"), "input-rule": c("input_rule"),
		"status-bar":               "bg:" + c("status_bar_bg") + " " + c("status_bar_text"),
		"status-bar-strong":        "bg:" + c("status_bar_bg") + " " + c("status_bar_strong") + " bold",
		"status-bar-session-title": "bg:" + c("status_bar_strong") + " " + c("status_bar_bg") + " bold",
		"status-bar-critical":      "bg:" + c("status_bar_bg") + " " + c("status_bar_critical") + " bold",
	} {
		if o[cls] != want {
			t.Fatalf("%s %q want %q", cls, o[cls], want)
		}
	}
}

func TestBrandingHelpersAndConfigInit(t *testing.T) {
	ctx, _ := skinHome(t)
	InitSkinFromConfig(ctx, map[string]any{"display": map[string]any{"skin": "  mono  "}})
	if GetActiveSkinName(ctx) != "mono" || GetActiveHelpHeader(ctx, "x") != "[?] Available Commands" {
		t.Fatal(GetActiveSkinName(ctx), GetActiveHelpHeader(ctx, "x"))
	}
	InitSkinFromConfig(ctx, map[string]any{"display": map[string]any{"skin": 3}})
	if GetActiveSkinName(ctx) != "default" || GetActivePromptSymbol(ctx, "❯") != "❯ " || GetActiveGoodbye(ctx, "") != "Goodbye! ☤" {
		t.Fatal(GetActiveSkinName(ctx), GetActivePromptSymbol(ctx, "❯"))
	}
}
