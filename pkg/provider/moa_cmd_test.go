package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var errMoANoMenu = errors.New("curses menu unavailable")

// Temp home whose picker payload lists two configured providers and whose menus fall back to scripted stdin
func moaCmdHome(t *testing.T, answers []string) (context.Context, *[]string) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), t.TempDir())
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	prevLoad, prevBuild := moaLoadPickerContextFn, moaBuildModelsPayloadFn
	t.Cleanup(func() { moaLoadPickerContextFn, moaBuildModelsPayloadFn = prevLoad, prevBuild })
	moaLoadPickerContextFn = func(context.Context) (PickerConfigContext, error) { return PickerConfigContext{}, nil }
	moaBuildModelsPayloadFn = func(context.Context, PickerConfigContext, ModelsPayloadOptions) (map[string]any, error) {
		return map[string]any{"providers": []any{
			map[string]any{"slug": "openrouter", "name": "OpenRouter", "models": []any{"a/x", "b/y"}},
			map[string]any{"slug": "anthropic", "models": []any{"claude-opus-5"}},
			map[string]any{"slug": "empty", "models": []any{}},
		}}, nil
	}
	prompts := scriptPickerTerminal(t, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) { return 0, errMoANoMenu }, answers, nil, nil)
	return ctx, prompts
}

func moaLoadedPreset(t *testing.T, ctx context.Context, name string) map[string]any {
	t.Helper()
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	return anyMap(anyMap(anyMap(tree["moa"])["presets"])[name])
}

func TestMoAConfigurePicksSlotsThroughTheFallbackPromptAndSaves(t *testing.T) {
	ctx, prompts := moaCmdHome(t, []string{"2", "", "1", "", "", "", "", "9"})
	var out strings.Builder
	if err := CmdMoA(ctx, &out, MoAArgs{Command: "configure", Name: " review "}); err != nil {
		t.Fatal(err)
	}
	preset := moaLoadedPreset(t, ctx, "review")
	wantRefs := []any{map[string]any{"provider": "anthropic", "model": "claude-opus-5", "enabled": true},
		map[string]any{"provider": "openrouter", "model": "a/x", "enabled": true}}
	if !reflect.DeepEqual(preset["reference_models"], wantRefs) || !reflect.DeepEqual(preset["aggregator"], map[string]any{"provider": "openrouter", "model": "b/y"}) {
		t.Fatalf("saved preset %v", preset)
	}
	wantPrompts := []string{"Select provider [1]: ", "Select model for anthropic [1]: ", "Add another reference model? [2]: ",
		"Select provider [1]: ", "Select model for openrouter [1]: ", "Add another reference model? [2]: ",
		"Select provider [1]: ", "Select model for openrouter [1]: "}
	if !reflect.DeepEqual(*prompts, wantPrompts) {
		t.Fatalf("prompts %q", *prompts)
	}
	for _, want := range []string{"Configure MoA preset: review", "1. OpenRouter  (openrouter)", "2. anthropic  (anthropic)",
		"Saved MoA preset: review", "* default", "  review", "    1. anthropic:claude-opus-5"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "empty") {
		t.Fatal("a provider without models is never offered")
	}
}

func TestMoAConfigureWithoutProvidersFails(t *testing.T) {
	ctx, _ := moaCmdHome(t, nil)
	moaBuildModelsPayloadFn = func(context.Context, PickerConfigContext, ModelsPayloadOptions) (map[string]any, error) {
		return map[string]any{"providers": []any{map[string]any{"slug": "moa", "models": []any{"default"}}}}, nil
	}
	err := CmdMoA(ctx, &strings.Builder{}, MoAArgs{Command: "config"})
	if err == nil || err.Error() != "No configured model providers found. Run `icebath model` first." {
		t.Fatalf("err %v", err)
	}
}

func TestMoAPromptChoiceUsesTheMenuAndClampsTypedNumbers(t *testing.T) {
	var seen PickerRadiolistOptions
	scriptPickerTerminal(t, func(_ string, _ [][]PickerSegment, opts PickerRadiolistOptions) (int, error) {
		seen = opts
		return 2, nil
	}, nil, nil, nil)
	if idx, err := moaPromptChoice(&strings.Builder{}, "t", []string{"a", "b", "c"}, 1); err != nil || idx != 2 || seen.Selected != 1 || seen.CancelReturns != 1 {
		t.Fatalf("menu choice %d %v %+v", idx, err, seen)
	}
	scriptPickerTerminal(t, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) { return 0, errMoANoMenu }, []string{"0", "abc", "+2", "99999999999999999999"}, nil, nil)
	for _, want := range []int{0, 1, 1, 2} {
		if idx, err := moaPromptChoice(&strings.Builder{}, "t", []string{"a", "b", "c"}, 1); err != nil || idx != want {
			t.Fatalf("typed choice %d want %d %v", idx, want, err)
		}
	}
	if _, err := moaPromptChoice(&strings.Builder{}, "t", []string{"a"}, 0); err == nil {
		t.Fatal("end of input propagates")
	}
}

func TestMoADeleteAndSubcommandRefusals(t *testing.T) {
	ctx, _ := moaCmdHome(t, []string{"", "", "", "", ""})
	if err := CmdMoA(ctx, &strings.Builder{}, MoAArgs{Command: "configure", Name: "review"}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := CmdMoA(ctx, &out, MoAArgs{Command: "delete", Name: "review"}); err != nil || !strings.Contains(out.String(), "Deleted MoA preset: review") {
		t.Fatalf("delete %v %s", err, out.String())
	}
	if len(moaLoadedPreset(t, ctx, "review")) != 0 {
		t.Fatal("review survived the delete")
	}
	for args, want := range map[MoAArgs]string{
		{Command: "delete"}:                  "Usage: icebath moa delete <name>",
		{Command: "delete", Name: "nope"}:    "Unknown MoA preset: nope",
		{Command: "delete", Name: "default"}: "Cannot delete the only MoA preset",
		{Command: "rm", Name: "default"}:     "Unknown moa subcommand: rm",
	} {
		var exit *MoAExitError
		if err := CmdMoA(ctx, &strings.Builder{}, args); !errors.As(err, &exit) || exit.Message != want {
			t.Fatalf("%+v got %v", args, err)
		}
	}
	out.Reset()
	if err := CmdMoA(ctx, &out, MoAArgs{}); err != nil || !strings.HasPrefix(out.String(), "Mixture of Agents presets\nDefault: default\nActive in config: (off)\n") {
		t.Fatalf("bare moa lists %v %q", err, out.String())
	}
}

func TestMoADeleteMovesDefaultAndClearsActive(t *testing.T) {
	ctx, _ := moaCmdHome(t, nil)
	cfg := config.Tree{"moa": map[string]any{"default_preset": "b", "active_preset": "b",
		"presets": map[string]any{"a": map[string]any{}, "b": map[string]any{}, "c": map[string]any{}}}}
	var out strings.Builder
	if err := moaCmdDelete(ctx, &out, cfg, MoAArgs{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	moa := anyMap(cfg["moa"])
	if moa["default_preset"] != "a" || moa["active_preset"] != "" {
		t.Fatalf("moa %v", moa)
	}
	mustEqual(t, moaFormatSlot(map[string]any{"provider": "p", "model": "m", "reasoning_effort": " high "}), "p:m [reasoning=high]", "slot label")
	mustEqual(t, moaProviderMismatchNotice(map[string]any{"model": map[string]any{"provider": "auto"}}, map[string]any{"provider": "x"}), "", "auto main")
}
