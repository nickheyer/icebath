package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated icebath home and a context bound to it, the hermes isolated_home fixture
func fallbackTestHome(t *testing.T) (string, context.Context) {
	t.Helper()
	h := newTestHome(t)
	return h, home.WithOverride(context.Background(), h)
}

// Picker double writing the selection to config model through load and save like the real picker
func fallbackPickerWriting(t *testing.T, h string, model any, extra map[string]any) func(ctx context.Context, ns map[string]any) error {
	t.Helper()
	return func(ctx context.Context, ns map[string]any) error {
		cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
		if err != nil {
			return err
		}
		cfg["model"] = model
		for k, v := range extra {
			cfg[k] = v
		}
		return config.Save(h, cfg)
	}
}

// Swaps the fallback picker and the terminal check for one test
func fallbackPickerDouble(t *testing.T, picker func(ctx context.Context, ns map[string]any) error) {
	t.Helper()
	swapFlowSeam(t, &fallbackPickerFn, picker)
	swapFlowSeam(t, &fallbackStdinTTYFn, func() bool { return true })
}

// Runs icebath fallback with the subcommand and returns stdout, stderr and the exit code
func runFallback(t *testing.T, ctx context.Context, h string, sub any) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := FallbackCommand(ctx, h, map[string]any{"fallback_command": sub}, &out, &errOut)
	if err != nil {
		t.Fatalf("fallback %v: %v", sub, err)
	}
	return out.String(), errOut.String(), code
}

