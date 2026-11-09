package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Recent chat messages the reviewer gets
const DefaultReviewContextMessages = 10

// Per message excerpt cap against a pathological turn
const reviewMessageCharCap = 12_000

// Standing instruction of the reviewer subagent
const ReviewGoal = "Act as an independent senior reviewer. Thoroughly review the work presented in the conversation excerpt " +
	"provided in your context: investigate any code, pull request, branch, commit, documentation, design, or other " +
	"artifact it references (open the PR, read the diff, run the code or tests where feasible) rather than judging " +
	"from the excerpt alone. Produce a full, structured review: what the work does, whether it is correct and " +
	"complete, concrete defects or risks found (with file/line references where possible), what was verified vs. " +
	"only read, and a clear final verdict with recommended next steps."

// Parent agents that expose the skill preload marker of their launch
type reviewSkillPromptAgent interface{ EphemeralSystemPrompt() string }

var reviewPreloadedSkillRe = regexp.MustCompile(`with the "([^"]+)" skill\s+preloaded`)

// Read only config loader of the review credentials, swapped by tests
var reviewLoadConfigReadonly = func(ctx context.Context, env *Env) (map[string]any, error) {
	h := envHome(env)
	tree, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	return tree, nil
}

// Display text of a message, multimodal parts joined and non text parts noted
func reviewMessageText(message map[string]any) string {
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, item := range content {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var text string
			if part["type"] == "text" {
				if v := part["text"]; pyTruthy(v) {
					text = pyStr(v)
				}
			} else if typ, present := part["type"]; present {
				text = "[" + pyStr(typ) + "]"
			} else {
				text = "[attachment]"
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Last limit user and assistant messages with text as role and text pairs, oldest first
func SnapshotRecentMessages(messages []map[string]any, limit int) []map[string]string {
	var out []map[string]string
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message == nil {
			continue
		}
		role := ""
		if pyTruthy(message["role"]) {
			role = pyStr(message["role"])
		}
		text := ""
		if role == "user" || role == "assistant" {
			text = pystr.Strip(reviewMessageText(message))
		}
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > reviewMessageCharCap {
			text = string(runes[:reviewMessageCharCap]) + "\n[... truncated ...]"
		}
		out = append(out, map[string]string{"role": role, "text": text})
		if len(out) >= limit {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Skills the parent worked under, launch preloaded first then skill_view loads from history, deduped and capped
func CollectParentLoadedSkills(parent any, messages []map[string]any, limit int) []string {
	prompt := ""
	if p, ok := parent.(reviewSkillPromptAgent); ok && !isNilValue(parent) {
		prompt = p.EphemeralSystemPrompt()
	}
	var candidates []string
	for _, m := range reviewPreloadedSkillRe.FindAllStringSubmatch(prompt, -1) {
		candidates = append(candidates, m[1])
	}
	for _, message := range messages {
		if message == nil || message["role"] != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, call := range calls {
			callMap, ok := call.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := callMap["function"].(map[string]any)
			if fn["name"] != "skill_view" {
				continue
			}
			raw := "{}"
			if pyTruthy(fn["arguments"]) {
				s, isString := fn["arguments"].(string)
				if !isString {
					continue
				}
				raw = s
			}
			parsed, err := pyjson.Loads(raw)
			if err != nil {
				continue
			}
			args, ok := parsed.(*pyjson.Object)
			if !ok {
				continue
			}
			if filePath, _ := args.Get("file_path"); pyTruthy(filePath) {
				continue
			}
			name, _ := args.Get("name")
			if pyTruthy(name) {
				candidates = append(candidates, pyStr(name))
			} else {
				candidates = append(candidates, "")
			}
		}
	}
	var names []string
	seen := map[string]bool{}
	for _, name := range candidates {
		cleaned := pystr.Strip(name)
		if cleaned != "" && !seen[cleaned] {
			seen[cleaned] = true
			names = append(names, cleaned)
		}
	}
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}

// Viewer friendly goal and the complete reviewer briefing
func BuildReviewTask(snapshot []map[string]string, userPrompt string, loadedSkills []string) (string, string) {
	focus := strings.Join(pystr.SplitWhitespace(userPrompt, -1), " ")
	goal := "Review recent work"
	if focus != "" {
		goal = "Review: " + focus
	}
	if runes := []rune(goal); len(runes) > 80 {
		goal = pystr.Rstrip(string(runes[:79])) + "…"
	}
	lines := []string{
		ReviewGoal,
		"",
		"You were spawned by the /review command. The following is an excerpt of the most recent conversation " +
			"between the user and their primary agent. It is your starting evidence — the work to " +
			"review is referenced in it.",
		"",
		"--- Recent conversation (oldest first) ---",
	}
	for _, message := range snapshot {
		label := "[PRIMARY AGENT]"
		if message["role"] == "user" {
			label = "[USER]"
		}
		lines = append(lines, label, message["text"], "")
	}
	lines = append(lines, "--- End of conversation excerpt ---")
	if len(loadedSkills) > 0 {
		lines = append(lines,
			"",
			"The primary agent was operating under these loaded skills: "+
				strings.Join(loadedSkills, ", ")+". Before reviewing, load each with "+
				"skill_view(name=...) and treat their conventions, invariants, "+
				"and review standards as binding for your assessment — the work "+
				"was produced under them and must be judged against them.",
		)
	}
	if pystr.Strip(userPrompt) != "" {
		lines = append(lines, "", "Additional review instructions from the user:", pystr.Strip(userPrompt))
	}
	lines = append(lines,
		"",
		"Your review is delivered back into that conversation, addressed to "+
			"the primary agent and its user. Be direct and specific; do not "+
			"soften findings.",
	)
	return goal, strings.Join(lines, "\n")
}

// auxiliary.review as a delegation credentials map, nil when unconfigured so the reviewer inherits the parent
func loadReviewCredentialsCfg(ctx context.Context, env *Env) map[string]any {
	loaded, err := reviewLoadConfigReadonly(ctx, env)
	if err != nil {
		return nil
	}
	auxiliary, _ := loaded["auxiliary"].(map[string]any)
	reviewValue := auxiliary["review"]
	if !pyTruthy(reviewValue) {
		return nil
	}
	review, ok := reviewValue.(map[string]any)
	if !ok {
		return nil
	}
	cfg := map[string]any{}
	for _, k := range []string{"provider", "model", "base_url", "api_key", "api_mode"} {
		value := ""
		if pyTruthy(review[k]) {
			value = pyStr(review[k])
		}
		cfg[k] = pystr.Strip(value)
	}
	if pystr.Lower(cfg["provider"].(string)) == "auto" {
		cfg["provider"] = ""
	}
	if cfg["provider"] == "" && cfg["model"] == "" && cfg["base_url"] == "" {
		return nil
	}
	return cfg
}

// Dispatches the reviewer subagent and returns the parsed delegate_task reply, an error when nothing can be reviewed
func StartReview(ctx context.Context, env *Env, parent any, messages []map[string]any, userPrompt string) (*pyjson.Object, error) {
	if isNilValue(parent) {
		return nil, errors.New("No active agent — send a message first.")
	}
	snapshot := SnapshotRecentMessages(messages, DefaultReviewContextMessages)
	if len(snapshot) == 0 {
		return nil, errors.New("Nothing to review yet — the conversation is empty.")
	}
	goal, contextText := BuildReviewTask(snapshot, userPrompt, CollectParentLoadedSkills(parent, messages, 8))
	credentialsCfg := loadReviewCredentialsCfg(ctx, env)
	background := true
	raw := DelegateTask(ctx, env, DelegateTaskRequest{Goal: goal, Context: contextText, Background: &background, ParentAgent: parent, CredentialsCfg: credentialsCfg})
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		parsed = nil
	}
	result, isObject := parsed.(*pyjson.Object)
	if isObject {
		if errValue, _ := result.Get("error"); pyTruthy(errValue) {
			return nil, errors.New(pyStr(errValue))
		}
	}
	if !isObject {
		return nil, fmt.Errorf("Review dispatch failed: %s", pyrepr.Repr(raw))
	}
	if _, present := result.Get("review_model"); !present {
		model := ""
		if credentialsCfg != nil && pyTruthy(credentialsCfg["model"]) {
			model = pyStr(credentialsCfg["model"])
		}
		result.Set("review_model", model)
	}
	return result, nil
}

// Human facing one liner for a successful dispatch, shared by every surface
func FormatDispatchNote(result *pyjson.Object, userPrompt string) string {
	if status, _ := result.Get("status"); status == "dispatched" {
		return "Review started. Results will return here."
	}
	model := ""
	if v, _ := result.Get("review_model"); pyTruthy(v) {
		model = pystr.Strip(pyStr(v))
	}
	modelNote := ""
	if model != "" {
		modelNote = " on " + model
	}
	focusNote := ""
	if pystr.Strip(userPrompt) != "" {
		focusNote = " (focus: " + pystr.Strip(userPrompt) + ")"
	}
	var body any = result
	if results, present := result.Get("results"); present {
		body = results
	}
	dumped, err := pyjson.Dumps(body, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	if runes := []rune(dumped); len(runes) > 4000 {
		dumped = string(runes[:4000])
	}
	return "⚖ Review completed synchronously" + modelNote + focusNote + " — results:\n" + dumped
}

// True for nil and for a typed nil pointer, map, slice, func, channel or interface
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}
