package acp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Advertised description and input hint of one command the gateway path serves
type gatewayCommandSpec struct {
	Name        string
	Description string
	Hint        string
}

// Commands the gateway path advertises, each served through a gateway verb
var gatewayCommandSpecs = []gatewayCommandSpec{
	{"help", "List available commands", ""},
	{"model", "Show current model and provider, or switch models", "model name to switch to"},
	{"tools", "List available tools", ""},
	{"context", "Show conversation context usage", ""},
	{"new", "Start a new conversation with an empty history", ""},
	{"compress", "Compress conversation context", ""},
	{"steer", "Inject guidance into the currently running agent turn", "guidance for the active turn"},
	{"queue", "Queue a prompt to run after the current turn finishes", "prompt to run next"},
	{"usage", "Show token usage and rate limits", ""},
	{"status", "Show session, model, token, and context info", ""},
	{"version", "Show Icebath version", ""},
	{"title", "Show or set the session title", "new title"},
	{"undo", "Remove the last N user turns and their replies", "number of turns"},
	{"retry", "Resend the last user message", ""},
}

// Advertisement of the commands the gateway path serves
func gatewayAvailableCommandsUpdate() *acputil.AvailableCommandsUpdate {
	commands := make([]acputil.AvailableCommand, 0, len(gatewayCommandSpecs))
	for _, c := range gatewayCommandSpecs {
		cmd := acputil.AvailableCommand{Name: c.Name, Description: c.Description}
		if c.Hint != "" {
			cmd.Input = &acputil.UnstructuredCommandInput{Hint: c.Hint}
		}
		commands = append(commands, cmd)
	}
	return &acputil.AvailableCommandsUpdate{AvailableCommands: commands}
}

// Reply of /help on the gateway path
func gatewayHelpText() string {
	lines := []string{"Available commands:", ""}
	for _, c := range gatewayCommandSpecs {
		lines = append(lines, fmt.Sprintf("  /%-10s  %s", c.Name, c.Description))
	}
	lines = append(lines, "", "Other Icebath commands run on the gateway, which answers them or says why not.",
		"Unrecognized /commands are sent to the model as normal messages.")
	return strings.Join(lines, "\n")
}

// Mode of the session's edit approval policy, ask for an owner that reports none
func gatewayCurrentMode(info *jsonutil.Object) string {
	policy := DefaultEditApprovalPolicy
	if v := dget(info, "edit_approval_policy"); anyutil.IsTrue(v) {
		policy = anyutil.String(v)
	}
	if mode, ok := EditApprovalPolicyToMode[policy]; ok {
		return mode
	}
	return DefaultMode
}

// Mode state of a session from its info
func gatewaySessionModes(info *jsonutil.Object) *acputil.SessionModeState {
	modes := make([]acputil.SessionMode, 0, len(ModeOrder))
	for _, id := range ModeOrder {
		spec := Modes[id]
		modes = append(modes, acputil.SessionMode{ID: id, Name: spec.Name, Description: new(spec.Description)})
	}
	return &acputil.SessionModeState{CurrentModeID: gatewayCurrentMode(info), AvailableModes: modes}
}

// Config option id of the reasoning effort picker
const ReasoningConfigID = "reasoning_effort"

// Reasoning levels of the owner, default handing the level back to the profile
func reasoningChoices() []string {
	return append([]string{"default", "none"}, config.ValidReasoningEfforts...)
}

// Composer pickers of the editor, the edit mode, the model and the reasoning effort
func gatewayConfigOptions(models *acputil.SessionModelState, info *jsonutil.Object, modeID string) []acputil.SessionConfigOption {
	selectOption := func(configID, name, category, current string, choices [][2]string) *acputil.SessionConfigOptionSelect {
		options := make([]acputil.SessionConfigSelectOption, 0, len(choices))
		for _, choice := range choices {
			options = append(options, acputil.SessionConfigSelectOption{Value: choice[0], Name: choice[1]})
		}
		return &acputil.SessionConfigOptionSelect{ID: configID, Name: name, Category: &category, CurrentValue: current, Options: options}
	}
	var policies [][2]string
	for _, id := range ModeOrder {
		policies = append(policies, [2]string{Modes[id].Policy, Modes[id].Name})
	}
	options := []acputil.SessionConfigOption{selectOption(EditApprovalPolicyConfigID, "Edits", "mode", Modes[modeID].Policy, policies)}
	if models != nil {
		var choices [][2]string
		for _, m := range models.AvailableModels {
			choices = append(choices, [2]string{m.ModelID, m.Name})
		}
		options = append(options, selectOption("model", "Model", "model", models.CurrentModelID, choices))
	}
	levels := reasoningChoices()[1:]
	effort := "default"
	if v, ok := dget(info, "reasoning_effort").(string); ok && slices.Contains(levels, v) {
		effort = v
	}
	offered := levels
	if effort == "default" {
		offered = append([]string{"default"}, levels...)
	}
	var efforts [][2]string
	for _, level := range offered {
		efforts = append(efforts, [2]string{level, level})
	}
	options = append(options, selectOption(ReasoningConfigID, "Reasoning effort", "thought_level", effort, efforts))
	return options
}