func TestReadChainReadsNewListFormat(t *testing.T) {
	cfg := map[string]any{"fallback_providers": []any{
		map[string]any{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"},
		map[string]any{"provider": "nous", "model": "Hermes-4-Llama-3.1-405B"},
	}}
	want := []map[string]any{
		{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"},
		{"provider": "nous", "model": "Hermes-4-Llama-3.1-405B"},
	}
	if got := ReadFallbackChain(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("chain %v", got)
	}
}

func TestReadChainReturnsCopiesNotAliases(t *testing.T) {
	cfg := map[string]any{"fallback_providers": []any{map[string]any{"provider": "nous", "model": "foo"}}}
	result := ReadFallbackChain(cfg)
	result[0]["provider"] = "mutated"
	if cfg["fallback_providers"].([]any)[0].(map[string]any)["provider"] != "nous" {
		t.Fatal("chain aliases the config entry")
	}
}

func TestExtractFallbackExtractsFromDefaultField(t *testing.T) {
	entry, ok, err := ExtractFallbackFromModelCfg(map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6"})
	want := map[string]any{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"}
	if err != nil || !ok || !reflect.DeepEqual(entry, want) {
		t.Fatalf("entry %v %v %v", entry, ok, err)
	}
}

func TestExtractFallbackReturnsNoneWithoutModel(t *testing.T) {
	if entry, ok, err := ExtractFallbackFromModelCfg(map[string]any{"provider": "openrouter"}); err != nil || ok || entry != nil {
		t.Fatalf("entry %v %v %v", entry, ok, err)
	}
}

func TestListCommandListWithEntries(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{
		"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"},
		"fallback_providers": []any{
			map[string]any{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"},
			map[string]any{"provider": "nous", "model": "Hermes-4"},
		},
	})
	out, _, code := runFallback(t, ctx, h, "ls")
	for _, want := range []string{"anthropic/claude-sonnet-4.6", "Hermes-4", "claude-sonnet-4-6"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	recorded := "\n  Primary:   claude-sonnet-4-6  (via anthropic)\n\n  Fallback chain (2 entries):\n    1. anthropic/claude-sonnet-4.6  (via openrouter)\n    2. Hermes-4  (via nous)\n\n  Tried in order when the primary fails (rate-limit, 5xx, connection errors).\n  Docs: https://hermes-agent.nousresearch.com/docs/user-guide/features/fallback-providers\n\n"
	if out != recorded || code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestAddCommandAddAppendsNewEntry(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"}})
	fallbackPickerDouble(t, fallbackPickerWriting(t, h, map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6",
		"base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}, nil))
	out, _, _ := runFallback(t, ctx, h, "add")
	cfg := readConfig(t, h)
	model := cfg["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["default"] != "claude-sonnet-4-6" {
		t.Fatalf("primary %v", model)
	}
	want := []any{map[string]any{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6", "base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}}
	if !reflect.DeepEqual(cfg["fallback_providers"], want) {
		t.Fatalf("chain %v", cfg["fallback_providers"])
	}
	recorded := "\n  Adding a fallback provider.  The picker below is the same one used by\n  `icebath model` — select the provider + model you want as a fallback.\n\n\n  Added fallback: anthropic/claude-sonnet-4.6  (via openrouter)  [https://openrouter.ai/api/v1]\n  Chain is now 1 entry long.\n\n  Run `icebath fallback list` to view, or `icebath fallback remove` to delete.\n"
	if out != recorded {
		t.Fatalf("out %q", out)
	}
}

func TestAddCommandAddRejectsSameAsPrimary(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "openrouter", "default": "gpt-5.4"}})
	fallbackPickerDouble(t, fallbackPickerWriting(t, h, map[string]any{"provider": "openrouter", "default": "gpt-5.4"}, nil))
	out, _, _ := runFallback(t, ctx, h, "add")
	cfg := readConfig(t, h)
	if chain, has := cfg["fallback_providers"]; has && !reflect.DeepEqual(chain, []any{}) {
		t.Fatalf("self fallback written %v", chain)
	}
	recorded := "\n  Adding a fallback provider.  The picker below is the same one used by\n  `icebath model` — select the provider + model you want as a fallback.\n\n\n  Selected model matches the current primary (gpt-5.4  (via openrouter)).\n  A provider cannot be a fallback for itself — no change.\n"
	if out != recorded {
		t.Fatalf("out %q", out)
	}
}

func TestAddCommandAddPreservesPrimaryWhenPickerChangesIt(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	primary := map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	writeConfig(t, h, map[string]any{"model": primary})
	custom := []any{map[string]any{"name": "Picker-created endpoint", "base_url": "https://picker.example/v1", "model": "picker-model"}}
	fallbackPickerDouble(t, fallbackPickerWriting(t, h, map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6",
		"base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}, map[string]any{"custom_providers": custom}))
	runFallback(t, ctx, h, "add")
	cfg := readConfig(t, h)
	model := cfg["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["default"] != "claude-sonnet-4-6" || model["base_url"] != "https://api.anthropic.com" || model["api_mode"] != "anthropic_messages" {
		t.Fatalf("primary %v", model)
	}
	chain := cfg["fallback_providers"].([]any)
	if len(chain) != 1 || chain[0].(map[string]any)["provider"] != "openrouter" {
		t.Fatalf("chain %v", chain)
	}
	if !reflect.DeepEqual(cfg["custom_providers"], custom) {
		t.Fatalf("custom providers %v", cfg["custom_providers"])
	}
}

func TestAddCommandRestorePreservesAbsentActiveProvider(t *testing.T) {
	h, _ := fallbackTestHome(t)
	store := NewStore(h)
	if err := store.Save(AuthData{"version": 1, "providers": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	before, present, err := SnapshotAuthActiveProvider(store)
	if err != nil || present {
		t.Fatalf("snapshot %v %v %v", before, present, err)
	}
	if err := RestoreAuthActiveProvider(store, before, present); err != nil {
		t.Fatal(err)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, has := data["active_provider"]; has {
		t.Fatalf("active_provider written %v", data)
	}
}

func TestAddCommandPickerFailureRestoresPersistedPrimaryWithoutMaskingError(t *testing.T) {
	primary := map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	picked := map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6", "base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}
	assertRestored := func(t *testing.T, h string) {
		t.Helper()
		persisted := readConfig(t, h)
		if !reflect.DeepEqual(persisted["model"], primary) || persisted["theme"] != "midnight" {
			t.Fatalf("persisted %v", persisted)
		}
	}
	t.Run("exception", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"model": primary, "theme": "midnight"})
		pickerError := errors.New("picker failed")
		write := fallbackPickerWriting(t, h, picked, nil)
		fallbackPickerDouble(t, func(ctx context.Context, ns map[string]any) error {
			if err := write(ctx, ns); err != nil {
				return err
			}
			return pickerError
		})
		_, err := FallbackCommand(ctx, h, map[string]any{"fallback_command": "add"}, io.Discard, io.Discard)
		if err != pickerError {
			t.Fatalf("err %v", err)
		}
		assertRestored(t, h)
	})
	t.Run("ctrl-c", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"model": primary, "theme": "midnight"})
		write := fallbackPickerWriting(t, h, picked, nil)
		fallbackPickerDouble(t, func(ctx context.Context, ns map[string]any) error {
			if err := write(ctx, ns); err != nil {
				return err
			}
			panic(errFlowCancelled)
		})
		func() {
			defer func() {
				if p := recover(); p != errFlowCancelled {
					t.Fatalf("panic %v", p)
				}
			}()
			_, _ = FallbackCommand(ctx, h, map[string]any{"fallback_command": "add"}, io.Discard, io.Discard)
		}()
		assertRestored(t, h)
	})
}

func TestRemoveCommandRemoveSelectedEntry(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{"fallback_providers": []any{
		map[string]any{"provider": "openrouter", "model": "gpt-5.4"},
		map[string]any{"provider": "nous", "model": "Hermes-4"},
		map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6"},
	}})
	var gotChoices []string
	swapFlowSeam(t, &fallbackChooseFn, func(question string, choices []string, def int, description string) (int, error) {
		gotChoices = choices
		return 1, nil
	})
	out, _, _ := runFallback(t, ctx, h, "rm")
	want := []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6"}}
	if got := readConfig(t, h)["fallback_providers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain %v", got)
	}
	if !reflect.DeepEqual(gotChoices, []string{"gpt-5.4  (via openrouter)", "Hermes-4  (via nous)", "claude-sonnet-4-6  (via anthropic)", "Cancel"}) {
		t.Fatalf("choices %v", gotChoices)
	}
	if out != "\n  Removed fallback: Hermes-4  (via nous)\n  Chain is now 2 entries long.\n\n" {
		t.Fatalf("out %q", out)
	}
}

