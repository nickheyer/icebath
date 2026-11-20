package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// MoAArgs carries the parsed icebath moa subcommand as typed and the optional preset name
type MoAArgs struct {
	Command string
	Name    string
}

// MoAExitError is a moa command refusal that exits with status one after printing its message
type MoAExitError struct{ Message string }

func (e *MoAExitError) Error() string { return e.Message }

// Inventory seams the slot prompts read, swappable so tests script the provider rows
var (
	moaLoadPickerContextFn  = loadPickerContext
	moaBuildModelsPayloadFn = buildModelsPayload
)

// Picker payload of configured providers the slot prompts offer
func moaModelsPayload(ctx context.Context) (map[string]any, error) {
	pc, err := moaLoadPickerContextFn(ctx)
	if err != nil {
		return nil, err
	}
	maxModels := 200
	return moaBuildModelsPayloadFn(ctx, pc, ModelsPayloadOptions{IncludeUnconfigured: false, PickerHints: true,
		CanonicalOrder: true, Pricing: true, Capabilities: true, ProbeCustomProviders: true, MaxModels: &maxModels})
}

// Index chosen from a radio list, the numbered stdin prompt when the menu cannot draw
func moaPromptChoice(out io.Writer, title string, rows []string, def int) (int, error) {
	choices := make([][]PickerSegment, len(rows))
	for i, row := range rows {
		choices[i] = []PickerSegment{{Text: row}}
	}
	if idx, err := pickerRadiolistFn(title, choices, PickerRadiolistOptions{Selected: def, CancelReturns: def}); err == nil {
		return idx, nil
	}
	for i, row := range rows {
		fmt.Fprintf(out, "%d. %s\n", i+1, row)
	}
	line, err := pickerInputFn(out, fmt.Sprintf("%s [%d]: ", title, def+1))
	if err != nil {
		return 0, err
	}
	raw := pyStripSpace(line)
	if raw == "" {
		return def, nil
	}
	n, ok := moaPyIntText(raw)
	if !ok {
		return def, nil
	}
	n.Sub(n, big.NewInt(1))
	if n.Sign() < 0 {
		return 0, nil
	}
	if last := big.NewInt(int64(len(rows) - 1)); n.Cmp(last) > 0 {
		return len(rows) - 1, nil
	}
	return int(n.Int64()), nil
}

