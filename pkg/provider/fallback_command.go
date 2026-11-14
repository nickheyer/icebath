package provider

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fallback command seams, swappable so tests script the picker, the terminal check and the menus
var (
	fallbackPickerFn     = SelectProviderAndModel
	fallbackStdinTTYFn   = func() bool { return setupUI().StdinIsTTY() }
	fallbackChooseFn     = CursesPromptChoice
	fallbackConfirmFn    = func(out io.Writer, prompt string) (string, error) { return flowRawInputFn(out, prompt) }
	fallbackStoreForHome = NewStore
)

// FallbackEntryIdentity builds the backend identity of a provider, model and base_url chain entry
func FallbackEntryIdentity(entry map[string]any) BackendIdentity {
	return BuildBackendIdentity(stringField(entry, "provider"), stringField(entry, "model"), stringField(entry, "base_url"))
}

// ReadFallbackChain is the normalized chain merged with the legacy fallback_model, always fresh copies
func ReadFallbackChain(cfg map[string]any) []map[string]any {
	chain := GetFallbackChain(cfg)
	out := make([]map[string]any, 0, len(chain))
	for _, entry := range chain {
		out = append(out, cloneMap(entry))
	}
	return out
}

// WriteFallbackChain stores the chain under fallback_providers and drops the legacy fallback_model key
func WriteFallbackChain(cfg map[string]any, chain []map[string]any) {
	list := make([]any, 0, len(chain))
	for _, entry := range chain {
		list = append(list, map[string]any(entry))
	}
	pyDictSet(cfg, "fallback_providers", list)
	delete(cfg, "fallback_model")
}

// FormatFallbackEntry renders one chain entry as model (via provider) with the base URL when set
func FormatFallbackEntry(entry map[string]any) string {
	out := fmt.Sprintf("%s  (via %s)", pyStr(dictGet(entry, "model", "?")), pyStr(dictGet(entry, "provider", "?")))
	if base := entry["base_url"]; pyTruthy(base) {
		out += "  [" + pyStr(base) + "]"
	}
	return out
}

// ExtractFallbackFromModelCfg pulls provider, model, base_url and api_mode out of a config model snapshot
func ExtractFallbackFromModelCfg(modelCfg any) (map[string]any, bool, error) {
	m, ok := modelCfg.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	rawProvider, err := pyOrEmptyStr(m["provider"], "strip")
	if err != nil {
		return nil, false, err
	}
	rawModel := m["default"]
	if !pyTruthy(rawModel) {
		rawModel = m["model"]
	}
	modelText, err := pyOrEmptyStr(rawModel, "strip")
	if err != nil {
		return nil, false, err
	}
	provider, model := pystr.Strip(rawProvider), pystr.Strip(modelText)
	if provider == "" || model == "" {
		return nil, false, nil
	}
	entry := map[string]any{"provider": provider, "model": model}
	config.SetMappingKeys(entry, []string{"provider", "model"})
	for _, key := range []string{"base_url", "api_mode"} {
		raw, err := pyOrEmptyStr(m[key], "strip")
		if err != nil {
			return nil, false, err
		}
		if v := pystr.Strip(raw); v != "" {
			pyDictSet(entry, key, v)
		}
	}
	return entry, true, nil
}

// SnapshotAuthActiveProvider returns the active_provider of auth.json under the store lock and whether it exists
func SnapshotAuthActiveProvider(store *Store) (any, bool, error) {
	unlock, err := store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	data, err := store.Load()
	if err != nil {
		return nil, false, err
	}
	v, ok := data["active_provider"]
	return v, ok, nil
}

// RestoreAuthActiveProvider writes a snapshotted active_provider back, removing the key when it was absent
func RestoreAuthActiveProvider(store *Store, value any, present bool) error {
	return store.Update(func(data AuthData) (bool, error) {
		if !present {
			delete(data, "active_provider")
		} else {
			data["active_provider"] = value
		}
		return true, nil
	})
}

// RestoreModelCfg puts the config model section back to a captured snapshot, re inserted last like a dict
func RestoreModelCfg(ctx context.Context, h string, modelBefore any) error {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	delete(cfg, "model")
	if modelBefore != nil {
		cfg["model"] = config.Clone(modelBefore)
		config.AppendMappingKey(cfg, "model")
	}
	return config.Save(h, cfg)
}