func TestClearCommandClearWithConfirmation(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{"fallback_providers": []any{
		map[string]any{"provider": "openrouter", "model": "gpt-5.4"},
		map[string]any{"provider": "nous", "model": "Hermes-4"},
	}})
	swapFlowSeam(t, &fallbackConfirmFn, func(io.Writer, string) (string, error) { return "y", nil })
	out, _, _ := runFallback(t, ctx, h, "clear")
	if got, has := readConfig(t, h)["fallback_providers"]; !has || !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("chain %v %v", got, has)
	}
	if out != "\n  Current fallback chain (2 entries):\n    1. gpt-5.4  (via openrouter)\n    2. Hermes-4  (via nous)\n\n\n  Fallback chain cleared.\n\n" {
		t.Fatalf("out %q", out)
	}
}

func TestDispatcherUnknownSubcommandExits(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{})
	out, _, code := runFallback(t, ctx, h, "nope")
	if code != 2 || out != "Unknown fallback subcommand: nope\nUse one of: list, add, remove, clear\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestFallbackCommandMatchesRecordedHermesOutput(t *testing.T) {
	adding := "\n  Adding a fallback provider.  The picker below is the same one used by\n  `icebath model` — select the provider + model you want as a fallback.\n\n"
	t.Run("list_empty", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{})
		if out, _, _ := runFallback(t, ctx, h, nil); out != "\n  No fallback providers configured.\n\n  Add one with:  icebath fallback add\n\n" {
			t.Fatalf("out %q", out)
		}
	})
	t.Run("add_dup", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"},
			"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}}})
		fallbackPickerDouble(t, fallbackPickerWriting(t, h, map[string]any{"provider": "openrouter", "default": "gpt-5.4"}, nil))
		if out, _, _ := runFallback(t, ctx, h, "add"); out != adding+"\n  gpt-5.4  (via openrouter) is already in the fallback chain — skipped.\n" {
			t.Fatalf("out %q", out)
		}
		if got := readConfig(t, h)["fallback_providers"]; !reflect.DeepEqual(got, []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}}) {
			t.Fatalf("chain %v", got)
		}
	})
	t.Run("add_none", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"}})
		fallbackPickerDouble(t, fallbackPickerWriting(t, h, map[string]any{"provider": "openrouter"}, nil))
		if out, _, _ := runFallback(t, ctx, h, "add"); out != adding+"\n  No fallback added.\n" {
			t.Fatalf("out %q", out)
		}
		if got := readConfig(t, h)["model"]; !reflect.DeepEqual(got, map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"}) {
			t.Fatalf("model %v", got)
		}
	})
	t.Run("remove_last", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"fallback_providers": []any{map[string]any{"provider": "nous", "model": "Hermes-4"}}})
		swapFlowSeam(t, &fallbackChooseFn, func(string, []string, int, string) (int, error) { return 0, nil })
		if out, _, _ := runFallback(t, ctx, h, "remove"); out != "\n  Removed fallback: Hermes-4  (via nous)\n  Fallback chain is now empty.\n\n" {
			t.Fatalf("out %q", out)
		}
		if got, has := readConfig(t, h)["fallback_providers"]; !has || !reflect.DeepEqual(got, []any{}) {
			t.Fatalf("chain %v %v", got, has)
		}
	})
	t.Run("remove_cancel", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"fallback_providers": []any{map[string]any{"provider": "nous", "model": "Hermes-4"}}})
		swapFlowSeam(t, &fallbackChooseFn, func(string, []string, int, string) (int, error) { return -1, nil })
		if out, _, _ := runFallback(t, ctx, h, "remove"); out != "\n  Cancelled — no change.\n" {
			t.Fatalf("out %q", out)
		}
	})
	t.Run("clear_no_and_eof", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}}})
		listing := "\n  Current fallback chain (1 entry):\n    1. gpt-5.4  (via openrouter)\n\n"
		swapFlowSeam(t, &fallbackConfirmFn, func(io.Writer, string) (string, error) { return "n", nil })
		if out, _, _ := runFallback(t, ctx, h, "clear"); out != listing+"  Cancelled — no change.\n" {
			t.Fatalf("out %q", out)
		}
		swapFlowSeam(t, &fallbackConfirmFn, func(io.Writer, string) (string, error) { return "", io.EOF })
		if out, _, _ := runFallback(t, ctx, h, "clear"); out != listing+"\n  Cancelled.\n" {
			t.Fatalf("out %q", out)
		}
		if got := readConfig(t, h)["fallback_providers"]; len(got.([]any)) != 1 {
			t.Fatalf("chain changed %v", got)
		}
	})
	t.Run("empty_chain", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writeConfig(t, h, map[string]any{})
		if out, _, _ := runFallback(t, ctx, h, "remove"); out != "\n  No fallback providers configured — nothing to remove.\n\n" {
			t.Fatalf("out %q", out)
		}
		if out, _, _ := runFallback(t, ctx, h, "clear"); out != "\n  No fallback providers configured — nothing to clear.\n\n" {
			t.Fatalf("out %q", out)
		}
	})
}

