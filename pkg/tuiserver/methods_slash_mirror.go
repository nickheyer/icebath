package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Commands that read then mutate live state a running turn uses, refused while a turn runs
var mutatesWhileRunning = map[string]bool{"model": true, "personality": true, "compress": true}

// NormalizePersonalityName is the personality name normalizer of the CLI, assigned in cmd/icebath/wiring.go
var NormalizePersonalityName func(value any) string

// RenderPersonalityPrompt renders a personality definition as its prompt, assigned in cmd/icebath/wiring.go
var RenderPersonalityPrompt func(value any) string

// PersistPersonality saves the chosen personality to config, assigned in cmd/icebath/wiring.go
var PersistPersonality func(ctx context.Context, value any) bool

// Compresses the live session and returns the user facing feedback, a held lock answering with its skip note
func compressLiveWithFeedback(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string, snapshotKwargs bool) (string, error) {
	request := agent.ParseCompressArgs(arg)
	if request.Aggressive {
		return agent.AggressiveUnsupported, nil
	}
	scope := sessionProfileRuntimeScope(ctx, session, true)
	if request.Preview {
		preview := slashRows(slashHistorySnapshot(session))
		result, err := agent.CompressNow(scope, a, preview, request, agent.CompressNowOptions{})
		if err != nil {
			return "", err
		}
		return strings.Join(agent.RenderCompressResult(result, ""), "\n"), nil
	}
	estimate := func(messages []map[string]any, prompt string, toolDefs []map[string]any) int {
		if len(messages) == 0 {
			return 0
		}
		return agent.SessionCompressAgent{}.RequestTokens(scope, messages, prompt, toolDefs)
	}
	var feedback string
	err := manualCompressTurn(scope, sid, session, func() error {
		unlock := lockHistory(session)
		beforeAny := append([]any(nil), historyList(session.Get("history"))...)
		historyVersion := slashInt(session.Get("history_version"))
		unlock()
		before := slashRows(beforeAny)
		sysPrompt := a.CachedSystemPrompt()
		toolDefs := a.ToolDefs()
		if len(toolDefs) == 0 {
			toolDefs = nil
		}
		beforeTokens := estimate(before, sysPrompt, toolDefs)
		var err error
		if snapshotKwargs {
			_, _, err = compressSessionHistory(scope, session, pystr.Strip(arg), beforeAny, &historyVersion)
		} else {
			_, _, err = compressSessionHistory(scope, session, arg, nil, nil)
		}
		var held *CompressionLockHeld
		if errors.As(err, &held) {
			feedback = agent.DescribeCompressionLockSkip(held.Holder)
			return nil
		}
		if err != nil {
			return err
		}
		syncSessionKeyAfterCompress(scope, sid, session, true, true)
		after := slashRows(slashHistorySnapshot(session))
		afterPrompt := a.CachedSystemPrompt()
		if afterPrompt == "" {
			afterPrompt = sysPrompt
		}
		afterTools := a.ToolDefs()
		if len(afterTools) == 0 {
			afterTools = toolDefs
		}
		afterTokens := estimate(after, afterPrompt, afterTools)
		tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, session))
		fb := agent.SessionCompressAgent{}.ManualSummary(before, after, beforeTokens, afterTokens, a.ContextCompressor())
		agent.FinalizeContextEngineCompressionNotification(a, true)
		var parts []string
		for _, key := range []string{"headline", "token_line", "note"} {
			if truthy(fb[key]) {
				parts = append(parts, strOr(fb[key]))
			}
		}
		feedback = strings.Join(parts, "\n")
		return nil
	})
	if err != nil {
		return "", err
	}
	return feedback, nil
}

// Name and prompt of a requested personality, an error naming the available ones when it is unknown
func validatePersonality(value string, cfg map[string]any) (string, string, error) {
	name := NormalizePersonalityName(value)
	if name == "" {
		return "", "", nil
	}
	personalities := AvailablePersonalities(cfg)
	def, ok := personalities[name]
	if !ok {
		names := make([]string, 0, len(personalities))
		for n := range personalities {
			names = append(names, n)
		}
		sort.Strings(names)
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = "`" + n + "`"
		}
		return "", "", fmt.Errorf("Unknown personality: `%s`.\n\nAvailable: `none`, %s", pystr.Strip(value), strings.Join(quoted, ", "))
	}
	return name, RenderPersonalityPrompt(def), nil
}

// Applies a personality in place with a pivot marker row, history kept, and emits the refreshed session info
func applyPersonalityToSession(ctx context.Context, sid string, session *tuigateway.Session, newPrompt, personality string) (bool, map[string]any) {
	if session == nil {
		return false, nil
	}
	session.Set("personality", personality)
	a := sessionAgent(session)
	if a == nil {
		return false, nil
	}
	a.SetEphemeralSystemPrompt(newPrompt)
	marker := "[System: The user has cleared the personality overlay. From this point forward, respond in your normal default style.]"
	if newPrompt != "" {
		marker = "[System: The user has changed the assistant's personality. From this point forward, adopt the following persona and respond accordingly: " + newPrompt + "]"
	}
	unlock := lockHistory(session)
	session.Set("history", append(historyList(session.Get("history")), map[string]any{"role": "user", "content": marker, "display_kind": "personality_switch"}))
	session.Set("history_version", slashInt(session.Get("history_version"))+1)
	unlock()
	info := sessionInfo(ctx, a, session)
	tuigateway.Emit(ctx, "session.info", sid, info)
	return false, info
}

// Service tier each fast argument selects, nil clearing the tier
var fastTiers = map[string]*string{"fast": replyText("priority"), "on": replyText("priority"), "normal": nil, "off": nil,
	"auto": replyText("auto"), "cold": replyText("cold"), "ultrafast": replyText("ultrafast"), "ultra": replyText("ultrafast")}

