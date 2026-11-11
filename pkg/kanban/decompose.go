package kanban

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

var decomposeLog = slog.Default().With("logger", "hermes_cli.kanban_decompose")

const decomposeSystemPrompt = `You are the Kanban decomposer for the Icebath Agent board.

A user dropped a rough idea into the Triage column. Your job is to break it
into a small graph of concrete child tasks and route each one to the best-
matching profile from the available roster.

You will be given:
  - The original task title and body
  - The list of available profiles (each with name + description)
  - The fallback "default_assignee" used when no profile fits

Output a single JSON object with this exact shape:

  {
    "fanout": true,
    "rationale": "<one sentence on why this decomposition>",
    "tasks": [
      {
        "title": "<concrete task title, imperative voice, <= 80 chars>",
        "body":  "<detailed spec for the worker on this child task>",
        "assignee": "<profile name from the roster, or null for default>",
        "parents": [<int>, ...]
      },
      ...
    ]
  }

Rules:
  - "parents" is a list of INDICES (0-based) into this same "tasks" list,
    expressing actual data dependencies. Tasks with no parents run in
    PARALLEL. Tasks with parents wait until every parent completes.
  - Prefer parallelism. If two tasks can be done independently, give
    them no parents so the dispatcher fans them out at once.
  - Use 2-6 tasks for normal work. Don't create 20 tiny tasks. Don't
    cram everything into 1 task.
  - Pick assignees from the roster by matching the task to the profile's
    DESCRIPTION (not just the name). When nothing matches well, use null
    and the system will route to the default_assignee.
  - Each child task body is what a fresh worker will read with no other
    context — be specific about goal, approach, and acceptance criteria.

When the task is genuinely a single unit of work (no useful decomposition),
return:

  {
    "fanout": false,
    "rationale": "<one sentence>",
    "title": "<tightened title>",
    "body":  "<concrete spec for a single worker>",
    "assignee": "<profile name from the roster, or null for default>"
  }

In that case the task stays as one work item, just with a tightened spec and
a concrete assignee. If no profile fits, use null and the system will route to
the default_assignee.

No preamble, no closing remarks, no code fences. Output only the JSON object.
`

// Decomposer user prompt for one task with its roster and default assignee
func decomposeUserPrompt(f taskPromptFields, roster, defaultAssignee string) string {
	return fmt.Sprintf("Task id: %s\nTitle: %s\nBody:\n%s\n\nAvailable profiles (assignees you may pick from):\n%s\n\n"+
		"Default assignee (used when no profile fits a task): %s\n", f.taskID, f.title, f.body, roster, defaultAssignee)
}

var decomposeFenceRE = regexp2.MustCompile("^```(?:json)?\\s*|\\s*```$", regexp2.Multiline|regexp2.Unicode)

// DecomposeOutcome is the result of decomposing one triage task
type DecomposeOutcome struct {
	TaskID   string
	OK       bool
	Reason   string
	Fanout   bool
	ChildIDs []string
	NewTitle *string
}

// kanban.<key> naming an existing profile, else the root's assignee, else the active profile
func resolveProfileFromCfg(ctx context.Context, cfg any, key string, fallback *string) (string, error) {
	var kanbanCfg any = map[string]any{}
	if isDict(cfg) {
		if v, ok := dictGet(cfg, "kanban"); ok {
			kanbanCfg = v
		}
	}
	if !isDict(kanbanCfg) {
		return "", &TypeError{fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(kanbanCfg))}
	}
	raw, _ := dictGet(kanbanCfg, key)
	if !pyrepr.Truthy(raw) {
		raw = ""
	}
	text, ok := raw.(string)
	if !ok {
		return "", &TypeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))}
	}
	fb := ""
	if fallback != nil {
		fb = *fallback
	}
	for _, candidate := range []string{pystr.Strip(text), pystr.Strip(fb)} {
		if candidate != "" && profileExistsFn(candidate) {
			return candidate, nil
		}
	}
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name, nil
	}
	return "default", nil
}

// rosterEntry is one profile the decomposer may route to
type rosterEntry struct {
	name, description string
	hasDescription    bool
}

// Roster for the prompt and the valid assignee names, empty when profiles cannot be listed
func buildRoster(ctx context.Context) ([]rosterEntry, map[string]bool) {
	all, err := manage.ListProfiles(ctx, false)
	if err != nil {
		decomposeLog.Warn(fmt.Sprintf("decompose: failed to list profiles: %s", pyrepr.ExceptionMessage(err)))
		return nil, map[string]bool{}
	}
	roster := make([]rosterEntry, 0, len(all))
	valid := map[string]bool{}
	for _, p := range all {
		desc := pystr.Strip(p.Description)
		entry := rosterEntry{name: p.Name, description: desc, hasDescription: desc != ""}
		if desc == "" {
			entry.description = "(no description; profile named " + pyrepr.Repr(p.Name) + ")"
		}
		roster = append(roster, entry)
		valid[p.Name] = true
	}
	return roster, valid
}

