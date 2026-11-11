package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var specifyLog = slog.Default().With("logger", "hermes_cli.kanban_specify")

// Integer environment value with the default for unset or unparseable text
func utilsEnvInt(name string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, ok := pyint.Parse(raw)
	if !ok || !n.IsInt64() {
		return def
	}
	return n.Int64()
}

// KanbanSpecifyMaxTokens is the specifier reply budget, at least 1500
var KanbanSpecifyMaxTokens = max(1500, utilsEnvInt("ICEBATH_KANBAN_SPECIFY_MAX_TOKENS", 6000))

const specifySystemPrompt = `You are the Kanban triage specifier for the Icebath Agent board.
A user dropped a rough idea into the Triage column. Your job is to turn it
into a concrete, actionable task spec that an autonomous worker can pick up
and execute without further clarification.

Output a single JSON object with exactly two keys:

  {
    "title": "<tightened task title, <= 80 chars, imperative voice>",
    "body":  "<multi-line spec, see structure below>"
  }

The body MUST include these sections, each prefixed with a bold markdown
heading, in this order:

  **Goal** — one sentence, user-facing outcome.
  **Approach** — 2-5 bullets on how a worker should tackle it.
  **Acceptance criteria** — checklist of concrete, verifiable conditions.
  **Out of scope** — short list of things NOT to touch (omit if nothing
      obvious; never invent scope creep).

Rules:
  - Keep the tightened title close in meaning to the original idea — do
    NOT invent a different project.
  - If the original idea is already detailed, preserve its substance and
    just reformat into the sections above.
  - Never add invented requirements the user didn't hint at.
  - No preamble, no closing remarks, no code fences around the JSON.
  - Output only the JSON object and nothing else.
`

// Specifier user prompt for one task
func specifyUserPrompt(f taskPromptFields) string {
	return fmt.Sprintf("Task id: %s\nCurrent title: %s\nCurrent body:\n%s\n", f.taskID, f.title, f.body)
}

// SpecifyOutcome is the result of specifying one triage task
type SpecifyOutcome struct {
	TaskID   string
	OK       bool
	Reason   string
	NewTitle *string
}

// Text clamped to limit characters with an ellipsis
func truncateText(text string, limit int) string {
	if pystr.Len(text) <= limit {
		return text
	}
	return pystr.Slice(text, 0, limit-1) + "…"
}

var specifyFenceRE = regexp2.MustCompile("^\\s*```(?:json)?\\s*|\\s*```\\s*$", regexp2.IgnoreCase|regexp2.Unicode)

// First brace to last brace of a fence stripped reply decoded as a JSON object, nil when none parses
func extractJSONBlob(raw string, fenceRE *regexp2.Regexp) *pyjson.Object {
	if raw == "" {
		return nil
	}
	stripped, err := fenceRE.Replace(pystr.Strip(raw), "", -1, -1)
	if err != nil {
		return nil
	}
	first, last := strings.Index(stripped, "{"), strings.LastIndex(stripped, "}")
	if first == -1 || last == -1 || last <= first {
		return nil
	}
	val, err := pyjson.Loads(stripped[first : last+1])
	if err != nil {
		return nil
	}
	obj, _ := val.(*pyjson.Object)
	return obj
}

// String value when it has non whitespace text, nil otherwise
func nonblank(v any) *string {
	s, ok := v.(string)
	if !ok || pystr.Strip(s) == "" {
		return nil
	}
	return &s
}

// Stripped title and verbatim body of an LLM reply, each nil when missing or blank
func titleBody(parsed *pyjson.Object) (*string, *string) {
	title := nonblank(parsed.GetOr("title", nil))
	if title != nil {
		t := pystr.Strip(*title)
		title = &t
	}
	return title, nonblank(parsed.GetOr("body", nil))
}

// Author for a specifier write, the current profile then $USER then the default
func specifyProfileAuthor(ctx context.Context, def string) string {
	if name := profiles.CurrentProfileName(ctx); name != "" {
		return name
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return def
}

// Task when it exists in triage, else the refusal reason
func loadTriageTask(ctx context.Context, taskID string) (*Task, string, error) {
	var task *Task
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		task, err = GetTask(ctx, conn, taskID)
		return err
	}); err != nil {
		return nil, "", err
	}
	if task == nil {
		return nil, "unknown task id", nil
	}
	if task.Status != "triage" {
		return nil, "task is not in triage (status=" + pyrepr.Repr(task.Status) + ")", nil
	}
	return task, "", nil
}

