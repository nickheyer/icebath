package server

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// One task of a delegate_task call
type task struct {
	index   int
	goal    string
	context string
	role    string
	// JSON schema its final answer must satisfy, nil for none
	schema any
	// Images its subagent must see, local paths or addresses
	images []string
	// Completion bucket of its background unit, grouped for a task that names one
	group   string
	grouped bool
	// Everything the model gave the task, for the parameters a backend declares
	raw map[string]any
}

var (
	filler       = regexp.MustCompile(`(?i)^(todo|task\s*\d+)$`)
	templateMark = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9]*(?:[ _-][A-Za-z0-9]+)+>|\{[A-Za-z][A-Za-z0-9]*(?:[ _-][A-Za-z0-9]+)+\}`)
)

const (
	// Shortest goal of a task of a batch of several
	minBatchGoal = 10
	// Images one task may hand its subagent
	maxTaskImages = 8
)

// Kind of a JSON value as an error names it
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, json.Number:
		return "number"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func quoted(text string) string { return strconv.Quote(text) }

// Text of an argument that is one, empty otherwise
func argText(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// Leaf or orchestrator, anything else becoming leaf, an unknown word with a warning
func normalizeRole(raw any) string {
	word := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
	if raw == nil || word == "" || word == "false" || word == "0" {
		return "leaf"
	}
	if word != "leaf" && word != "orchestrator" {
		slog.Warn(fmt.Sprintf("Unknown delegate_task role=%s, coercing to 'leaf'", quoted(fmt.Sprint(raw))))
		return "leaf"
	}
	return word
}

// Tasks of the call from tasks, or from the single goal of an older call, each checked; the error text the model reads when they are bad
func parseTasks(args map[string]any, topRole string, limit int) ([]*task, string) {
	rawTasks := args["tasks"]
	if s, ok := rawTasks.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, "Provide either 'goal' (single task) or 'tasks' (batch)."
		}
		var parsed any
		decoder := json.NewDecoder(strings.NewReader(s))
		decoder.UseNumber()
		if err := decoder.Decode(&parsed); err != nil {
			return nil, fmt.Sprintf("tasks must be a JSON array of task objects; received a string that could not be parsed as JSON (%s).", err)
		}
		list, ok := parsed.([]any)
		if !ok {
			return nil, fmt.Sprintf("tasks must be a JSON array of task objects; parsed %s instead.", jsonKind(parsed))
		}
		rawTasks = list
	}
	list, batch := rawTasks.([]any)
	var raw []map[string]any
	switch goal := argText(args, "goal"); {
	case batch && len(list) > 0:
		if len(list) > limit {
			return nil, fmt.Sprintf("Too many tasks: %d provided, but max_concurrent_children is %d. Either reduce the task count, split into multiple "+
				"delegate_task calls, or increase delegation.max_concurrent_children in config.yaml.", len(list), limit)
		}
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Sprintf("Task %d must be an object, got %s.", i, jsonKind(item))
			}
			raw = append(raw, m)
		}
	case strings.TrimSpace(goal) != "":
		single := map[string]any{"goal": goal, "context": args["context"], "role": topRole}
		if schema, ok := args["output_schema"]; ok && schema != nil {
			single["output_schema"] = schema
		}
		raw = []map[string]any{single}
		batch = false
	default:
		return nil, "No tasks provided. Pass tasks=[{goal: '...', context: '...'}, ...] — one entry per subagent (a single task is a one-entry array)."
	}
	tasks := make([]*task, len(raw))
	for i, m := range raw {
		goal, isText := m["goal"].(string)
		if m["goal"] != nil && !isText {
			return nil, fmt.Sprintf("Task %d goal is %s, expected text.", i, jsonKind(m["goal"]))
		}
		if strings.TrimSpace(goal) == "" {
			return nil, fmt.Sprintf("Task %d is missing a 'goal'.", i)
		}
		t := &task{index: i, goal: goal, context: argText(m, "context"), raw: m}
		if t.context == "" && len(raw) == 1 {
			t.context = argText(args, "context")
		}
		t.role = topRole
		if role, ok := m["role"]; ok && role != nil && fmt.Sprint(role) != "" {
			t.role = normalizeRole(role)
		}
		if g, ok := m["group"]; ok && g != nil && fmt.Sprint(g) != "" {
			t.group, t.grouped = fmt.Sprint(g), true
		}
		tasks[i] = t
	}
	if batch {
		if refusal := checkBatch(tasks); refusal != "" {
			return nil, refusal
		}
	}
	for i, t := range tasks {
		schema := t.raw["output_schema"]
		if schema == nil && len(tasks) == 1 {
			schema = args["output_schema"]
		}
		coerced, refusal := outputSchema(schema)
		if refusal != "" {
			return nil, fmt.Sprintf("Task %d output_schema invalid: %s", i, refusal)
		}
		t.schema = coerced
		images := t.raw["images"]
		if images == nil && len(tasks) == 1 {
			images = args["images"]
		}
		cleaned, refusal := taskImages(images, i)
		if refusal != "" {
			return nil, refusal
		}
		t.images = cleaned
	}
	return tasks, ""
}

// What makes a batch of tasks no good: a filler goal, a template left unfilled, a goal too short to say what to do in a fan out
func checkBatch(tasks []*task) string {
	for i, t := range tasks {
		goal := strings.TrimSpace(t.goal)
		if filler.MatchString(strings.Join(strings.Fields(strings.ToLower(goal)), " ")) {
			return fmt.Sprintf("Task %d has a placeholder goal (%s). Replace it with a specific, self-contained description of what the subagent should "+
				"accomplish.", i, quoted(goal))
		}
		if marker := templateMark.FindString(goal); marker != "" {
			return fmt.Sprintf("Task %d goal contains an unexpanded template marker (%s). Substitute the real value before calling delegate_task — "+
				"subagents cannot resolve placeholders.", i, quoted(marker))
		}
		if utf8.RuneCountInString(goal) < minBatchGoal && len(tasks) >= 2 {
			return fmt.Sprintf("Task %d goal is too short (%s). Write a specific, self-contained goal of at least %d characters so the subagent knows "+
				"exactly what to do.", i, quoted(goal), minBatchGoal)
		}
	}
	return ""
}

// Images of one task, a lone string taken as one, empty entries and too many refused
func taskImages(raw any, i int) ([]string, string) {
	if raw == nil {
		return nil, ""
	}
	if s, ok := raw.(string); ok {
		raw = []any{s}
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Sprintf("Task %d 'images' must be an array of local file paths or http(s) URLs.", i)
	}
	var cleaned []string
	for _, item := range items {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Sprintf("Task %d 'images' entries must be non-empty strings (local file paths or http(s) URLs).", i)
		}
		cleaned = append(cleaned, strings.TrimSpace(s))
	}
	if len(cleaned) > maxTaskImages {
		return nil, fmt.Sprintf("Task %d has %d images; the per-task limit is %d. Trim to the images the child actually needs to see.", i, len(cleaned),
			maxTaskImages)
	}
	return cleaned, ""
}

// Output schema of a task as a JSON schema object, a JSON text of one decoded; why it is no good, empty when it is or none was given
func outputSchema(raw any) (any, string) {
	if raw == nil {
		return nil, ""
	}
	if s, ok := raw.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, "output_schema must be a JSON Schema object, got a non-JSON string."
		}
		if _, ok := parsed.(map[string]any); !ok {
			return nil, "output_schema must be a JSON Schema object."
		}
		raw = parsed
	}
	if _, ok := raw.(map[string]any); !ok {
		return nil, fmt.Sprintf("output_schema must be a JSON Schema object, got %s.", jsonKind(raw))
	}
	if _, err := compileSchema(raw); err != nil {
		return nil, "output_schema is not a valid JSON Schema: " + err.Error()
	}
	return raw, ""
}

// Schema compiled from a decoded JSON object, its numbers kept exact
func compileSchema(schema any) (*jsonschema.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("output_schema.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("output_schema.json")
}

// Context of a subagent with the output contract of its schema after it
func withContract(context string, schema any) string {
	text, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		text = []byte(fmt.Sprint(schema))
	}
	block := "OUTPUT CONTRACT (machine-validated):\n" +
		"Your FINAL response must be ONLY the JSON value that validates against this JSON Schema — no prose before or after it, no code fence, no " +
		"explanation. Anything else costs a correction turn and, if it fails again, is handed to the caller unvalidated.\n" + string(text)
	if base := strings.TrimRightFunc(context, unicode.IsSpace); base != "" {
		return base + "\n\n" + block
	}
	return block
}

// JSON a final answer holds: the outermost object or array that parses, a code fence and the prose around it dropped
func jsonCandidate(text string) string {
	raw := strings.TrimSpace(text)
	if strings.HasPrefix(raw, "```") {
		if _, rest, ok := strings.Cut(raw, "\n"); ok {
			raw = rest
		}
		if trimmed := strings.TrimRightFunc(raw, unicode.IsSpace); strings.HasSuffix(trimmed, "```") {
			raw = trimmed[:len(trimmed)-3]
		}
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(raw), "json\n") {
			_, raw, _ = strings.Cut(raw, "\n")
		}
	}
	type span struct {
		start int
		text  string
	}
	var spans []span
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(raw, pair[0]), strings.LastIndex(raw, pair[1])
		if start >= 0 && end > start {
			spans = append(spans, span{start, raw[start : end+1]})
		}
	}
	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Or(cmp.Compare(a.start, b.start), strings.Compare(a.text, b.text)) })
	for _, s := range spans {
		if json.Valid([]byte(s.text)) {
			return s.text
		}
	}
	if len(spans) > 0 {
		return spans[0].text
	}
	return raw
}