// Roster lines for the prompt, flagging undescribed profiles
func formatRoster(roster []rosterEntry) string {
	if len(roster) == 0 {
		return "  (no profiles installed — decomposer cannot route work)"
	}
	lines := make([]string, 0, len(roster))
	for _, e := range roster {
		flag := ""
		if !e.hasDescription {
			flag = " ⚠ undescribed"
		}
		lines = append(lines, fmt.Sprintf("  - %s%s: %s", e.name, flag, e.description))
	}
	return strings.Join(lines, "\n")
}

// Valid stripped assignee choice, else the default so promoted work is never unassigned
func normalizeAssigneeChoice(assignee any, defaultAssignee string, valid map[string]bool) string {
	s, ok := assignee.(string)
	if !ok || pystr.Strip(s) == "" {
		return defaultAssignee
	}
	if chosen := pystr.Strip(s); valid[chosen] {
		return chosen
	}
	return defaultAssignee
}

// decomposeRouting is the config derived routing context of one decomposition
type decomposeRouting struct {
	orchestrator    string
	defaultAssignee string
	autoPromote     bool
	roster          []rosterEntry
	validNames      map[string]bool
}

// Routing context from the readonly config and the profile roster
func loadRouting(ctx context.Context, rootAssignee *string) (*decomposeRouting, error) {
	var cfg any = map[string]any{}
	if tree, err := loadKanbanConfigTree(ctx, true); err == nil {
		cfg = map[string]any(tree)
	}
	kanbanCfg, _ := dictGet(cfg, "kanban")
	if _, present := dictGet(cfg, "kanban"); !present {
		kanbanCfg = map[string]any{}
	}
	roster, valid := buildRoster(ctx)
	orchestrator, err := resolveProfileFromCfg(ctx, cfg, "orchestrator_profile", rootAssignee)
	if err != nil {
		return nil, err
	}
	defaultAssignee, err := resolveProfileFromCfg(ctx, cfg, "default_assignee", rootAssignee)
	if err != nil {
		return nil, err
	}
	autoPromote := true
	if v, ok := dictGet(kanbanCfg, "auto_promote_children"); ok {
		autoPromote = pyrepr.Truthy(v)
	}
	return &decomposeRouting{orchestrator, defaultAssignee, autoPromote, roster, valid}, nil
}

// Single task promotion of a fanout false reply, the same effect as specify
func applySingle(ctx context.Context, task *Task, parsed *pyjson.Object, routing *decomposeRouting, author string) (*DecomposeOutcome, error) {
	titleVal, bodyVal := titleBody(parsed)
	var assigneeVal *string
	if task.Assignee == nil || *task.Assignee == "" {
		chosen := normalizeAssigneeChoice(parsed.GetOr("assignee", nil), routing.defaultAssignee, routing.validNames)
		assigneeVal = &chosen
	}
	if titleVal == nil && bodyVal == nil {
		return &DecomposeOutcome{TaskID: task.ID, Reason: "decomposer returned fanout=false with no title/body"}, nil
	}
	var ok bool
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = SpecifyTriageTask(ctx, conn, task.ID, titleVal, bodyVal, assigneeVal, &author)
		return err
	}); err != nil {
		return nil, err
	}
	if !ok {
		return &DecomposeOutcome{TaskID: task.ID, Reason: "task moved out of triage before promotion"}, nil
	}
	return &DecomposeOutcome{TaskID: task.ID, OK: true, Reason: "single task (no fanout)", NewTitle: titleVal}, nil
}