// Live side effect of a slash command, the warning text it returns or empty
type slashMirror func(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error)

// Repaints session info everywhere after an approvals mode write
func mirrorApprovals(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	if arg != "" {
		BroadcastSessionInfo(ctx)
	}
	return "", nil
}

// Persists the chosen personality and applies it to the live session
func mirrorPersonality(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	if arg == "" || a == nil {
		return "", nil
	}
	name, prompt, err := validatePersonality(arg, loadCfg(ctx))
	if err != nil {
		return "", err
	}
	PersistPersonality(ctx, name)
	applyPersonalityToSession(ctx, sid, session, prompt, name)
	return "", nil
}

// Sets the live service tier for a known fast argument and repaints session info
func mirrorFast(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	if a == nil {
		return "", nil
	}
	if tier, known := fastTiers[pystr.Lower(arg)]; known {
		a.SetServiceTier(tier)
	}
	tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, session))
	return "", nil
}

// Reloads MCP tools on an agent that offers the reload
func mirrorReloadMCP(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	if a == nil {
		return "", nil
	}
	if reloader, ok := any(a).(interface{ ReloadMCPTools() }); ok {
		reloader.ReloadMCPTools()
	}
	return "", nil
}

// Kills every tracked background process as a deliberate user stop
func mirrorStop(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	tools.DefaultProcessRegistry().KillAll(ctx, tools.ToolEnvFrom(ctx), "", nil, "slash.stop", false)
	return "", nil
}

// Clears the sticky low disk notice once the auto prune question is answered
func mirrorAutoprune(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
	if arg != "" && pruneAdvisorAnswered(ctx, strOr(session.Get("profile_home"))) {
		tuigateway.Emit(ctx, "notification.clear", sid, map[string]any{"key": "sessions.auto_prune"})
	}
	return "", nil
}

// Live side effect of each mirrored slash command by name
var slashMirrors map[string]slashMirror

func init() {
	slashMirrors = map[string]slashMirror{
		"model": func(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
			if arg == "" || a == nil {
				return "", nil
			}
			result, err := applyModelSwitch(ctx, sid, session, arg, modelSwitchOptions{pinSessionOverride: true, countSwitch: true})
			if err != nil {
				return "", err
			}
			return strOr(result["warning"]), nil
		},
		"approvals":   mirrorApprovals,
		"personality": mirrorPersonality,
		"compress": func(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, arg string) (string, error) {
			if a == nil {
				return "", nil
			}
			return compressLiveWithFeedback(ctx, sid, session, a, arg, false)
		},
		"fast":       mirrorFast,
		"reload-mcp": mirrorReloadMCP,
		"stop":       mirrorStop,
		"autoprune":  mirrorAutoprune,
	}
}

// Forwards a mutating slash command to the compute host, returning status and text of its ack
func computeHostSlash(ctx context.Context, sid string, session *tuigateway.Session, name, command string) (string, string) {
	routeName := "slash." + name
	isCompress := name == "compress"
	timeout := 30.0
	var onLateAck func(*pyjson.Object)
	if isCompress {
		timeout = computeHostCompressWaitSeconds(ctx, nil)
		onLateAck = func(late *pyjson.Object) {
			adoptLateComputeHostCompressAck(ctx, sid, session, objectMap(late), routeName)
		}
	}
	ack, err := sendComputeHostControl(ctx, sid, routeName, command, nil, true, timeout, onLateAck)
	if errors.Is(err, ErrHostReplyTimeout) {
		if isCompress {
			return "pending", "compression still running in the background; the transcript will refresh when it finishes"
		}
		return "failed", "compute-host " + routeName + " failed: timed out"
	}
	if err != nil {
		return "failed", "compute-host " + routeName + " failed: " + err.Error()
	}
	frame := objectMap(ack)
	if frame["type"] == "control.error" || frame["type"] == "error" {
		if truthy(frame["message"]) {
			return "rejected", strOr(frame["message"])
		}
		return "rejected", "compute-host " + routeName + " failed"
	}
	applyComputeHostMetadataMirror(ctx, session, frame)
	return "ok", strOr(frame["output"])
}

// Applies the side effects a worker slash command must also have on the live agent, returning any warning
func mirrorSlashSideEffects(ctx context.Context, sid string, session *tuigateway.Session, command string) string {
	trimmed := pystr.Strip(strings.TrimLeft(command, "/"))
	if trimmed == "" {
		return ""
	}
	name, rest := splitFirstWord(trimmed)
	arg := pystr.Strip(rest)
	a := sessionAgent(session)
	if name == "compact" {
		name = "compress"
	}
	if mutatesWhileRunning[name] {
		if sessionUsesComputeHost(ctx, session, nil) {
			_, text := computeHostSlash(ctx, sid, session, name, command)
			return text
		}
		if truthy(session.Get("running")) {
			return busyMessage(name, truthy(session.Get("_manual_compress_active")))
		}
	}
	mirror, ok := slashMirrors[name]
	if !ok {
		return ""
	}
	var out string
	var failure any
	func() {
		defer func() {
			if r := recover(); r != nil {
				failure = r
			}
		}()
		text, err := mirror(sessionProfileRuntimeScope(ctx, session, true), sid, session, a, arg)
		if err != nil {
			failure = err
			return
		}
		out = text
	}()
	if failure == nil {
		return out
	}
	var busy *CompressionBusy
	if err, isErr := failure.(error); isErr && errors.As(err, &busy) {
		return busy.Error()
	}
	if name == "compress" && a != nil {
		agent.FinalizeContextEngineCompressionNotification(a, false)
	}
	return "live session sync failed: " + failureText(failure)
}