// Configured provider rows with models, the moa virtual provider excluded
func moaModelOptions(ctx context.Context) ([]map[string]any, error) {
	payload, err := moaModelsPayload(ctx)
	if err != nil {
		return nil, err
	}
	var rows []any
	if config.PyTruthy(payload["providers"]) {
		rows, _ = payload["providers"].([]any)
	}
	out := []map[string]any{}
	for _, raw := range rows {
		p := anyMap(raw)
		if config.PyTruthy(p["slug"]) && strings.ToLower(pyStripSpace(pyStr(p["slug"]))) != "moa" && config.PyTruthy(p["models"]) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Model list of a provider row as Python values
func moaRowModels(p map[string]any) []any {
	switch models := p["models"].(type) {
	case []any:
		return append([]any{}, models...)
	case []string:
		out := make([]any, len(models))
		for i, m := range models {
			out[i] = m
		}
		return out
	}
	return nil
}

// Provider and model picked through the radio lists, starting at the current slot
func moaPickSlot(ctx context.Context, out io.Writer, current map[string]any) (map[string]any, error) {
	providers, err := moaModelOptions(ctx)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, errors.New("No configured model providers found. Run `icebath model` first.")
	}
	currentProvider, hasProvider := current["provider"]
	if !hasProvider {
		currentProvider = ""
	}
	providerDefault := 0
	rows := make([]string, len(providers))
	for i, p := range providers {
		label := p["slug"]
		if config.PyTruthy(p["name"]) {
			label = p["name"]
		}
		rows[i] = fmt.Sprintf("%s  (%s)", pyStr(label), pyStr(p["slug"]))
	}
	for i, p := range providers {
		if moaSameValue(p["slug"], currentProvider) {
			providerDefault = i
			break
		}
	}
	choice, err := moaPromptChoice(out, "Select provider", rows, providerDefault)
	if err != nil {
		return nil, err
	}
	picked := providers[choice]
	models := moaRowModels(picked)
	if len(models) == 0 {
		return nil, fmt.Errorf("Provider %s has no selectable models", pyStr(picked["slug"]))
	}
	currentModel, hasModel := current["model"]
	if !hasModel {
		currentModel = ""
	}
	modelDefault := 0
	modelRows := make([]string, len(models))
	for i, m := range models {
		modelRows[i] = pyStr(m)
	}
	for i, m := range models {
		if moaSameValue(m, currentModel) {
			modelDefault = i
			break
		}
	}
	idx, err := moaPromptChoice(out, "Select model for "+pyStr(picked["slug"]), modelRows, modelDefault)
	if err != nil {
		return nil, err
	}
	return map[string]any{"provider": pyOrStr(picked["slug"]), "model": pyStr(models[idx])}, nil
}

// Slot label with its reasoning effort when one is set
func moaFormatSlot(slot map[string]any) string {
	label := pyStr(slot["provider"]) + ":" + pyStr(slot["model"])
	if effort := pyStripSpace(pyOrStr(slot["reasoning_effort"])); effort != "" {
		return label + " [reasoning=" + effort + "]"
	}
	return label
}

// Billing notice when the aggregator runs on a different provider than the main model
func moaProviderMismatchNotice(cfg map[string]any, aggregator map[string]any) string {
	mainProvider := ""
	if section, ok := cfg["model"].(map[string]any); ok {
		mainProvider = strings.ToLower(pyStripSpace(pyOrStr(section["provider"])))
	}
	aggProvider := strings.ToLower(pyStripSpace(pyOrStr(aggregator["provider"])))
	if mainProvider == "" || aggProvider == "" || mainProvider == "moa" || mainProvider == "auto" || mainProvider == aggProvider {
		return ""
	}
	return fmt.Sprintf("Aggregator is on %s; the whole tool loop will be billed there, not to %s.", aggProvider, mainProvider)
}

// Normalized MoA section of a loaded config
func moaSection(cfg map[string]any) map[string]any {
	return normalizeMoAConfig(cfg["moa"])
}

// Prints every preset with its reference and aggregator slots and the billing notice
func moaPrintConfig(out io.Writer, cfg map[string]any) {
	moa := moaSection(cfg)
	fmt.Fprintln(out, "Mixture of Agents presets")
	fmt.Fprintf(out, "Default: %s\n", pyStr(moa["default_preset"]))
	active := "(off)"
	if config.PyTruthy(moa["active_preset"]) {
		active = pyStr(moa["active_preset"])
	}
	fmt.Fprintf(out, "Active in config: %s\n", active)
	presets := anyMap(moa["presets"])
	for _, name := range moaPresetOrder(presets) {
		preset := anyMap(presets[name])
		marker := " "
		if name == moa["default_preset"] {
			marker = "*"
		}
		fmt.Fprintf(out, "\n%s %s\n", marker, name)
		fmt.Fprintln(out, "  Reference models (advise once per user turn by default):")
		refs, _ := preset["reference_models"].([]any)
		for i, slot := range refs {
			fmt.Fprintf(out, "    %d. %s\n", i+1, moaFormatSlot(anyMap(slot)))
		}
		agg := anyMap(preset["aggregator"])
		fmt.Fprintf(out, "  Aggregator: %s (acting model — runs every step and carries almost all of the cost)\n", moaFormatSlot(agg))
		if notice := moaProviderMismatchNotice(cfg, agg); notice != "" {
			fmt.Fprintf(out, "    note: %s\n", notice)
		}
	}
}

// Stores the normalized MoA section in the config and saves it to the home
func moaSave(ctx context.Context, cfg config.Tree, moa map[string]any) error {
	cfg["moa"] = normalizeMoAConfig(moa)
	return config.Save(home.Dir(ctx), cfg)
}

func moaCmdList(_ context.Context, out io.Writer, cfg config.Tree, _ MoAArgs) error {
	moaPrintConfig(out, cfg)
	return nil
}

// Picks reference models until done and an aggregator for the named preset, then saves it
func moaCmdConfigure(ctx context.Context, out io.Writer, cfg config.Tree, args MoAArgs) error {
	moa := moaSection(cfg)
	presetName := args.Name
	if presetName == "" {
		presetName = pyOrStr(moa["default_preset"])
	}
	if presetName == "" {
		presetName = DefaultMoAPresetName
	}
	presetName = pyStripSpace(presetName)
	presets := anyMap(moa["presets"])
	current, ok := presets[presetName].(map[string]any)
	if !ok {
		current = anyMap(presets[pyStr(moa["default_preset"])])
	}
	fmt.Fprintf(out, "Configure MoA preset: %s\n", presetName)
	fmt.Fprintln(out, "Pick at least one reference model; choose Done when finished.")
	refs := []any{}
	var existing []any
	if config.PyTruthy(current["reference_models"]) {
		existing, _ = current["reference_models"].([]any)
	}
	for {
		var base map[string]any
		if len(refs) < len(existing) {
			base = anyMap(existing[len(refs)])
		}
		picked, err := moaPickSlot(ctx, out, base)
		if err != nil {
			return err
		}
		enabled, has := base["enabled"]
		if !has {
			enabled = true
		}
		picked["enabled"] = config.PyTruthy(enabled)
		refs = append(refs, picked)
		choice, err := moaPromptChoice(out, "Add another reference model?", []string{"Add another", "Done"}, 1)
		if err != nil {
			return err
		}
		if choice == 1 {
			break
		}
	}
	fmt.Fprintln(out, "Configure aggregator model.")
	fmt.Fprintln(out, "The aggregator is the acting model: it runs every tool-loop step, and almost all of the run's cost lands on its provider.")
	updated := make(map[string]any, len(current))
	for k, v := range current {
		updated[k] = v
	}
	updated["reference_models"] = refs
	aggregator, err := moaPickSlot(ctx, out, anyMap(updated["aggregator"]))
	if err != nil {
		return err
	}
	updated["aggregator"] = aggregator
	presets[presetName] = updated
	if _, has := moa["default_preset"]; !has {
		moa["default_preset"] = presetName
	}
	if notice := moaProviderMismatchNotice(cfg, aggregator); notice != "" {
		fmt.Fprintln(out, notice)
	}
	if err := moaSave(ctx, cfg, moa); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved MoA preset: %s\n", presetName)
	moaPrintConfig(out, cfg)
	return nil
}

// Removes a named preset unless it is the only one, moving the default and clearing the active name
func moaCmdDelete(ctx context.Context, out io.Writer, cfg config.Tree, args MoAArgs) error {
	moa := moaSection(cfg)
	presetName := pyStripSpace(args.Name)
	if presetName == "" {
		return &MoAExitError{Message: "Usage: icebath moa delete <name>"}
	}
	presets := anyMap(moa["presets"])
	if _, ok := presets[presetName]; !ok {
		return &MoAExitError{Message: "Unknown MoA preset: " + presetName}
	}
	if len(presets) <= 1 {
		return &MoAExitError{Message: "Cannot delete the only MoA preset"}
	}
	delete(presets, presetName)
	if moa["default_preset"] == presetName {
		moa["default_preset"] = moaPresetOrder(presets)[0]
	}
	if moa["active_preset"] == presetName {
		moa["active_preset"] = ""
	}
	if err := moaSave(ctx, cfg, moa); err != nil {
		return err
	}
	fmt.Fprintf(out, "Deleted MoA preset: %s\n", presetName)
	return nil
}

var moaSubcommands = map[string]func(context.Context, io.Writer, config.Tree, MoAArgs) error{
	"list":      moaCmdList,
	"ls":        moaCmdList,
	"config":    moaCmdConfigure,
	"configure": moaCmdConfigure,
	"delete":    moaCmdDelete,
}

// CmdMoA manages Mixture of Agents presets for the icebath moa command, list when no subcommand is given
func CmdMoA(ctx context.Context, out io.Writer, args MoAArgs) error {
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	sub := args.Command
	if sub == "" {
		sub = "list"
	}
	handler, ok := moaSubcommands[sub]
	if !ok {
		return &MoAExitError{Message: "Unknown moa subcommand: " + sub}
	}
	return handler(ctx, out, cfg, args)
}

// Python equality of two scalar config values, false for unhashable shapes that never equal a scalar
func moaSameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return reflect.DeepEqual(a, b)
	}
	return a == b
}