func TestFallbackAddRequiresAnInteractiveTerminal(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	swapFlowSeam(t, &fallbackStdinTTYFn, func() bool { return false })
	swapFlowSeam(t, &fallbackPickerFn, func(context.Context, map[string]any) error {
		t.Fatal("picker ran without a terminal")
		return nil
	})
	out, errOut, code := runFallback(t, ctx, h, "add")
	if code != 1 || out != "" || errOut != "Error: 'icebath fallback add' requires an interactive terminal.\nIt cannot be run through a pipe or non-interactive subprocess.\nRun it directly in your terminal instead.\n" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestFallbackEntryFormattingAndPrimaryDescription(t *testing.T) {
	if FormatFallbackEntry(map[string]any{"model": "m", "provider": "p", "base_url": "http://x"}) != "m  (via p)  [http://x]" || FormatFallbackEntry(map[string]any{}) != "?  (via ?)" {
		t.Fatal("format")
	}
	if EntriesLabel(1) != "1 entry" || EntriesLabel(3) != "3 entries" {
		t.Fatal("entries label")
	}
	for _, tc := range []struct {
		cfg  map[string]any
		want string
	}{
		{map[string]any{"model": "  bare  "}, "bare"},
		{map[string]any{"model": map[string]any{"provider": "p"}}, "?  (via p)"},
		{map[string]any{"model": map[string]any{"model": "m", "provider": " "}}, "m  (via ?)"},
		{map[string]any{}, ""},
	} {
		if got, err := DescribePrimary(tc.cfg); err != nil || got != tc.want {
			t.Fatalf("%v gave %q %v", tc.cfg, got, err)
		}
	}
	if _, err := DescribePrimary(map[string]any{"model": map[string]any{"provider": 5}}); err == nil || err.Error() != "AttributeError: 'int' object has no attribute 'strip'" {
		t.Fatalf("err %v", err)
	}
}

func TestRestoreModelCfgPutsTheSnapshotBack(t *testing.T) {
	h, ctx := fallbackTestHome(t)
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "openrouter", "default": "changed"}, "theme": "x"})
	if err := RestoreModelCfg(ctx, h, map[string]any{"provider": "nous", "default": "Hermes-4"}); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	if !reflect.DeepEqual(cfg["model"], map[string]any{"provider": "nous", "default": "Hermes-4"}) || cfg["theme"] != "x" {
		t.Fatalf("cfg %v", cfg)
	}
	if err := RestoreModelCfg(ctx, h, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := readConfig(t, h)["model"]; has {
		t.Fatal("nil snapshot keeps a model section")
	}
}

