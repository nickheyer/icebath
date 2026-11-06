package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// PreludeStep is one scripted assistant row, its text and the single tool call it carries
type PreludeStep struct {
	Content  string
	ToolName string
	Args     any
}

// Prelude yields scripted steps, each Next receiving the previous step's tool result or nil
type Prelude interface {
	Next(result *string) (PreludeStep, bool)
	Close()
}

// Call id seeded by the session, the call and its place in history, byte stable on replay
func preludeCallID(sessionID, name string, args any, index int) (string, error) {
	encoded, err := pyjson.Dumps(args, pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	return tools.DeterministicCallID(name, sessionID+":"+encoded, index), nil
}

// Tool call of a prelude step with dict arguments JSON encoded and anything else as its str
func buildPreludeToolCall(id, name string, args any) (state.ToolCall, error) {
	var argsText string
	switch v := args.(type) {
	case map[string]any, *pyjson.Object:
		var err error
		if argsText, err = pyjson.Dumps(v); err != nil {
			return state.ToolCall{}, err
		}
	default:
		argsText = pyrepr.Str(v)
	}
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: argsText}}, nil
}

// Plays the prelude through the tool round, returning the last round's verdict or nil when no step ran
func runScriptedPrelude(ctx context.Context, a *AIAgent, s *LoopState, prelude Prelude) (*PhaseVerdict, error) {
	defer prelude.Close()
	var verdict *PhaseVerdict
	var result *string
	for !a.interruptRequested {
		step, ok := prelude.Next(result)
		if !ok {
			break
		}
		s.FinishReason = "tool_calls"
		id, err := preludeCallID(a.sessionID, step.ToolName, step.Args, len(s.Messages))
		if err != nil {
			return nil, err
		}
		call, err := buildPreludeToolCall(id, step.ToolName, step.Args)
		if err != nil {
			return nil, err
		}
		s.AssistantMessage = &provider.Response{FinishReason: "tool_calls",
			Message: &state.Message{Role: "assistant", Content: step.Content, ToolCalls: []state.ToolCall{call}}}
		v, err := runToolRound(ctx, a, s)
		if err != nil {
			return nil, err
		}
		verdict = &v
		if v.Action != "continue" {
			break
		}
		result = nil
		if n := len(s.Messages); n > 0 && s.Messages[n-1] != nil && s.Messages[n-1].Role == "tool" {
			text := pyrepr.Str(s.Messages[n-1].Content)
			result = &text
		}
	}
	return verdict, nil
}

// Return ends the turn now, break skips the model call, run runs the loop as usual including with no prelude
func playPrelude(ctx context.Context, a *AIAgent, s *LoopState, prelude Prelude) (PhaseVerdict, error) {
	if prelude == nil {
		return PhaseVerdict{Action: "run"}, nil
	}
	verdict, err := runScriptedPrelude(ctx, a, s, prelude)
	if err != nil {
		return PhaseVerdict{}, err
	}
	if verdict != nil && (verdict.Action == "return" || verdict.Action == "break") {
		return *verdict, nil
	}
	return PhaseVerdict{Action: "run"}, nil
}
