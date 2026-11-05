package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// OuterLoopError is an error escaping the response processing block with the source module of every frame it unwound through
type OuterLoopError struct {
	Err     error
	Modules map[string]bool
	Stack   string
}

func (e *OuterLoopError) Error() string { return e.Err.Error() }

func (e *OuterLoopError) Unwrap() error { return e.Err }

// Turns a recovered panic into an outer loop error carrying the modules of the panicking stack
func RecoveredOuterLoopError(r any) *OuterLoopError {
	err, ok := r.(error)
	if !ok {
		err = fmt.Errorf("%v", r)
	}
	pcs := make([]uintptr, 128)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	out := &OuterLoopError{Err: err, Modules: map[string]bool{}}
	var stack strings.Builder
	for {
		frame, more := frames.Next()
		if frame.File != "" {
			out.Modules[strings.TrimSuffix(filepath.Base(frame.File), filepath.Ext(frame.File))] = true
			fmt.Fprintf(&stack, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		}
		if !more {
			break
		}
	}
	out.Stack = stack.String()
	return out
}

// OuterErrorVerdict is break when the turn ends with its exit reason and final response set, fallthrough to retry
type OuterErrorVerdict struct {
	Action          string
	OuterErrorCount int
	TurnExitReason  any
	Failed          bool
	FinalResponse   any
}

// Classifies an escaped loop exception, fills unanswered tool results and ends the turn once errors are deterministic or capped
func (a *AIAgent) handleOuterLoopError(ctx context.Context, e error, outerErrorCount, apiCallCount int, messages *[]*state.Message,
	conversationHistory []*state.Message, turnExitReason any, failed bool, finalResponse any) (OuterErrorVerdict, error) {
	log := conversationLoopLog()
	verdict := func(action string) OuterErrorVerdict {
		return OuterErrorVerdict{Action: action, OuterErrorCount: outerErrorCount, TurnExitReason: turnExitReason,
			Failed: failed, FinalResponse: finalResponse}
	}
	outerErrorCount++

	if interpreterFinalizing() || isInterpreterShutdownError(e) {
		errorMsg := fmt.Sprintf("Interpreter is shutting down — cannot continue (API call #%d): %s", apiCallCount, e)
		a.safePrint("❌ "+errorMsg, true)
		log.Warn(errorMsg)
		_ = a.persistSession(*messages, conversationHistory)
		turnExitReason = "interpreter_shutdown"
		failed = true
		resume := ""
		if a.sessionID != "" {
			resume = " (CLI: `icebath --resume " + a.sessionID + "`)"
		}
		text, err := tools.SiteCopy(ctx, "interpreter_shutdown", map[string]any{"resume": resume})
		if err != nil {
			return OuterErrorVerdict{}, err
		}
		finalResponse = text
		return verdict("break"), nil
	}

	tbModules := map[string]bool{}
	var traced *OuterLoopError
	if errors.As(e, &traced) {
		tbModules = traced.Modules
	}
	isLocal := intersects(tbModules, localProcessingModules) && !intersects(tbModules, apiCallModules)
	errorMsg := fmt.Sprintf("Error during API call #%d: %s", apiCallCount, e)
	if isLocal {
		errorMsg = fmt.Sprintf("Error during local message processing after API call #%d: %s", apiCallCount, e)
	}
	if a.suppressStatusOutput {
		log.Error(errorMsg)
	} else {
		a.safePrint("❌ "+errorMsg, true)
	}
	stack := ""
	if traced != nil {
		stack = traced.Stack
	}
	log.Error(fmt.Sprintf("Outer loop error in API call #%d", apiCallCount), "error", e.Error(), "traceback", stack)

	rows := *messages
	for idx := len(rows) - 1; idx >= 0; idx-- {
		msg := rows[idx]
		if msg == nil {
			break
		}
		if msg.Role == "tool" {
			continue
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			answered := map[string]bool{}
			for _, m := range rows[idx+1:] {
				if m != nil && m.Role == "tool" {
					answered[m.ToolCallID] = true
				}
			}
			for _, tc := range msg.ToolCalls {
				if answered[tc.ID] {
					continue
				}
				scratch := []map[string]any{}
				row := state.AppendMessage(&scratch, map[string]any{
					"role":         "tool",
					"name":         getToolCallNameStatic(tc),
					"tool_call_id": tc.ID,
					"content":      "Error executing tool: " + errorMsg,
				}, nil)
				*messages = append(*messages, state.MessageFromDict(row))
			}
		}
		break
	}

	outerErrorCap := min(maxOuterLoopErrors, max(1, a.maxIterations))
	if isLocal || apiCallCount >= a.maxIterations-1 || outerErrorCount >= outerErrorCap {
		detail := ShortDetail(e, 200)
		head := pystr.Runes(errorMsg)
		head = head[:min(len(head), 80)]
		code := "loop_error"
		switch {
		case isLocal:
			turnExitReason = "local_processing_error(" + pystr.FromRunes(head) + ")"
			code = "local_processing_error"
		case outerErrorCount >= outerErrorCap:
			failed = true
			turnExitReason = "repeated_outer_errors(" + pystr.FromRunes(head) + ")"
		default:
			turnExitReason = "error_near_max_iterations(" + pystr.FromRunes(head) + ")"
		}
		text, err := tools.SiteCopy(ctx, code, map[string]any{"detail": detail})
		if err != nil {
			return OuterErrorVerdict{}, err
		}
		finalResponse = text
		return verdict("break"), nil
	}
	return verdict("fallthrough"), nil
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
