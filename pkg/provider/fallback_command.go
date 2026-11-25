package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Fallback command seams, swappable so tests script the picker, the terminal check and the menus
var (
	fallbackPickerFn   = SelectProviderAndModel
	fallbackStdinTTYFn = func() bool { return setupUI().StdinIsTTY() }
	fallbackChooseFn   = CursesPromptChoice
	fallbackConfirmFn  = func(out io.Writer, prompt string) (string, error) { return flowRawInputFn(out, prompt) }
)

// Handler of icebath fallback check, installed by the package that links the support redactor and the parallel probe runner
var fallbackCheckCommand func(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error)

// SetFallbackCheckCommand installs the handler icebath fallback check dispatches to
func SetFallbackCheckCommand(check func(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error)) {
	fallbackCheckCommand = check
}

// FallbackEntryIdentity builds the backend identity of a provider, model and base_url chain entry
func FallbackEntryIdentity(entry map[string]any) BackendIdentity {
	return BuildBackendIdentity(stringField(entry, "provider"), stringField(entry, "model"), stringField(entry, "base_url"))
}

// FormatFallbackEntry renders one chain entry as model (via provider) with the base URL when set
func FormatFallbackEntry(entry map[string]any) string {
	out := fmt.Sprintf("%s  (via %s)", anyutil.String(dictGet(entry, "model", "?")), anyutil.String(dictGet(entry, "provider", "?")))
	if base := entry["base_url"]; anyutil.IsTrue(base) {
		out += "  [" + anyutil.String(base) + "]"
	}
	return out
}

// Primary route of the profile of ctx as a chain entry with its base_url and api_mode, nil when no provider or model is set
func primaryFallbackEntry(ctx context.Context) map[string]any {
	provider, model := strings.TrimSpace(settings.ModelProvider.Get(ctx)), strings.TrimSpace(settings.Model.Get(ctx))
	if provider == "" || model == "" {
		return nil
	}
	entry := map[string]any{"provider": provider, "model": model}
	keys := []string{"provider", "model"}
	for _, field := range []struct{ key, value string }{{"base_url", settings.ModelBaseURL.Get(ctx)}, {"api_mode", settings.ModelAPIMode.Get(ctx)}} {
		if v := strings.TrimSpace(field.value); v != "" {
			entry[field.key] = v
			keys = append(keys, field.key)
		}
	}
	config.SetMappingKeys(entry, keys)
	return entry
}

// Model settings the profile's config.yaml holds as written, by key, taken before the picker runs so the primary route can be put back
type modelSnapshot map[string]any

// Keys of every registered model setting
func modelSettingKeys() []string {
	var keys []string
	for _, e := range settings.All() {
		if key := e.Info().Key; strings.HasPrefix(key, "model.") && !e.Info().State {
			keys = append(keys, key)
		}
	}
	return keys
}

// Snapshot of every model setting the profile of ctx stores
func snapshotModelSettings(ctx context.Context) modelSnapshot {
	out := modelSnapshot{}
	for _, key := range modelSettingKeys() {
		if v, ok := settings.Stored(ctx, key); ok {
			out[key] = v
		}
	}
	return out
}

// Puts the profile's model settings back to a snapshot, writing through the settings writer only the keys that differ from it
func restoreModelSettings(ctx context.Context, before modelSnapshot) error {
	for _, key := range modelSettingKeys() {
		want, held := before[key]
		now, stored := settings.Stored(ctx, key)
		if held == stored && (!held || reflect.DeepEqual(now, want)) {
			continue
		}
		var value any
		if held {
			value = want
		}
		if _, err := settings.Set(ctx, key, value, settings.Global); err != nil {
			return err
		}
	}
	return nil
}

// Picker failure with the note a failed primary route restore attached, the picker error kept as the cause
type fallbackPickerError struct {
	err  error
	note string
}

func (e *fallbackPickerError) Error() string { return e.err.Error() + "\n" + e.note }
func (e *fallbackPickerError) Unwrap() error { return e.err }

