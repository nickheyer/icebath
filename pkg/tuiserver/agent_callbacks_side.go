package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Parent agent surface a side agent copies its route, toolsets and store from
type sideAgentParent interface {
	BaseURL() string
	APIKey() string
	Provider() string
	APIMode() string
	ACPCommand() string
	ACPArgs() []string
	EphemeralSystemPrompt() string
	ProviderRouting() agent.ProviderRouting
	Model() string
	EnabledToolsets() []string
	DisabledToolsets() []string
	ReasoningConfig() map[string]any
	ServiceTier() string
	RequestOverrides() map[string]any
	SessionStore() agent.SessionStore
	FallbackChain() []map[string]any
}

var _ sideAgentParent = (*agent.AIAgent)(nil)

// Construction options of a detached side agent on the parent agent's route, persisting into the parent's store
func backgroundAgentKwargs(ctx context.Context, parent sideAgentParent, taskID string) agent.AIAgentOptions {
	cfg := loadCfg(ctx)
	fallback := parent.FallbackChain()
	if fallback == nil {
		fallback = []map[string]any{}
	}
	routing := parent.ProviderRouting()
	model := parent.Model()
	if model == "" {
		model = resolveModel(ctx)
	}
	enabled := parent.EnabledToolsets()
	if len(enabled) == 0 {
		enabled = loadEnabledToolsets(ctx, "tui")
	}
	disabled := parent.DisabledToolsets()
	if len(disabled) == 0 {
		disabled = loadDisabledToolsets(ctx)
	}
	reasoning := parent.ReasoningConfig()
	if len(reasoning) == 0 {
		reasoning = loadReasoningConfig(ctx, parent.Model())
	}
	tier := parent.ServiceTier()
	if tier == "" {
		tier = loadServiceTier(ctx)
	}
	overrides := map[string]any{}
	for key, value := range parent.RequestOverrides() {
		overrides[key] = value
	}
	var store agent.SessionStore
	if parentStore := parent.SessionStore(); parentStore != nil && !isNilStore(parentStore) {
		store = parentStore
	} else if db := getDB(ctx); db != nil {
		store = db
	}
	return agent.AIAgentOptions{BaseURL: parent.BaseURL(), APIKey: parent.APIKey(), Provider: parent.Provider(),
		APIMode: parent.APIMode(), ACPCommand: parent.ACPCommand(), ACPArgs: parent.ACPArgs(),
		EphemeralSystemPrompt: parent.EphemeralSystemPrompt(), ProvidersAllowed: routing.Allowed, ProvidersIgnored: routing.Ignored,
		ProvidersOrder: routing.Order, ProviderSort: routing.Sort, ProviderDataCollection: routing.DataCollection,
		OpenRouterMinCodingScore: routing.MinCodingScore, Model: model, MaxIterations: cfgMaxTurns(cfg, 25),
		EnabledToolsets: enabled, DisabledToolsets: disabled, QuietMode: true, VerboseLogging: false,
		ProviderRequireParameters: routing.RequireParameters, SessionID: taskID, ReasoningConfig: reasoning, ServiceTier: tier,
		RequestOverrides: overrides, Platform: "tui", SessionDB: store, FallbackModel: fallback, SideAgent: true}
}

// Whether a session store interface holds a nil pointer
func isNilStore(store agent.SessionStore) bool {
	db, isDB := store.(*state.DB)
	return isDB && db == nil
}

// Options of the hidden preview restart agent, terminal and file tools only with no store and no memory
func ephemeralPreviewAgentKwargs(ctx context.Context, parent sideAgentParent, taskID string) agent.AIAgentOptions {
	opts := backgroundAgentKwargs(ctx, parent, taskID)
	opts.EnabledToolsets = []string{"terminal", "file"}
	opts.SessionDB = nil
	opts.SkipMemory = true
	return opts
}

// Side agent's own registry reference on the parent's store for its turn, released by the returned call
func sideAgentSessionDB(ctx context.Context, parentStore agent.SessionStore) (agent.SessionStore, func(), error) {
	db, isDB := parentStore.(*state.DB)
	if parentStore == nil || !isDB || db == nil || db.Path() == "" {
		return parentStore, func() {}, nil
	}
	acquired, err := state.Acquire(ctx, db.Path())
	if err != nil {
		return nil, nil, err
	}
	return acquired, func() { state.ReleaseOrClose(acquired) }, nil
}

