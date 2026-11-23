package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// NormalizePersonalityName is the personality name normalizer of the CLI, assigned in cmd/icebath/wiring.go
var NormalizePersonalityName func(value any) string

// RenderPersonalityPrompt renders a personality definition as its prompt, assigned in cmd/icebath/wiring.go
var RenderPersonalityPrompt func(value any) string

// Compresses the live session and returns the user facing feedback, a held lock answering with its skip note
func compressLiveWithFeedback(ctx context.Context, sid string, session *tuihost.Session, a *agent.AIAgent, arg string, snapshotKwargs bool) (string, error) {
	request := agent.ParseCompressArgs(arg)
	scope := sessionProfileRuntimeScope(ctx, session, true)
	if request.Preview {
		preview := slashRows(slashHistorySnapshot(session))
		result, err := compressNow(scope, a, preview, request, agent.CompressNowOptions{})
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
		syncSessionKeyAfterCompress(scope, sid, session, true)
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
		tuihost.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, session))
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
func applyPersonalityToSession(ctx context.Context, sid string, session *tuihost.Session, newPrompt, personality string) (bool, map[string]any) {
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
	tuihost.Emit(ctx, "session.info", sid, info)
	return false, info
}

// Compresses the live session for /compress and returns its feedback, the busy notice while a turn runs
func liveCompressOutput(ctx context.Context, sid string, session *tuihost.Session, arg string) string {
	if truthy(session.Get("running")) {
		return busyMessage("compress", truthy(session.Get("_manual_compress_active")))
	}
	a := sessionAgent(session)
	if a == nil {
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
		text, err := compressLiveWithFeedback(sessionProfileRuntimeScope(ctx, session, true), sid, session, a, pystr.Strip(arg), false)
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
	agent.FinalizeContextEngineCompressionNotification(a, false)
	return "live session sync failed: " + failureText(failure)
}