func TestFallbackCommandWritesTheRecordedHermesConfigBytes(t *testing.T) {
	picked := map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6", "base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}
	configBytes := func(t *testing.T, h string) string {
		t.Helper()
		raw, err := os.ReadFile(home.ConfigPath(h))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	t.Run("add_append", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writePickerConfig(t, h, "model:\n  default: claude-sonnet-4-6\n  provider: anthropic\n")
		fallbackPickerDouble(t, fallbackPickerWriting(t, h, picked, nil))
		runFallback(t, ctx, h, "add")
		want := "model:\n  default: claude-sonnet-4-6\n  provider: anthropic\n_config_version: 52\nfallback_providers:\n  - provider: openrouter\n    model: anthropic/claude-sonnet-4.6\n    base_url: https://openrouter.ai/api/v1\n    api_mode: chat_completions\n"
		if got := configBytes(t, h); got != want {
			t.Fatalf("config %q", got)
		}
	})
	t.Run("remove", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writePickerConfig(t, h, "fallback_providers:\n- model: gpt-5.4\n  provider: openrouter\n- model: Hermes-4\n  provider: nous\n- model: claude-sonnet-4-6\n  provider: anthropic\n")
		swapFlowSeam(t, &fallbackChooseFn, func(string, []string, int, string) (int, error) { return 1, nil })
		runFallback(t, ctx, h, "rm")
		want := "fallback_providers:\n  - model: gpt-5.4\n    provider: openrouter\n  - model: claude-sonnet-4-6\n    provider: anthropic\n_config_version: 52\n"
		if got := configBytes(t, h); got != want {
			t.Fatalf("config %q", got)
		}
	})
	t.Run("clear", func(t *testing.T) {
		h, ctx := fallbackTestHome(t)
		writePickerConfig(t, h, "fallback_providers:\n- model: gpt-5.4\n  provider: openrouter\n- model: Hermes-4\n  provider: nous\n")
		swapFlowSeam(t, &fallbackConfirmFn, func(io.Writer, string) (string, error) { return "y", nil })
		runFallback(t, ctx, h, "clear")
		if got := configBytes(t, h); got != "fallback_providers: []\n_config_version: 52\n" {
			t.Fatalf("config %q", got)
		}
	})
}
