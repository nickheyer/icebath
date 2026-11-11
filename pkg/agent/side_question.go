package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	sideQuestionTask          = "side_question"
	sideQuestionForkMaxIters  = 3
	sideQuestionPerMessageCap = 2000
	sideQuestionTranscriptCap = 24000
)

const sideQuestionForkPrompt = "The user asked a quick SIDE question with /btw while the main work continues in the original " +
	"session.\nRules:\n- Answer ONLY the side question, using the conversation above as context. Do not continue, " +
	"redo, or critique the main task.\n- Do NOT call any tools — they are disabled for this side question. Answer " +
	"directly in text.\n- If the conversation does not contain enough information to answer, say so plainly instead " +
	"of guessing.\n- Be concise and direct."

const sideQuestionOneshotInstructions = "You are the same AI assistant that is currently working inside the conversation transcribed below. The user " +
	"has asked a quick SIDE question with /btw while the main work continues.\nRules:\n- Answer ONLY the side " +
	"question. Do not continue, redo, or critique the main task.\n- Use the transcript as your primary context; it " +
	"is a snapshot and may not include the very latest activity.\n- If the transcript does not contain enough " +
	"information to answer, say so plainly instead of guessing.\n- Be concise and direct."

var sideQuestionRoleLabels = map[string]string{"user": "USER", "assistant": "ASSISTANT", "tool": "TOOL RESULT"}

var sideQuestionLog = slog.Default().With("logger", "agent.side_question")

// Plain transcript fitted newest first to the budget, tool calls named, results truncated, system rows skipped
func RenderHistoryForSideQuestion(history []map[string]any, charBudget int) string {
	var lines []string
	for _, msg := range history {
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		text := reviewMsgText(msg)
		if calls, ok := msg["tool_calls"].([]any); role == "assistant" && ok && len(calls) > 0 {
			var names []string
			for _, tc := range calls {
				call, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				name := "?"
				if v, ok := fn["name"]; ok {
					name = pyrepr.Str(v)
				}
				names = append(names, name)
			}
			lines = append(lines, "ASSISTANT [called tools: "+strings.Join(names, ", ")+"]")
		}
		if label, ok := sideQuestionRoleLabels[role]; ok && text != "" {
			lines = append(lines, label+": "+pystr.Slice(text, 0, sideQuestionPerMessageCap))
		}
	}
	var kept []string
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if used+pystr.Len(line)+1 > charBudget && len(kept) > 0 {
			break
		}
		kept = append(kept, line)
		used += pystr.Len(line) + 1
	}
	if len(kept) == 0 {
		return "(no prior conversation)"
	}
	prefix := ""
	if len(kept) < len(lines) {
		prefix = "[...older conversation omitted...]\n"
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return prefix + strings.Join(kept, "\n")
}

// The auxiliary side_question block of the config, empty on failure or a shape mismatch
func sideQuestionTaskConfig(ctx context.Context) map[string]any {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return map[string]any{}
	}
	aux, ok := tree["auxiliary"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	task, ok := aux[sideQuestionTask].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return task
}

// Answers through a cache parity fork of the parent with every tool denied, publishing the fork to onFork
func answerViaFork(ctx context.Context, parent *AIAgent, question string, history []map[string]any, onFork func(*AIAgent)) (answer string, err error) {
	fork, _, routed, err := buildCacheParityFork(ctx, parent, sideQuestionTaskConfig(ctx), sideQuestionForkMaxIters, sideQuestionOrigin)
	if err != nil {
		return "", err
	}
	if onFork != nil {
		onFork(fork)
	}
	defer func() {
		releaseCacheParityFork(ctx, parent, fork, sideQuestionTask, "")
		if onFork != nil {
			onFork(nil)
		}
	}()
	limited := plugins.WithToolWhitelist(ctx, map[string]bool{},
		"Side question (/btw) denied tool call: {tool_name}. "+
			"Tools are disabled here — answer directly from the conversation context.")
	snapshot := trimSnapshotForFork(history)
	if routed {
		snapshot = digestHistory(snapshot, 24)
	}
	result, err := fork.RunConversation(limited, sideQuestionForkPrompt+"\n\nSide question: "+question,
		RunConversationOptions{ConversationHistory: snapshot})
	if err != nil {
		return "", err
	}
	text := ""
	if v, ok := result["final_response"]; ok && pyrepr.Truthy(v) {
		text = pyrepr.Str(v)
	}
	if text == "" && result != nil && pyrepr.Truthy(result["error"]) {
		return "", errors.New(pyrepr.Str(result["error"]))
	}
	return pystr.Strip(text), nil
}

// Answers from a rendered transcript digest in one auxiliary call
func answerViaOneshot(ctx context.Context, question string, history []map[string]any, req OneshotRequest) (string, error) {
	req.Instructions = sideQuestionOneshotInstructions
	req.UserInput = "Conversation transcript (snapshot):\n-----\n" + RenderHistoryForSideQuestion(history, sideQuestionTranscriptCap) +
		"\n-----\n\nSide question: " + question
	req.Task = sideQuestionTask
	return RunOneshot(ctx, req)
}

// Options of one side question, zero MaxTokens and Timeout taking 2048 and 180 seconds
type SideQuestionOptions struct {
	Parent      *AIAgent
	MainRuntime map[string]any
	MaxTokens   int
	Temperature *float64
	Timeout     float64
	OnFork      func(*AIAgent)
	ShouldStop  func() bool
}

// Default side question options, temperature 0.3
func NewSideQuestionOptions() SideQuestionOptions {
	temp := 0.3
	return SideQuestionOptions{MaxTokens: 2048, Temperature: &temp, Timeout: 180.0}
}

// Answers a question about the conversation from a fork of the live parent, else a one shot transcript call
func AnswerSideQuestion(ctx context.Context, question string, history []map[string]any, o SideQuestionOptions) (string, error) {
	question = pystr.Strip(question)
	if question == "" {
		return "", errors.New("ValueError: answer_side_question requires a non-empty question")
	}
	if o.Parent != nil {
		answer, err := answerViaFork(ctx, o.Parent, question, history, o.OnFork)
		switch {
		case err != nil:
			sideQuestionLog.Warn("/btw cache-parity fork failed; falling back to one-shot", "exc_info", err)
		case answer != "":
			return answer, nil
		default:
			sideQuestionLog.Warn("/btw fork returned an empty answer; falling back to one-shot")
		}
	}
	if o.ShouldStop != nil && o.ShouldStop() {
		return "", errors.New("side question stopped")
	}
	return answerViaOneshot(ctx, question, history, OneshotRequest{
		MainRuntime: o.MainRuntime, MaxTokens: o.MaxTokens, Temperature: o.Temperature, Timeout: o.Timeout,
	})
}