// RestorePrimaryRoute attempts both halves of the temporary picker route restoration
func RestorePrimaryRoute(ctx context.Context, h string, store *Store, modelBefore, activeBefore any, activePresent bool) error {
	var errs []error
	if err := RestoreModelCfg(ctx, h, modelBefore); err != nil {
		errs = append(errs, err)
	}
	if err := RestoreAuthActiveProvider(store, activeBefore, activePresent); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	details := ""
	for i, e := range errs {
		if i > 0 {
			details += "; "
		}
		details += e.Error()
	}
	return &fallbackRestoreError{msg: "Could not fully restore the primary route: " + details, cause: errs[0]}
}

// RuntimeError raised from the first restore failure
type fallbackRestoreError struct {
	msg   string
	cause error
}

func (e *fallbackRestoreError) Error() string { return e.msg }
func (e *fallbackRestoreError) Unwrap() error { return e.cause }

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

// Loads the config and its chain, printing the empty message block and a nil chain when empty
func loadChain(ctx context.Context, w io.Writer, h string, emptyMessage string) (config.Tree, []map[string]any, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, nil, err
	}
	chain := ReadFallbackChain(cfg)
	if len(chain) == 0 {
		fmt.Fprintf(w, "\n%s\n\n", emptyMessage)
		return cfg, nil, nil
	}
	return cfg, chain, nil
}

// DescribePrimary is the one line model (via provider) of the configured primary, empty for none
func DescribePrimary(cfg map[string]any) (string, error) {
	switch m := cfg["model"].(type) {
	case map[string]any:
		rawProvider := m["provider"]
		if !pyTruthy(rawProvider) {
			rawProvider = "?"
		}
		provider, err := pyOrEmptyStr(rawProvider, "strip")
		if err != nil {
			return "", err
		}
		if provider = pystr.Strip(provider); provider == "" {
			provider = "?"
		}
		rawModel := m["default"]
		if !pyTruthy(rawModel) {
			rawModel = m["model"]
		}
		if !pyTruthy(rawModel) {
			rawModel = "?"
		}
		model, err := pyOrEmptyStr(rawModel, "strip")
		if err != nil {
			return "", err
		}
		if model = pystr.Strip(model); model == "" {
			model = "?"
		}
		return fmt.Sprintf("%s  (via %s)", model, provider), nil
	case string:
		return pystr.Strip(m), nil
	}
	return "", nil
}

// FallbackList prints the primary and the fallback chain in order
func FallbackList(ctx context.Context, w io.Writer, h string) error {
	cfg, chain, err := loadChain(ctx, w, h, "  No fallback providers configured.")
	if err != nil {
		return err
	}
	if chain == nil {
		fmt.Fprint(w, "  Add one with:  icebath fallback add\n\n")
		return nil
	}
	fmt.Fprintln(w)
	primary, err := DescribePrimary(cfg)
	if err != nil {
		return err
	}
	if primary != "" {
		fmt.Fprintf(w, "  Primary:   %s\n\n", primary)
	}
	printChain(w, "Fallback chain", chain)
	fmt.Fprintln(w, "  Tried in order when the primary fails (rate-limit, 5xx, connection errors).")
	fmt.Fprint(w, "  Docs: https://hermes-agent.nousresearch.com/docs/user-guide/features/fallback-providers\n\n")
	return nil
}

