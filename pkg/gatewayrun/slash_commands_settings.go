package gatewayrun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Busy input mode to its status card and set confirmation behavior keys
var busyModeBehavior = map[string][2]string{
	"queue":     {"gateway.busy.behavior_queue_short", "gateway.busy.behavior_queue_long"},
	"steer":     {"gateway.busy.behavior_steer_short", "gateway.busy.behavior_steer_long"},
	"interrupt": {"gateway.busy.behavior_interrupt_short", "gateway.busy.behavior_interrupt_long"},
}

// Footer argument to the new enabled state, an empty argument toggling
var footerStateByArg = map[string]bool{"on": true, "enable": true, "true": true, "1": true,
	"off": false, "disable": false, "false": false, "0": false}

// Profile approval mode shown or persisted with its reply message, assigned in cmd/icebath wiring
var RunApprovalModeCommand func(ctx context.Context, h string, env *tools.Env, requestedMode string) (message string, err error)

// Profile scoped tools environment of the gateway
func slashToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Reviews pending memory writes and toggles their approval gate against a fresh on disk store
func (r *GatewayRunner) handleMemoryCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	env := slashToolsEnv(ctx)
	store, err := memory.LoadOnDiskStore(ctx, env)
	if err != nil {
		return "", err
	}
	setMode, err := r.writeApprovalSetter(ctx, "memory", event)
	if err != nil {
		return "", err
	}
	out, handled, err := memory.HandlePendingSubcommand(ctx, env, tools.WriteApprovalMemory,
		strings.Fields(strings.TrimSpace(event.GetCommandArgs())), memory.PendingOptions{MemoryStore: store, SetMode: setMode})
	if err != nil {
		return "", err
	}
	if !handled {
		return i18n.T(ctx, "gateway.memory.unknown_subcommand", "", nil), nil
	}
	return out, nil
}

// Reviews pending skill writes, answering while staged writes remain after the gate turned off
func (r *GatewayRunner) handleSkillsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	env := slashToolsEnv(ctx)
	args := strings.Fields(strings.TrimSpace(event.GetCommandArgs()))
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	gateOff := !tools.WriteApprovalEnabled(env, tools.WriteApprovalSkills) && sub != "approval" && sub != "mode"
	if gateOff && tools.PendingCount(env, tools.WriteApprovalSkills) == 0 {
		return i18n.T(ctx, "gateway.skills.gate_off", "", nil), nil
	}
	setMode, err := r.writeApprovalSetter(ctx, "skills", event)
	if err != nil {
		return "", err
	}
	out, handled, err := memory.HandlePendingSubcommand(ctx, env, tools.WriteApprovalSkills, args, memory.PendingOptions{
		ApplySkill: func(payload map[string]any) string { return skills.ApplySkillPending(ctx, env, payload) },
		SetMode:    setMode})
	if err != nil {
		return "", err
	}
	if !handled {
		return i18n.T(ctx, "gateway.skills.unknown_subcommand", "", nil), nil
	}
	if sub == "diff" && utf8.RuneCountInString(out) > 3000 {
		pendingID := "<id>"
		if len(args) > 1 {
			pendingID = args[1]
		}
		out = textutil.First(out, 3000) + i18n.T(ctx, "gateway.skills.diff_truncated", "", map[string]any{"pending_id": pendingID})
	}
	return out, nil
}

// Shows the profile approval mode or persists a new one, admins only when the slash gate is configured
func (r *GatewayRunner) handleApprovalsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	requested := strings.TrimSpace(event.GetCommandArgs())
	policy := gateway.PolicyForRunnerSource(r, event.Source)
	userID := ""
	if event.Source.UserID != nil {
		userID = *event.Source.UserID
	}
	if requested != "" && !policy.IsAdmin(userID) {
		return i18n.T(ctx, "gateway.approvals.admin_only", "", nil), nil
	}
	return RunApprovalModeCommand(ctx, home.Dir(ctx), slashToolsEnv(ctx), requested)
}

// Toggles the session's dangerous command approval bypass, persisted on its routing entry
func (r *GatewayRunner) handleYoloCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	persisted := false
	var persist func(on bool) error
	if store := r.sessionStore; store != nil {
		entry, err := store.GetOrCreateSession(ctx, event.Source, false, true)
		if err != nil {
			return nil, err
		}
		persisted = entry.Yolo != nil && *entry.Yolo
		persist = func(on bool) error {
			_, err := store.SetSessionYolo(ctx, sessionKey, &on)
			return err
		}
	}
	key := "gateway.yolo.disabled"
	if tools.ToggleSessionYolo(ctx, sessionKey, nil, persisted, persist) {
		key = "gateway.yolo.enabled"
	}
	return gateway.NewEphemeralReply(i18n.T(ctx, key, "", nil), nil), nil
}

