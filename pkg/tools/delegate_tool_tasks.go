package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	fillerGoalRe     = regexp.MustCompile(`(?i)^(todo|task\s*\d+)$`)
	templateMarkerRe = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9]*(?:[ _-][A-Za-z0-9]+)+>|\{[A-Za-z][A-Za-z0-9]*(?:[ _-][A-Za-z0-9]+)+\}`)
)

const (
	minBatchGoalLen = 10
	MaxTaskImages   = 8
)

// Task objects of a JSON array string, an error message for a bad string, both empty for any other value
func recoverTasksFromJSONString(tasks any) ([]any, string) {
	s, ok := tasks.(string)
	if !ok {
		return nil, ""
	}
	raw := pystr.Strip(s)
	if raw == "" {
		return nil, "Provide either 'goal' (single task) or 'tasks' (batch)."
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		msg := err.Error()
		var de *pyjson.DecodeError
		if errors.As(err, &de) {
			msg = de.Msg
		}
		return nil, fmt.Sprintf("tasks must be a JSON array of task objects; received a string that could not be parsed as JSON (%s).", msg)
	}
	list, ok := parsed.([]any)
	if !ok {
		return nil, fmt.Sprintf("tasks must be a JSON array of task objects; parsed %s instead.", pyrepr.TypeName(parsed))
	}
	return list, ""
}

// Batch quality gate rejecting filler goals, unexpanded templates and terse goals in a fan out
func validateBatchTasks(taskList []map[string]any) string {
	for i, task := range taskList {
		goal := ""
		if g, ok := task["goal"]; ok {
			goal = pyrepr.Str(g)
		}
		goal = pystr.Strip(goal)
		if fillerGoalRe.MatchString(strings.Join(strings.Fields(strings.ToLower(goal)), " ")) {
			return fmt.Sprintf("Task %d has a placeholder goal (%s). Replace it with a specific, self-contained description of what the subagent should accomplish.", i, pyrepr.Repr(goal))
		}
		if marker := templateMarkerRe.FindString(goal); marker != "" {
			return fmt.Sprintf("Task %d goal contains an unexpanded template marker (%s). Substitute the real value before calling delegate_task — subagents cannot resolve placeholders.", i, pyrepr.Repr(marker))
		}
		if utf8.RuneCountInString(goal) < minBatchGoalLen && len(taskList) >= 2 {
			return fmt.Sprintf("Task %d goal is too short (%s). Write a specific, self-contained goal of at least %d characters so the subagent knows exactly what to do.", i, pyrepr.Repr(goal), minBatchGoalLen)
		}
	}
	return ""
}

// Task list from tasks or the legacy single goal, an error message for the model, an error where Python raises
func NormalizeTaskList(goal, context, tasks, outputSchema any, topRole string, maxChildren int) ([]map[string]any, string, error) {
	recovered, tasksErr := recoverTasksFromJSONString(tasks)
	if tasksErr != "" {
		return nil, tasksErr, nil
	}
	if recovered != nil {
		tasks = recovered
	}
	list, isList := tasks.([]any)
	var raw []any
	switch g, _ := goal.(string); {
	case isList && len(list) > 0:
		if len(list) > maxChildren {
			return nil, fmt.Sprintf("Too many tasks: %d provided, but max_concurrent_children is %d. Either reduce the task count, split into multiple delegate_task calls, or increase delegation.max_concurrent_children in config.yaml.", len(list), maxChildren), nil
		}
		raw = list
	case g != "" && pystr.Strip(g) != "":
		task := map[string]any{"goal": g, "context": context, "role": topRole}
		if outputSchema != nil {
			task["output_schema"] = outputSchema
		}
		raw = []any{task}
	default:
		return nil, "No tasks provided. Pass tasks=[{goal: '...', context: '...'}, ...] — one entry per subagent (a single task is a one-entry array).", nil
	}
	taskList := make([]map[string]any, len(raw))
	for i, item := range raw {
		var task map[string]any
		switch t := item.(type) {
		case map[string]any:
			task = t
		case *pyjson.Object:
			task = t.Vals
		default:
			return nil, fmt.Sprintf("Task %d must be an object, got %s.", i, pyrepr.TypeName(item)), nil
		}
		g, present := task["goal"]
		if !present {
			g = ""
		}
		gs, ok := g.(string)
		if !ok {
			return nil, "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(g))
		}
		if pystr.Strip(gs) == "" {
			return nil, fmt.Sprintf("Task %d is missing a 'goal'.", i), nil
		}
		taskList[i] = task
	}
	if isList {
		if msg := validateBatchTasks(taskList); msg != "" {
			return nil, msg, nil
		}
	}
	return taskList, "", nil
}

// Coerced output schema per task, a malformed one failing the whole call before any child spawns
func CoerceTaskSchemas(taskList []map[string]any, outputSchema any) ([]any, string) {
	schemas := make([]any, 0, len(taskList))
	for i, task := range taskList {
		raw := task["output_schema"]
		if raw == nil && len(taskList) == 1 && outputSchema != nil {
			raw = outputSchema
		}
		coerced, msg := CoerceOutputSchema(raw)
		if msg != "" {
			return nil, fmt.Sprintf("Task %d output_schema invalid: %s", i, msg)
		}
		schemas = append(schemas, coerced)
	}
	return schemas, ""
}

// Cleaned image list of one task with a bare string wrapped, nil when absent or empty
func NormalizeTaskImages(task map[string]any, i int) ([]string, string) {
	raw, present := task["images"]
	if !present || raw == nil {
		return nil, ""
	}
	if s, ok := raw.(string); ok {
		raw = []any{s}
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		return nil, fmt.Sprintf("Task %d 'images' must be an array of local file paths or http(s) URLs.", i)
	}
	var cleaned []string
	for _, item := range items {
		s, ok := item.(string)
		if !ok || pystr.Strip(s) == "" {
			return nil, fmt.Sprintf("Task %d 'images' entries must be non-empty strings (local file paths or http(s) URLs).", i)
		}
		cleaned = append(cleaned, pystr.Strip(s))
	}
	if len(cleaned) > MaxTaskImages {
		return nil, fmt.Sprintf("Task %d has %d images; the per-task limit is %d. Trim to the images the child actually needs to see.", i, len(cleaned), MaxTaskImages)
	}
	return cleaned, ""
}

// Validated image list per task, the legacy top level images applying to a single task only
func CoerceTaskImages(taskList []map[string]any, images any) ([][]string, string) {
	out := make([][]string, 0, len(taskList))
	for i, task := range taskList {
		if task["images"] == nil && len(taskList) == 1 && images != nil {
			merged := make(map[string]any, len(task)+1)
			for k, v := range task {
				merged[k] = v
			}
			merged["images"] = images
			task = merged
		}
		cleaned, msg := NormalizeTaskImages(task, i)
		if msg != "" {
			return nil, msg
		}
		out = append(out, cleaned)
	}
	return out, ""
}