// taskPromptFields are the bounded id, title and body of a task for prompt templates
type taskPromptFields struct{ taskID, title, body string }

// Bounded prompt fields of a task
func promptFieldsOf(task *Task) taskPromptFields {
	body := "(no body)"
	if task.Body != nil && *task.Body != "" {
		body = *task.Body
	}
	return taskPromptFields{task.ID, truncateText(task.Title, 400), truncateText(body, 4000)}
}

// First choice message content of a wire response, empty when any step is missing
func auxReplyContent(resp any) string {
	m, _ := resp.(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// One auxiliary LLM call under a per task affinity scope, the reply or nil with a reason
func callAux(ctx context.Context, verb, taskID, auxTask, system, user string, maxTokens, timeout int64, logger *slog.Logger) (*string, string) {
	if provider.PortalAffinityScope(ctx) == "" {
		ctx = provider.WithPortalAffinityScope(ctx, "kanban:"+taskID)
	}
	resp, err := provider.CallLLM(ctx, map[string]any{
		"task":        auxTask,
		"messages":    []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user}},
		"temperature": 0.3,
		"max_tokens":  maxTokens,
		"timeout":     float64(timeout),
	})
	if err != nil {
		suffix := ""
		if verb == "specify" {
			suffix = " — skipping"
		}
		logger.Info(fmt.Sprintf("%s: API call failed for %s (%s)%s", verb, taskID, pyrepr.ExceptionMessage(err), suffix))
		return nil, "LLM error: " + provider.AuxExcTypeName(err)
	}
	content := auxReplyContent(resp)
	return &content, ""
}

// Specifies one triage task through the aux model and promotes it to the backlog, expected failures as not ok
func SpecifyTask(ctx context.Context, taskID string, author *string, timeout *int64) (*SpecifyOutcome, error) {
	task, reason, err := loadTriageTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return &SpecifyOutcome{TaskID: taskID, Reason: reason}, nil
	}
	limit := int64(120)
	if timeout != nil && *timeout != 0 {
		limit = *timeout
	}
	raw, reason := callAux(ctx, "specify", taskID, "triage_specifier", specifySystemPrompt, specifyUserPrompt(promptFieldsOf(task)),
		KanbanSpecifyMaxTokens, limit, specifyLog)
	if raw == nil {
		return &SpecifyOutcome{TaskID: taskID, Reason: reason}, nil
	}
	text := pystr.Strip(*raw)
	var newTitle, newBody *string
	if parsed := extractJSONBlob(text, specifyFenceRE); parsed == nil {
		if text == "" {
			return &SpecifyOutcome{TaskID: taskID, Reason: "LLM returned an empty response"}, nil
		}
		newBody = &text
	} else {
		newTitle, newBody = titleBody(parsed)
		if newBody == nil && newTitle == nil {
			return &SpecifyOutcome{TaskID: taskID, Reason: "LLM response missing title and body"}, nil
		}
	}
	who := specifyProfileAuthor(ctx, "specifier")
	if author != nil && *author != "" {
		who = *author
	}
	var ok bool
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = SpecifyTriageTask(ctx, conn, taskID, newTitle, newBody, nil, &who)
		return err
	}); err != nil {
		return nil, err
	}
	if !ok {
		return &SpecifyOutcome{TaskID: taskID, Reason: "task moved out of triage before promotion"}, nil
	}
	return &SpecifyOutcome{TaskID: taskID, OK: true, Reason: "specified", NewTitle: newTitle}, nil
}

// Task ids in the triage column, narrowed by tenant
func SpecifyListTriageIDs(ctx context.Context, tenant *string) ([]string, error) {
	status := "triage"
	var tasks []*Task
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		tasks, err = ListTasks(ctx, conn, ListTasksOptions{Status: &status, Tenant: tenant})
		return err
	}); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return ids, nil
}