// Tail of a tool result the restart panel shows under its summary, empty when the result is no object
func previewToolResultPreview(name, result string) string {
	decoded, err := pyjson.Loads(result)
	if err != nil {
		return ""
	}
	data, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return ""
	}
	text := func(key string) string {
		value := data.GetOr(key, nil)
		if !pyrepr.Truthy(value) {
			return ""
		}
		return pyrepr.Str(value)
	}
	if name == "terminal" {
		if output := pystr.Strip(text("output")); output != "" {
			return pystr.Slice(output, -1200, pystr.Len(output))
		}
		if pyrepr.Truthy(data.GetOr("session_id", nil)) {
			return "Background process started: " + pyrepr.Str(data.GetOr("session_id", nil))
		}
		if code := data.GetOr("exit_code", nil); code != nil {
			return "terminal exited with code " + pyrepr.Str(code)
		}
	}
	return pystr.Slice(pystr.Strip(text("error")), 0, 1200)
}

// Restart panel callbacks of the hidden preview agent, every tool step rendered as a progress row
type previewRestartCallbackSet struct {
	ToolStart    func(toolCallID, name string, args map[string]any)
	ToolComplete func(toolCallID, name string, args map[string]any, result string)
	ToolProgress func(event, name, preview string, args map[string]any, extra map[string]any)
	ToolGen      func(name string)
	Status       func(kind string, text any)
}

// Copies the restart panel callbacks into agent construction options
func (c previewRestartCallbackSet) applyTo(o *agent.AIAgentOptions) {
	o.ToolStartCallback, o.ToolCompleteCallback, o.ToolProgressCallback = c.ToolStart, c.ToolComplete, c.ToolProgress
	o.ToolGenCallback, o.StatusCallback = c.ToolGen, c.Status
}

// Callbacks of the preview restart agent emitting preview.restart.progress rows on the parent session
func previewRestartCallbacks(ctx context.Context, parent, taskID string) previewRestartCallbackSet {
	var mu sync.Mutex
	startedAt := map[string]float64{}
	progress := func(message any, level string) {
		if text := pystr.Strip(pyrepr.Str(orEmpty(message))); text != "" {
			tuigateway.Emit(ctx, "preview.restart.progress", parent, map[string]any{"task_id": taskID, "level": level, "text": text})
		}
	}
	now := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	return previewRestartCallbackSet{
		ToolStart: func(toolCallID, name string, args map[string]any) {
			mu.Lock()
			startedAt[toolCallID] = now()
			mu.Unlock()
			line := "Running " + name
			if toolCtx := tuigateway.ToolCtx(name, args); toolCtx != "" {
				line += ": " + toolCtx
			}
			progress(line, "info")
		},
		ToolComplete: func(toolCallID, name string, _ map[string]any, result string) {
			end := now()
			mu.Lock()
			started, known := startedAt[toolCallID]
			mu.Unlock()
			if !known {
				started = end
			}
			duration := end - started
			summary := toolSummary(name, result, &duration)
			if summary == "" {
				summary = "Finished " + name
				if duration != 0 {
					summary += " in " + fmtToolDuration(&duration)
				}
			}
			if output := previewToolResultPreview(name, result); output != "" {
				summary += "\n" + output
			}
			progress(summary, "info")
		},
		ToolProgress: func(event, name, preview string, _ map[string]any, _ map[string]any) {
			if preview == "" && name == "" {
				return
			}
			if preview != "" {
				progress(preview, "info")
				return
			}
			progress(fmt.Sprintf("%s: %s", strings.ReplaceAll(event, ".", " "), name), "info")
		},
		ToolGen: func(name string) { progress("Preparing "+name, "info") },
		Status:  restartStatusFactory(ctx, parent, progress),
	}
}

// Restart panel status rows, an automatic warning dropped when the TUI warning policy mutes it
func restartStatusFactory(ctx context.Context, parent string, progress func(message any, level string)) func(kind string, text any) {
	return func(kind string, text any) {
		message := text
		if text == nil {
			message = kind
		}
		if config.IsWarningStatus(kind, message) && !agentPresentationEnabled(ctx, parent, true) {
			return
		}
		progress(message, "info")
	}
}
