package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What the pre_tool_call hooks made of a call
type Directive struct {
	// Why a hook blocked it, empty when none did
	Block string
	// Approval of the user a hook asks for before it runs, nil when none does
	Approve *Ask
	// Arguments the hooks rewrote it to, nil when none did
	Args map[string]any
}

// Approval a hook asks for: what the user is told, and the name the answer is remembered under, the tool's name when empty
type Ask struct {
	Message string
	Rule    string
}

// Runs the pre_tool_call hooks: the first block that says why wins, else the first approval asked for; the arguments every modify gives are
// laid over the call's in turn
func (h *Hooks) PreToolCall(ctx context.Context, cfg *config.Config, call ToolCall) Directive {
	var d Directive
	for _, r := range h.Fire(ctx, cfg, call) {
		switch r.Action {
		case Modify:
			if len(r.Args) == 0 {
				continue
			}
			merged := maps.Clone(d.Args)
			if merged == nil {
				merged = maps.Clone(call.Args)
			}
			if merged == nil {
				merged = map[string]any{}
			}
			maps.Copy(merged, r.Args)
			d.Args = merged
		case Block:
			if r.Message != "" {
				d.Block = r.Message
				return d
			}
		case Approve:
			if d.Approve == nil {
				d.Approve = &Ask{Message: r.Message, Rule: strings.TrimSpace(r.Rule)}
			}
		}
	}
	return d
}

// Text the first hook that replaces it hands back for a transform point, false when none does
func replaced(results []Result) (string, bool) {
	for _, r := range results {
		if r.Replace != nil {
			return *r.Replace, true
		}
	}
	return "", false
}

// Runs the transform_tool_result hooks: the result the first that replaces it gives, false when none does
func (h *Hooks) TransformResult(ctx context.Context, cfg *config.Config, result ToolResult) (string, bool) {
	return replaced(h.Fire(ctx, cfg, result))
}

// Runs the transform_terminal_output hooks: the output the first that replaces it gives, else the output as it was
func (h *Hooks) TransformTerminal(ctx context.Context, cfg *config.Config, output TerminalOutput) string {
	if text, ok := replaced(h.Fire(ctx, cfg, output)); ok {
		return text
	}
	return output.Output
}

// Said in place of a reply held back for a hook that failed to rewrite it
const withheld = "A plugin that rewrites replies in this session failed, so this reply is held back."

// Runs the transform_llm_output hooks: the first rewrite that holds text, and whether there was one; a reply held back from the screen is
// withheld when every hook failed to rewrite it
func (h *Hooks) TransformReply(ctx context.Context, cfg *config.Config, reply Reply, held bool) (string, bool) {
	if reply.Text == "" {
		return "", false
	}
	results, failed := h.fire(ctx, cfg, reply)
	for _, r := range results {
		if r.Replace != nil && *r.Replace != "" {
			return *r.Replace, true
		}
	}
	if held && failed > 0 {
		return withheld, true
	}
	return reply.Text, false
}

// Runs the pre_llm_call hooks: why the first that blocked the turn did, and the context the others gave, each saved to a file and
// previewed when it is long, joined by blank lines
func (h *Hooks) PreLLMCall(ctx context.Context, cfg *config.Config, prompt Prompt) (string, string) {
	var blocks, parts []string
	for _, r := range h.Fire(ctx, cfg, prompt) {
		if r.Action == Block {
			if message := strings.TrimSpace(r.Message); message != "" {
				blocks = append(blocks, message)
			}
			continue
		}
		if strings.TrimSpace(r.Context) == "" {
			continue
		}
		parts = append(parts, Spill(cfg, r.Context, prompt.Session, "plugin hook"))
	}
	return strs.First(blocks), strings.Join(parts, "\n\n")
}

// Runs the pre_verify hooks: the message of the first that keeps the turn going, empty to let it end
func (h *Hooks) PreVerify(ctx context.Context, cfg *config.Config, verify Verify) string {
	for _, r := range h.Fire(ctx, cfg, verify) {
		action := strings.ToLower(strings.TrimSpace(r.Action))
		if message := strings.TrimSpace(r.Message); (action == Continue || action == Block) && message != "" {
			return message
		}
	}
	return ""
}

// Runs the on_session_finalize hooks: what they have to tell the user, each trimmed, blank ones left out
func (h *Hooks) Finalize(ctx context.Context, cfg *config.Config, finalized Finalized) []string {
	var out []string
	for _, r := range h.Fire(ctx, cfg, finalized) {
		if message := strings.TrimSpace(r.Message); message != "" {
			out = append(out, message)
		}
	}
	return out
}

// Runs the transform_api_error_classification hooks: the first classification whose reason the agent knows, in registration order, its
// message cut to 500 characters; nil when none gave one
func (h *Hooks) Classify(ctx context.Context, cfg *config.Config, failure Classify, known func(reason string) bool) *Classification {
	var picks []*Classification
	for _, r := range h.Fire(ctx, cfg, failure) {
		if r.Class == nil {
			continue
		}
		class := *r.Class
		class.Reason = strings.ToLower(strings.TrimSpace(class.Reason))
		if !known(class.Reason) {
			continue
		}
		class.Message = strs.Head(strings.TrimSpace(class.Message), 500)
		picks = append(picks, &class)
	}
	if len(picks) == 0 {
		return nil
	}
	if len(picks) > 1 {
		slog.Warn(fmt.Sprintf("transform_api_error_classification: skipped %d valid classification(s) after the first result in registration order "+
			"won (run-all-then-pick-first)", len(picks)-1))
	}
	return picks[0]
}

// Fields pre_transcription hooks may set
var transcriptionFields = []string{"model", "language", "prompt"}

// Runs the pre_transcription hooks: the model, language and prompt they set, each the last hook's that set it, what none set kept; the file
// is never theirs to change
func (h *Hooks) Transcribe(ctx context.Context, cfg *config.Config, audio Transcription) (model, language, prompt string) {
	model, prompt = strs.Deref(audio.Model), strs.Deref(audio.Prompt)
	set := map[string]string{}
	for _, r := range h.Fire(ctx, cfg, audio) {
		if r.Context != "" {
			slog.Debug("pre_transcription hook returned unsupported field context — ignoring.")
		}
		for _, key := range slices.Sorted(maps.Keys(r.Set)) {
			switch {
			case key == "file_path":
				slog.Warn("pre_transcription hook attempted to change file_path (read-only) — ignoring the attempt.")
			case !slices.Contains(transcriptionFields, key):
				slog.Debug("pre_transcription hook returned unsupported field " + key + " — ignoring.")
			default:
				set[key] = r.Set[key]
			}
		}
	}
	if value, ok := set["model"]; ok {
		model = value
	}
	if value, ok := set["prompt"]; ok {
		prompt = value
	}
	return model, set["language"], prompt
}
