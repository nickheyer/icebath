package acp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/state"
)

var gatewayMetaLog = logging.GetLogger("acp_adapter.gateway_session_meta")

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
	{"reset", "Clear conversation history", ""},
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
func gatewayAvailableCommandsUpdate() *pyacp.AvailableCommandsUpdate {
	commands := make([]pyacp.AvailableCommand, 0, len(gatewayCommandSpecs))
	for _, c := range gatewayCommandSpecs {
		cmd := pyacp.AvailableCommand{Name: c.Name, Description: c.Description}
		if c.Hint != "" {
			cmd.Input = &pyacp.UnstructuredCommandInput{Hint: c.Hint}
		}
		commands = append(commands, cmd)
	}
	return &pyacp.AvailableCommandsUpdate{AvailableCommands: commands}
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
func gatewayCurrentMode(info *pyjson.Object) string {
	policy := DefaultEditApprovalPolicy
	if v := dget(info, "edit_approval_policy"); truthy(v) {
		policy = pyStr(v)
	}
	if mode, ok := EditApprovalPolicyToMode[policy]; ok {
		return mode
	}
	return DefaultMode
}

// Mode state of a session from its info
func gatewaySessionModes(info *pyjson.Object) *pyacp.SessionModeState {
	modes := make([]pyacp.SessionMode, 0, len(ModeOrder))
	for _, id := range ModeOrder {
		spec := Modes[id]
		modes = append(modes, pyacp.SessionMode{ID: id, Name: spec.Name, Description: pyacp.Ptr(spec.Description)})
	}
	return &pyacp.SessionModeState{CurrentModeID: gatewayCurrentMode(info), AvailableModes: modes}
}

// Config option id of the reasoning effort picker
const ReasoningConfigID = "reasoning_effort"

// Reasoning levels of the owner, default handing the level back to the profile
func reasoningChoices() []string {
	return append([]string{"default", "none"}, config.ValidReasoningEfforts...)
}

// Composer pickers of the editor, the edit mode, the model and the reasoning effort
func gatewayConfigOptions(models *pyacp.SessionModelState, info *pyjson.Object, modeID string) []pyacp.SessionConfigOption {
	selectOption := func(configID, name, category, current string, choices [][2]string) *pyacp.SessionConfigOptionSelect {
		options := make([]pyacp.SessionConfigSelectOption, 0, len(choices))
		for _, choice := range choices {
			options = append(options, pyacp.SessionConfigSelectOption{Value: choice[0], Name: choice[1]})
		}
		return &pyacp.SessionConfigOptionSelect{ID: configID, Name: name, Category: &category, CurrentValue: current, Options: options}
	}
	var policies [][2]string
	for _, id := range ModeOrder {
		policies = append(policies, [2]string{Modes[id].Policy, Modes[id].Name})
	}
	options := []pyacp.SessionConfigOption{selectOption(EditApprovalPolicyConfigID, "Edits", "mode", Modes[modeID].Policy, policies)}
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
var gatewayModelState = func(ctx context.Context, info *pyjson.Object) *pyacp.SessionModelState {
	model := ""
	if v := dget(info, "model"); truthy(v) {
		model = pystr.Strip(pyStr(v))
	}
	providerName := ""
	if v := dget(info, "provider"); truthy(v) {
		providerName = pyStr(v)
	}
	if providerName == "" {
		providerName = DetectProvider(ctx)
	}
	if providerName == "" {
		providerName = "openrouter"
	}
	picker, err := func() (picker *pyacp.SessionModelState, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return BuildModelState(ctx, model, providerName, "")
	}()
	if err != nil {
		gatewayMetaLog.Debug("Could not build ACP model state\n" + err.Error())
	} else if picker != nil {
		return picker
	}
	if model == "" {
		return nil
	}
	choice := EncodeModelChoice(providerName, model)
	return &pyacp.SessionModelState{AvailableModels: []pyacp.ModelInfo{{ModelID: choice, Name: model}}, CurrentModelID: choice}
}

// Session provenance meta for the editor's id over the logical session's current compression tip
var gatewayProvenanceMeta = func(ctx context.Context, dbPath, acpSessionID, sessionID string) (*pyjson.Object, error) {
	if !pathExists(dbPath) {
		return nil, nil
	}
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		var sqliteErr *state.SQLiteError
		if errors.As(err, &sqliteErr) {
			gatewayMetaLog.Debug(fmt.Sprintf("Could not build ACP session provenance for %s\n%s", sessionID, err))
			return nil, nil
		}
		return nil, err
	}
	defer db.Close()
	tip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil {
		var sqliteErr *state.SQLiteError
		if errors.As(err, &sqliteErr) {
			gatewayMetaLog.Debug(fmt.Sprintf("Could not build ACP session provenance for %s\n%s", sessionID, err))
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
func gatewayUsageUpdate(stats *pyjson.Object) *pyacp.UsageUpdate {
	size, ok := pyIntStrict(dget(stats, "context_length"))
	if !ok || size <= 0 {
		return nil
	}
	used := 0
	if v := dget(stats, "context_tokens"); truthy(v) {
		f, _ := pyFloat(v)
		used = int(f)
	}
	return &pyacp.UsageUpdate{Size: size, Used: max(used, 0)}
}

// Integer value of a decoded JSON int, bools and floats refused
func pyIntStrict(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}

// Prompt response usage of the agent's session counters, nil when stats is not a dict
func gatewayTurnUsage(stats any) *pyacp.Usage {
	d, ok := stats.(*pyjson.Object)
	if !ok || d == nil {
		return nil
	}
	count := func(key string) int {
		if v := dget(d, key); truthy(v) {
			f, _ := pyFloat(v)
			return int(f)
		}
		return 0
	}
	usage := &pyacp.Usage{InputTokens: count("input_tokens"), OutputTokens: count("output_tokens"), TotalTokens: count("total_tokens")}
	if v := dget(d, "reasoning_tokens"); truthy(v) {
		usage.ThoughtTokens = pyacp.Ptr(count("reasoning_tokens"))
	}
	if v := dget(d, "cache_read_tokens"); truthy(v) {
		usage.CachedReadTokens = pyacp.Ptr(count("cache_read_tokens"))
	}
	return usage
}

// Session info update carrying a new title stamped now
func gatewayTitleUpdate(title string) *pyacp.SessionInfoUpdate {
	return &pyacp.SessionInfoUpdate{Title: &title, UpdatedAt: pyacp.Ptr(pydatetime.ISOFormatUTC(time.Now()))}
}
