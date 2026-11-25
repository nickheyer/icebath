package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	raw := strings.TrimSpace(line)
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
	if anyutil.IsTrue(payload["providers"]) {
		rows, _ = payload["providers"].([]any)
	}
	out := []map[string]any{}
	for _, raw := range rows {
		p := anyMap(raw)
		if anyutil.IsTrue(p["slug"]) && strings.ToLower(strings.TrimSpace(anyutil.String(p["slug"]))) != "moa" && anyutil.IsTrue(p["models"]) {
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
		if anyutil.IsTrue(p["name"]) {
			label = p["name"]
		}
		rows[i] = fmt.Sprintf("%s  (%s)", anyutil.String(label), anyutil.String(p["slug"]))
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
		return nil, fmt.Errorf("Provider %s has no selectable models", anyutil.String(picked["slug"]))
	}
	currentModel, hasModel := current["model"]
	if !hasModel {
		currentModel = ""
	}
	modelDefault := 0
	modelRows := make([]string, len(models))
	for i, m := range models {
		modelRows[i] = anyutil.String(m)
	}
	for i, m := range models {
		if moaSameValue(m, currentModel) {
			modelDefault = i
			break
		}
	}
	idx, err := moaPromptChoice(out, "Select model for "+anyutil.String(picked["slug"]), modelRows, modelDefault)
	if err != nil {
		return nil, err
	}
	return map[string]any{"provider": anyutil.StringOr(picked["slug"], ""), "model": anyutil.String(models[idx])}, nil
}

// Slot label with its reasoning effort when one is set
func moaFormatSlot(slot map[string]any) string {
	label := anyutil.String(slot["provider"]) + ":" + anyutil.String(slot["model"])
	if effort := strings.TrimSpace(anyutil.StringOr(slot["reasoning_effort"], "")); effort != "" {
		return label + " [reasoning=" + effort + "]"
	}
	return label
}

// Billing notice when the aggregator runs on a different provider than the main model of ctx
func moaProviderMismatchNotice(ctx context.Context, aggregator map[string]any) string {
	mainProvider := strings.ToLower(strings.TrimSpace(settings.ModelProvider.Get(ctx)))
	aggProvider := strings.ToLower(strings.TrimSpace(anyutil.StringOr(aggregator["provider"], "")))
	if mainProvider == "" || aggProvider == "" || mainProvider == "moa" || mainProvider == "auto" || mainProvider == aggProvider {
		return ""
	}
	return fmt.Sprintf("Aggregator is on %s; the whole tool loop will be billed there, not to %s.", aggProvider, mainProvider)
}

// Normalized mixture settings of the profile of ctx
func moaView(ctx context.Context) map[string]any {
	return normalizeMoAConfig(moaSettings(ctx))
}

// Prints every preset with its reference and aggregator slots and the billing notice
func moaPrintConfig(ctx context.Context, out io.Writer) {
	moa := moaView(ctx)
	fmt.Fprintln(out, "Mixture of Agents presets")
	fmt.Fprintf(out, "Default: %s\n", anyutil.String(moa["default_preset"]))
	active := "(off)"
	if anyutil.IsTrue(moa["active_preset"]) {
		active = anyutil.String(moa["active_preset"])
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
		if notice := moaProviderMismatchNotice(ctx, agg); notice != "" {
			fmt.Fprintf(out, "    note: %s\n", notice)
		}
	}
}

func moaCmdList(ctx context.Context, out io.Writer, _ MoAArgs) error {
	moaPrintConfig(ctx, out)
	return nil
}

// Picks reference models until done and an aggregator for the named preset, then saves it
func moaCmdConfigure(ctx context.Context, out io.Writer, args MoAArgs) error {
	moa := moaView(ctx)
	presetName := args.Name
	if presetName == "" {
		presetName = anyutil.StringOr(moa["default_preset"], "")
	}
	if presetName == "" {
		presetName = DefaultMoAPresetName
	}
	presetName = strings.TrimSpace(presetName)
	presets := anyMap(moa["presets"])
	current, ok := presets[presetName].(map[string]any)
	if !ok {
		current = anyMap(presets[anyutil.String(moa["default_preset"])])
	}
	fmt.Fprintf(out, "Configure MoA preset: %s\n", presetName)
	fmt.Fprintln(out, "Pick at least one reference model; choose Done when finished.")
	refs := []any{}
	var existing []any
	if anyutil.IsTrue(current["reference_models"]) {
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
		picked["enabled"] = anyutil.IsTrue(enabled)
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
	aggregator, err := moaPickSlot(ctx, out, anyMap(current["aggregator"]))
	if err != nil {
		return err
	}
	if notice := moaProviderMismatchNotice(ctx, aggregator); notice != "" {
		fmt.Fprintln(out, notice)
	}
	if err := moaStorePreset(ctx, presetName, refs, aggregator); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved MoA preset: %s\n", presetName)
	moaPrintConfig(ctx, out)
	return nil
}

// Removes a named preset unless it is the only one, moving the default and clearing the active name
func moaCmdDelete(ctx context.Context, out io.Writer, args MoAArgs) error {
	presetName := strings.TrimSpace(args.Name)
	if presetName == "" {
		return &MoAExitError{Message: "Usage: icebath moa delete <name>"}
	}
	if err := moaDeletePreset(ctx, presetName); err != nil {
		return err
	}
	fmt.Fprintf(out, "Deleted MoA preset: %s\n", presetName)
	return nil
}

var moaSubcommands = map[string]func(context.Context, io.Writer, MoAArgs) error{
	"list":      moaCmdList,
	"ls":        moaCmdList,
	"config":    moaCmdConfigure,
	"configure": moaCmdConfigure,
	"delete":    moaCmdDelete,
}

// CmdMoA manages Mixture of Agents presets for the icebath moa command, list when no subcommand is given
func CmdMoA(ctx context.Context, out io.Writer, args MoAArgs) error {
	sub := args.Command
	if sub == "" {
		sub = "list"
	}
	handler, ok := moaSubcommands[sub]
	if !ok {
		return &MoAExitError{Message: "Unknown moa subcommand: " + sub}
	}
	return handler(ctx, out, args)
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