// EntriesLabel renders 1 entry or N entries
func EntriesLabel(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// Prints the heading with the entry count then the numbered chain and a blank line
func printChain(w io.Writer, heading string, chain []map[string]any) {
	fmt.Fprintf(w, "  %s (%s):\n", heading, EntriesLabel(len(chain)))
	text := ""
	for i, entry := range chain {
		text += fmt.Sprintf("    %d. %s\n", i+1, FormatFallbackEntry(entry))
	}
	fmt.Fprintln(w, text)
}

// Chain the profile of ctx stores, printing the empty message block and answering nil when it is empty
func loadChain(ctx context.Context, w io.Writer, emptyMessage string) []map[string]any {
	chain := StoredFallbackChain(ctx)
	if len(chain) == 0 {
		fmt.Fprintf(w, "\n%s\n\n", emptyMessage)
		return nil
	}
	return chain
}

// DescribePrimary is the one line model (via provider) of the primary route of ctx, a question mark for the half not set, empty when neither is
func DescribePrimary(ctx context.Context) string {
	provider, model := strings.TrimSpace(settings.ModelProvider.Get(ctx)), strings.TrimSpace(settings.Model.Get(ctx))
	if provider == "" && model == "" {
		return ""
	}
	if provider == "" {
		provider = "?"
	}
	if model == "" {
		model = "?"
	}
	return fmt.Sprintf("%s  (via %s)", model, provider)
}

// FallbackList prints the primary and the fallback chain in order
func FallbackList(ctx context.Context, w io.Writer, h string) error {
	ctx = home.WithOverride(ctx, h)
	chain := loadChain(ctx, w, "  No fallback providers configured.")
	if chain == nil {
		fmt.Fprint(w, "  Add one with:  icebath fallback add\n\n")
		return nil
	}
	fmt.Fprintln(w)
	if primary := DescribePrimary(ctx); primary != "" {
		fmt.Fprintf(w, "  Primary:   %s\n\n", primary)
	}
	printChain(w, "Fallback chain", chain)
	fmt.Fprintln(w, "  Tried in order when the primary fails (rate-limit, 5xx, connection errors).")
	fmt.Fprint(w, "  Help: icebath fallback --help\n\n")
	return nil
}

// Runs the picker then reads the picked entry, restoring the primary route and re raising on any failure
func runFallbackPicker(ctx context.Context, picker func() error, before modelSnapshot) (entry map[string]any, err error) {
	failed := true
	defer func() {
		if failed {
			if p := recover(); p != nil {
				if rerr := restoreModelSettings(ctx, before); rerr != nil {
					if _, exiting := p.(SetupExit); !exiting {
						if cause, isErr := p.(error); isErr {
							p = &fallbackPickerError{err: cause, note: "Could not fully restore the primary route after fallback selection failed: " + rerr.Error()}
						}
					}
				}
				panic(p)
			}
		}
	}()
	if perr := picker(); perr != nil {
		failed = false
		if rerr := restoreModelSettings(ctx, before); rerr != nil {
			return nil, &fallbackPickerError{err: perr, note: "Could not fully restore the primary route after fallback selection failed: " + rerr.Error()}
		}
		return nil, perr
	}
	failed = false
	return primaryFallbackEntry(ctx), nil
}

// FallbackAdd runs the model picker, restores the primary route, then appends the pick to the chain
func FallbackAdd(ctx context.Context, w io.Writer, h string, picker func() error) error {
	ctx = home.WithOverride(ctx, h)
	before := snapshotModelSettings(ctx)
	fmt.Fprint(w, "\n  Adding a fallback provider.  The picker below is the same one used by\n  `icebath model` — select the provider + model you want as a fallback.\n\n")
	newEntry, err := runFallbackPicker(ctx, picker, before)
	if err != nil {
		return err
	}
	if err := restoreModelSettings(ctx, before); err != nil {
		return fmt.Errorf("Could not restore the primary route: %w", err)
	}
	if newEntry == nil {
		fmt.Fprintln(w, "\n  No fallback added.")
		return nil
	}
	chain, err := AppendFallback(ctx, h, newEntry)
	switch {
	case errors.Is(err, ErrFallbackIsPrimary):
		fmt.Fprintf(w, "\n  Selected model matches the current primary (%s).\n", FormatFallbackEntry(newEntry))
		fmt.Fprintln(w, "  A provider cannot be a fallback for itself — no change.")
		return nil
	case errors.Is(err, ErrFallbackInChain):
		fmt.Fprintf(w, "\n  %s is already in the fallback chain — skipped.\n", FormatFallbackEntry(newEntry))
		return nil
	case err != nil:
		return err
	}
	fmt.Fprintf(w, "\n  Added fallback: %s\n", FormatFallbackEntry(newEntry))
	fmt.Fprintf(w, "  Chain is now %s long.\n\n", EntriesLabel(len(chain)))
	fmt.Fprintln(w, "  Run `icebath fallback list` to view, or `icebath fallback remove` to delete.")
	return nil
}

// FallbackRemove lets the chooser pick an entry, a cancel or out of range answer leaving the chain alone
func FallbackRemove(ctx context.Context, w io.Writer, h string, choose func(question string, choices []string, def int, description string) (int, error)) error {
	chain := loadChain(home.WithOverride(ctx, h), w, "  No fallback providers configured — nothing to remove.")
	if chain == nil {
		return nil
	}
	options := make([]string, 0, len(chain)+1)
	for _, entry := range chain {
		options = append(options, FormatFallbackEntry(entry))
	}
	options = append(options, "Cancel")
	idx, err := choose("Select a fallback to remove:", options, 0, "")
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(chain) {
		fmt.Fprintln(w, "\n  Cancelled — no change.")
		return nil
	}
	removed, chain, err := RemoveFallbackAt(ctx, h, idx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\n  Removed fallback: %s\n", FormatFallbackEntry(removed))
	if len(chain) > 0 {
		fmt.Fprintf(w, "  Chain is now %s long.\n\n", EntriesLabel(len(chain)))
	} else {
		fmt.Fprint(w, "  Fallback chain is now empty.\n\n")
	}
	return nil
}

// FallbackClear empties the chain after a y or yes confirmation, an interrupted prompt cancelling
func FallbackClear(ctx context.Context, w io.Writer, h string, confirm func(out io.Writer, prompt string) (string, error)) error {
	chain := loadChain(home.WithOverride(ctx, h), w, "  No fallback providers configured — nothing to clear.")
	if chain == nil {
		return nil
	}
	fmt.Fprintln(w)
	printChain(w, "Current fallback chain", chain)
	resp, err := confirm(w, "  Clear all entries? [y/N]: ")
	if err != nil {
		if !errors.Is(err, io.EOF) && !IsPromptInterrupt(err) {
			return err
		}
		fmt.Fprintln(w, "\n  Cancelled.")
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "y", "yes":
	default:
		fmt.Fprintln(w, "  Cancelled — no change.")
		return nil
	}
	if err := ClearFallbacks(ctx, h); err != nil {
		return err
	}
	fmt.Fprint(w, "\n  Fallback chain cleared.\n\n")
	return nil
}

// Exits 1 with the terminal requirement on errw when stdin is no terminal, true when the command may run
func requireFallbackTTY(errw io.Writer, commandName string) bool {
	if fallbackStdinTTYFn() {
		return true
	}
	fmt.Fprint(errw, "Error: 'icebath "+commandName+"' requires an interactive terminal.\n"+
		"It cannot be run through a pipe or non-interactive subprocess.\n"+
		"Run it directly in your terminal instead.\n")
	return false
}

// FallbackCommand is icebath fallback dispatching on fallback_command, 2 for an unknown subcommand
func FallbackCommand(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (code int, err error) {
	restore := WithSetupOutput(w)
	defer restore()
	defer RecoverSetupExit(&code)
	sub := ""
	if v, ok := ns["fallback_command"]; ok && v != nil {
		sub = anyutil.String(v)
	}
	switch sub {
	case "", "list", "ls":
		return 0, FallbackList(ctx, w, h)
	case "add":
		if !requireFallbackTTY(errw, "fallback add") {
			return 1, nil
		}
		return 0, FallbackAdd(ctx, w, h, func() error { return fallbackPickerFn(ctx, ns) })
	case "remove", "rm":
		return 0, FallbackRemove(ctx, w, h, fallbackChooseFn)
	case "clear":
		return 0, FallbackClear(ctx, w, h, fallbackConfirmFn)
	case "check":
		return fallbackCheckCommand(ctx, h, ns, w)
	}
	fmt.Fprintf(w, "Unknown fallback subcommand: %s\n", sub)
	fmt.Fprintln(w, "Use one of: list, add, remove, clear, check")
	return 2, nil
}