// Picker for the session's model from the shared inventory, a single current model row when nothing is listable
var gatewayModelState = func(ctx context.Context, info *jsonutil.Object) *acputil.SessionModelState {
	model := ""
	if v := dget(info, "model"); anyutil.IsTrue(v) {
		model = strings.TrimSpace(anyutil.String(v))
	}
	providerName := ""
	if v := dget(info, "provider"); anyutil.IsTrue(v) {
		providerName = anyutil.String(v)
	}
	if providerName == "" {
		providerName = DetectProvider(ctx)
	}
	if providerName == "" {
		providerName = "openrouter"
	}
	picker, err := func() (picker *acputil.SessionModelState, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return BuildModelState(ctx, model, providerName, "")
	}()
	if err != nil {
		pkgLog().Debug("Could not build ACP model state\n" + err.Error())
	} else if picker != nil {
		return picker
	}
	if model == "" {
		return nil
	}
	choice := EncodeModelChoice(providerName, model)
	return &acputil.SessionModelState{AvailableModels: []acputil.ModelInfo{{ModelID: choice, Name: model}}, CurrentModelID: choice}
}

// Session provenance meta for the editor's id over the logical session's current compression tip
var gatewayProvenanceMeta = func(ctx context.Context, dbPath, acpSessionID, sessionID string) (*jsonutil.Object, error) {
	if !pathExists(dbPath) {
		return nil, nil
	}
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		var sqliteErr *state.SQLiteError
		if errors.As(err, &sqliteErr) {
			pkgLog().Debug(fmt.Sprintf("Could not build ACP session provenance for %s\n%s", sessionID, err))
			return nil, nil
		}
		return nil, err
	}
	defer db.Close()
	tip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil {
		var sqliteErr *state.SQLiteError
		if errors.As(err, &sqliteErr) {
			pkgLog().Debug(fmt.Sprintf("Could not build ACP session provenance for %s\n%s", sessionID, err))
			return nil, nil
		}
		return nil, err
	}
	if tip == "" {
		tip = sessionID
	}
	return SessionProvenanceMeta(ctx, db, acpSessionID, tip, ""), nil
}

// Context indicator update of a usage dict, nil without a positive integer window
func gatewayUsageUpdate(stats *jsonutil.Object) *acputil.UsageUpdate {
	size, ok := intStrict(dget(stats, "context_length"))
	if !ok || size <= 0 {
		return nil
	}
	used := 0
	if v := dget(stats, "context_tokens"); anyutil.IsTrue(v) {
		f, _ := floatNumber(v)
		used = int(f)
	}
	return &acputil.UsageUpdate{Size: size, Used: max(used, 0)}
}

// Integer value of a decoded JSON int, bools and floats refused
func intStrict(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}

// Prompt response usage of the agent's session counters, nil when stats is not a dict
func gatewayTurnUsage(stats any) *acputil.Usage {
	d, ok := stats.(*jsonutil.Object)
	if !ok || d == nil {
		return nil
	}
	count := func(key string) int {
		if v := dget(d, key); anyutil.IsTrue(v) {
			f, _ := floatNumber(v)
			return int(f)
		}
		return 0
	}
	usage := &acputil.Usage{InputTokens: count("input_tokens"), OutputTokens: count("output_tokens"), TotalTokens: count("total_tokens")}
	if v := dget(d, "reasoning_tokens"); anyutil.IsTrue(v) {
		usage.ThoughtTokens = new(count("reasoning_tokens"))
	}
	if v := dget(d, "cache_read_tokens"); anyutil.IsTrue(v) {
		usage.CachedReadTokens = new(count("cache_read_tokens"))
	}
	return usage
}

// Session info update carrying a new title stamped now
func gatewayTitleUpdate(title string) *acputil.SessionInfoUpdate {
	return &acputil.SessionInfoUpdate{Title: &title, UpdatedAt: new(time.Now().UTC().Format(timeutil.RFC3339Micro))}
}