// Runs the picker then reads the picked entry, restoring the primary route and re raising on any failure
func runFallbackPicker(ctx context.Context, h string, store *Store, picker func() error, modelBefore, activeBefore any, activePresent bool) (entry map[string]any, err error) {
	failed := true
	restoreOnFailure := func(cause error) error {
		if rerr := RestorePrimaryRoute(ctx, h, store, modelBefore, activeBefore, activePresent); rerr != nil {
			return &fallbackPickerError{err: cause, note: "Could not fully restore the primary route after fallback selection failed: " + rerr.Error()}
		}
		return cause
	}
	defer func() {
		if failed {
			if p := recover(); p != nil {
				if rerr := RestorePrimaryRoute(ctx, h, store, modelBefore, activeBefore, activePresent); rerr != nil {
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
	if err := picker(); err != nil {
		failed = false
		return nil, restoreOnFailure(err)
	}
	after, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		failed = false
		return nil, restoreOnFailure(err)
	}
	entry, _, err = ExtractFallbackFromModelCfg(after["model"])
	failed = false
	if err != nil {
		return nil, restoreOnFailure(err)
	}
	return entry, nil
}

// FallbackAdd runs the model picker, restores the primary route, then appends the pick to the chain
func FallbackAdd(ctx context.Context, w io.Writer, h string, store *Store, picker func() error) error {
	before, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	var modelBefore any
	if v, ok := before["model"]; ok {
		modelBefore = config.Clone(v)
	}
	activeBefore, activePresent, err := SnapshotAuthActiveProvider(store)
	if err != nil {
		return err
	}
	fmt.Fprint(w, "\n  Adding a fallback provider.  The picker below is the same one used by\n  `icebath model` — select the provider + model you want as a fallback.\n\n")
	newEntry, err := runFallbackPicker(ctx, h, store, picker, modelBefore, activeBefore, activePresent)
	if err != nil {
		return err
	}
	if err := RestorePrimaryRoute(ctx, h, store, modelBefore, activeBefore, activePresent); err != nil {
		return err
	}
	if newEntry == nil {
		fmt.Fprintln(w, "\n  No fallback added.")
		return nil
	}
	newIdent := FallbackEntryIdentity(newEntry)
	primaryEntry, _, err := ExtractFallbackFromModelCfg(modelBefore)
	if err != nil {
		return err
	}
	if primaryEntry != nil && SameDeploymentIn(ctx, FallbackEntryIdentity(primaryEntry), newIdent) {
		fmt.Fprintf(w, "\n  Selected model matches the current primary (%s).\n", FormatFallbackEntry(newEntry))
		fmt.Fprintln(w, "  A provider cannot be a fallback for itself — no change.")
		return nil
	}
	final, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	chain := ReadFallbackChain(final)
	for _, existing := range chain {
		if SameDeploymentIn(ctx, FallbackEntryIdentity(existing), newIdent) {
			fmt.Fprintf(w, "\n  %s is already in the fallback chain — skipped.\n", FormatFallbackEntry(newEntry))
			return nil
		}
	}
	chain = append(chain, newEntry)
	WriteFallbackChain(final, chain)
	if err := config.Save(h, final); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n  Added fallback: %s\n", FormatFallbackEntry(newEntry))
	fmt.Fprintf(w, "  Chain is now %s long.\n\n", EntriesLabel(len(chain)))
	fmt.Fprintln(w, "  Run `icebath fallback list` to view, or `icebath fallback remove` to delete.")
	return nil
}

// FallbackRemove lets the chooser pick an entry, a cancel or out of range answer leaving the chain alone
func FallbackRemove(ctx context.Context, w io.Writer, h string, choose func(question string, choices []string, def int, description string) (int, error)) error {
	cfg, chain, err := loadChain(ctx, w, h, "  No fallback providers configured — nothing to remove.")
	if err != nil || chain == nil {
		return err
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
	removed := chain[idx]
	chain = append(chain[:idx:idx], chain[idx+1:]...)
	WriteFallbackChain(cfg, chain)
	if err := config.Save(h, cfg); err != nil {
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
	cfg, chain, err := loadChain(ctx, w, h, "  No fallback providers configured — nothing to clear.")
	if err != nil || chain == nil {
		return err
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
	switch pystr.Lower(pystr.Strip(resp)) {
	case "y", "yes":
	default:
		fmt.Fprintln(w, "  Cancelled — no change.")
		return nil
	}
	WriteFallbackChain(cfg, nil)
	if err := config.Save(h, cfg); err != nil {
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
		sub = pyStr(v)
	}
	switch sub {
	case "", "list", "ls":
		return 0, FallbackList(ctx, w, h)
	case "add":
		if !requireFallbackTTY(errw, "fallback add") {
			return 1, nil
		}
		return 0, FallbackAdd(ctx, w, h, fallbackStoreForHome(h), func() error { return fallbackPickerFn(ctx, ns) })
	case "remove", "rm":
		return 0, FallbackRemove(ctx, w, h, fallbackChooseFn)
	case "clear":
		return 0, FallbackClear(ctx, w, h, fallbackConfirmFn)
	}
	fmt.Fprintf(w, "Unknown fallback subcommand: %s\n", sub)
	fmt.Fprintln(w, "Use one of: list, add, remove, clear")
	return 2, nil
}