// Cycles the platform's tool progress display mode and saves it, gated for messaging chats
func (r *GatewayRunner) handleVerboseCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	platformKey := platformConfigKey(event.Source.Platform)
	live := gateway.LocalLive(r, event.Source)
	if !settings.DisplayToolProgressCommand.Get(ctx) && live == nil {
		return i18n.T(ctx, "gateway.verbose.not_enabled", "", nil), nil
	}
	cycle := []string{"off", "new", "all", "verbose"}
	if live == nil {
		cycle = append(cycle, "log")
	}
	current := ""
	if own, ok := gateway.SessionOwnSetting(ctx, live, settings.ToolProgress.Key); ok {
		current, _ = own.(string)
	}
	if current == "" {
		current = settings.ToolProgress.Get(settings.WithPlatform(ctx, platformKey))
	}
	idx := slices.Index(cycle, current)
	if idx < 0 {
		idx = slices.Index(cycle, "all")
	}
	newMode := cycle[(idx+1)%len(cycle)]
	if live != nil && newMode != "log" {
		if _, err := settings.Set(settings.WithSession(ctx, live.Source.ChatID), settings.ToolProgress.Key, newMode, settings.Session); err != nil {
			pkgLog().Warn("could not keep the session's tool progress", "err", err)
		}
	}
	description := i18n.T(ctx, "gateway.verbose.mode_"+newMode, "", nil)
	if _, err := settings.Set(ctx, settings.PlatformDisplay.EntryKey(platformKey)+".tool_progress", newMode, settings.Global); err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to save tool_progress mode: %s", pyrepr.ExceptionMessage(err)))
		return description + "\n" + i18n.T(ctx, "gateway.verbose.save_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return description + "\n" + i18n.T(ctx, "gateway.verbose.saved_suffix", "", map[string]any{"platform": platformKey}), nil
}

// Shows or sets what a message sent while the agent works does, queue, steer or interrupt
func (r *GatewayRunner) handleBusyCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	arg := strings.ToLower(strings.TrimSpace(event.GetCommandArgs()))
	live := gateway.LocalLive(r, event.Source)
	if arg == "" || arg == "status" {
		var mode string
		if live != nil {
			m, err := gateway.SessionBusyMode(ctx, r, live)
			if err != nil {
				return nil, err
			}
			mode = m
		} else {
			mode = r.effectiveBusyInputMode(ctx, event.Source)
		}
		behavior, ok := busyModeBehavior[mode]
		if !ok {
			behavior = busyModeBehavior["interrupt"]
		}
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.busy.status", "", map[string]any{"mode": mode,
			"behavior": i18n.T(ctx, behavior[0], "", nil)}), nil), nil
	}
	behavior, ok := busyModeBehavior[arg]
	if !ok {
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.busy.unknown_mode", "", map[string]any{"arg": arg}), nil), nil
	}
	if !SaveConfigValue(ctx, "display.busy_input_mode", arg) {
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.busy.save_failed", "", nil), nil), nil
	}
	if live != nil {
		if _, err := settings.Set(settings.WithSession(ctx, live.Source.ChatID), settings.BusyMode.Key, arg, settings.Session); err != nil {
			pkgLog().Warn("could not keep the session's busy mode", "err", err)
		}
	}
	if profileName := r.busyProfileNameForSource(ctx, event.Source); profileName != "" {
		r.snapshotProfileBusyModes(ctx, profileName)
	} else {
		r.busyInputMode = arg
		r.busyTextMode = loadBusyTextMode(ctx)
	}
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	if adapter != nil {
		adapter.Base().SetBusyTextMode(r.effectiveBusyTextMode(ctx, event.Source))
	}
	return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.busy.set", "", map[string]any{"mode": arg,
		"behavior": i18n.T(ctx, behavior[1], "", nil)}), nil), nil
}

// Shows or toggles the runtime metadata footer, a local session saving it under its own platform key
func (r *GatewayRunner) handleFooterCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	platformKey := platformConfigKey(event.Source.Platform)
	arg := strings.ToLower(strings.TrimSpace(event.GetCommandArgs()))
	userConfig := gateway.LoadGatewayConfigMap(ctx, "")
	enabled, fields := gateway.ResolveFooterConfig(ctx, platformKey)
	state := func(on bool) string {
		if on {
			return i18n.T(ctx, "gateway.footer.state_on", "", nil)
		}
		return i18n.T(ctx, "gateway.footer.state_off", "", nil)
	}
	if arg == "status" || arg == "?" {
		return i18n.T(ctx, "gateway.footer.status", "", map[string]any{"state": state(enabled),
			"fields": strings.Join(fields, ", "), "platform": platformKey}), nil
	}
	newState, known := footerStateByArg[arg]
	if arg != "" && !known {
		return i18n.T(ctx, "gateway.footer.usage", "", nil), nil
	}
	if arg == "" {
		newState = !enabled
	}
	key := settings.ReplyFooter.Key
	if event.Source.Platform == gateway.PlatformLocal {
		key = settings.PlatformDisplay.EntryKey(platformKey) + ".runtime_footer.enabled"
	}
	if _, err := settings.Set(ctx, key, newState, settings.Global); err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to save runtime_footer.enabled: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.config_save_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	example := ""
	if newState {
		previewFields := fields
		if len(previewFields) == 0 {
			previewFields = []string{"model", "context_pct", "cwd"}
		}
		line, err := gateway.FormatRuntimeFooter(ctx, gateway.FooterInputs{Model: gateway.ResolveGatewayModel(ctx, userConfig)}, previewFields)
		if err != nil {
			return "", err
		}
		if line != "" {
			example = i18n.T(ctx, "gateway.footer.example_line", "", map[string]any{"preview": line})
		}
	}
	return i18n.T(ctx, "gateway.footer.saved", "", map[string]any{"state": state(newState), "example": example}), nil
}