// Whether the final answer satisfies the schema, and up to ten errors saying where it does not, each with its path
func validateOutput(answer string, schema any) (bool, []string, error) {
	candidate := jsonCandidate(answer)
	if strings.TrimSpace(candidate) == "" {
		return false, []string{"Response was empty — expected a JSON object matching the schema."}, nil
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(candidate))
	if err != nil {
		return false, []string{"Response is not valid JSON: " + err.Error()}, nil
	}
	compiled, err := compileSchema(schema)
	if err != nil {
		return false, nil, err
	}
	failure, ok := compiled.Validate(value).(*jsonschema.ValidationError)
	if !ok {
		return true, nil, nil
	}
	type found struct {
		path    []string
		message string
	}
	var errs []found
	for _, unit := range failure.BasicOutput().Errors {
		if unit.Error == nil {
			continue
		}
		var path []string
		if unit.InstanceLocation != "" {
			path = strings.Split(strings.TrimPrefix(unit.InstanceLocation, "/"), "/")
		}
		errs = append(errs, found{path, unit.Error.String()})
	}
	if len(errs) == 0 {
		errs = append(errs, found{nil, failure.Error()})
	}
	slices.SortStableFunc(errs, func(a, b found) int { return comparePaths(a.path, b.path) })
	var rendered []string
	for _, e := range errs[:min(len(errs), 10)] {
		var b strings.Builder
		b.WriteString("$")
		for _, part := range e.path {
			part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
			if n, err := strconv.Atoi(part); err == nil {
				fmt.Fprintf(&b, "[%d]", n)
			} else {
				b.WriteString("." + part)
			}
		}
		rendered = append(rendered, b.String()+": "+e.message)
	}
	return false, rendered, nil
}

// Order of two paths into a value: positions by number, names by text, a position before a name, a shorter path first
func comparePaths(a, b []string) int {
	for i := range min(len(a), len(b)) {
		an, aErr := strconv.Atoi(a[i])
		bn, bErr := strconv.Atoi(b[i])
		switch {
		case aErr == nil && bErr == nil:
			if c := cmp.Compare(an, bn); c != 0 {
				return c
			}
		case aErr == nil:
			return -1
		case bErr == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(a), len(b))
}

// The one correction turn a subagent gets, its errors verbatim, the schema not pasted again
func retryMessage(errs []string) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = "- " + e
	}
	return "Your previous final response was rejected by the output contract validator. Validation errors:\n" + strings.Join(lines, "\n") + "\n\n" +
		"Reply with ONLY the corrected JSON object matching the OUTPUT CONTRACT schema from your task context. No prose, no explanations."
}