// Validated child task mappings of the reply, or the reason the list was rejected
func cleanChildren(taskID string, rawTasks []any, routing *decomposeRouting) ([]any, string) {
	children := make([]any, 0, len(rawTasks))
	for idx, entry := range rawTasks {
		obj, ok := entry.(*pyjson.Object)
		if !ok {
			return nil, fmt.Sprintf("tasks[%d] is not an object", idx)
		}
		title, ok := obj.GetOr("title", nil).(string)
		if !ok || pystr.Strip(title) == "" {
			return nil, fmt.Sprintf("tasks[%d].title is missing or empty", idx)
		}
		body := obj.GetOr("body", nil)
		assignee := obj.GetOr("assignee", nil)
		chosen := normalizeAssigneeChoice(assignee, routing.defaultAssignee, routing.validNames)
		if s, isStr := assignee.(string); isStr && pystr.Strip(s) != "" && !routing.validNames[pystr.Strip(s)] {
			decomposeLog.Info(fmt.Sprintf("decompose: task %s child %d picked unknown assignee %s — routing to default_assignee %s",
				taskID, idx, pyrepr.Repr(s), pyrepr.Repr(routing.defaultAssignee)))
		}
		parentsValue := obj.GetOr("parents", nil)
		if !pyrepr.Truthy(parentsValue) {
			parentsValue = []any{}
		}
		rawParents, isList := parentsValue.([]any)
		if !isList {
			rawParents = nil
		}
		parents := []any{}
		for _, p := range rawParents {
			n, isInt := pyint.Instance(p)
			if isInt && n.IsInt64() && n.Int64() >= 0 && n.Int64() < int64(len(rawTasks)) && n.Int64() != int64(idx) {
				parents = append(parents, p)
			}
		}
		bodyText := ""
		if s, isStr := body.(string); isStr {
			bodyText = pystr.Strip(s)
		}
		children = append(children, pyjson.ObjectOf("title", pystr.Slice(pystr.Strip(title), 0, 200), "body", bodyText,
			"assignee", chosen, "parents", parents))
	}
	return children, ""
}

// Fan out of a reply into a child graph under the orchestrator
func applyFanout(ctx context.Context, taskID string, parsed *pyjson.Object, routing *decomposeRouting, author string) *DecomposeOutcome {
	rawValue := parsed.GetOr("tasks", nil)
	if !pyrepr.Truthy(rawValue) {
		rawValue = []any{}
	}
	rawTasks, ok := rawValue.([]any)
	if !ok || len(rawTasks) == 0 {
		return &DecomposeOutcome{TaskID: taskID, Reason: "decomposer returned fanout=true with empty tasks list"}
	}
	children, reason := cleanChildren(taskID, rawTasks, routing)
	if reason != "" {
		return &DecomposeOutcome{TaskID: taskID, Reason: reason}
	}
	var childIDs []string
	err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		childIDs, err = DecomposeTriageTask(ctx, conn, taskID, &routing.orchestrator, children, &author, routing.autoPromote)
		return err
	})
	if err != nil {
		var ve *ValueError
		if errors.As(err, &ve) {
			return &DecomposeOutcome{TaskID: taskID, Reason: "DB rejected graph: " + pyrepr.ExceptionMessage(err)}
		}
		decomposeLog.Error(fmt.Sprintf("decompose: DB error on task %s", taskID), "exc", err)
		return &DecomposeOutcome{TaskID: taskID, Reason: "DB error: " + pyrepr.ExceptionName(err)}
	}
	if childIDs == nil {
		return &DecomposeOutcome{TaskID: taskID, Reason: "task already decomposed or moved out of triage"}
	}
	return &DecomposeOutcome{TaskID: taskID, OK: true, Reason: fmt.Sprintf("decomposed into %d children", len(childIDs)), Fanout: true, ChildIDs: childIDs}
}

// Decomposes a triage task into a routed child graph, expected failures as not ok
func DecomposeTask(ctx context.Context, taskID string, author *string, timeout *int64) (*DecomposeOutcome, error) {
	task, reason, err := loadTriageTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return &DecomposeOutcome{TaskID: taskID, Reason: reason}, nil
	}
	routing, err := loadRouting(ctx, task.Assignee)
	if err != nil {
		return nil, err
	}
	limit := int64(180)
	if timeout != nil && *timeout != 0 {
		limit = *timeout
	}
	raw, reason := callAux(ctx, "decompose", taskID, "kanban_decomposer", decomposeSystemPrompt,
		decomposeUserPrompt(promptFieldsOf(task), formatRoster(routing.roster), routing.defaultAssignee), 4000, limit, decomposeLog)
	if raw == nil {
		return &DecomposeOutcome{TaskID: taskID, Reason: reason}, nil
	}
	parsed := extractJSONBlob(*raw, decomposeFenceRE)
	if parsed == nil {
		return &DecomposeOutcome{TaskID: taskID, Reason: "LLM returned malformed JSON"}, nil
	}
	auditAuthor := specifyProfileAuthor(ctx, "decomposer")
	if author != nil && *author != "" {
		auditAuthor = *author
	}
	if !pyrepr.Truthy(parsed.GetOr("fanout", nil)) {
		return applySingle(ctx, task, parsed, routing, auditAuthor)
	}
	return applyFanout(ctx, taskID, parsed, routing, auditAuthor), nil
}

// Task ids in the triage column, narrowed by tenant, at most a thousand
func DecomposeListTriageIDs(ctx context.Context, tenant *string) ([]string, error) {
	status := "triage"
	var tasks []*Task
	if err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		tasks, err = ListTasks(ctx, conn, ListTasksOptions{Status: &status, Tenant: tenant, Limit: 1000})
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
