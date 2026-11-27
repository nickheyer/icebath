package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	Name    = "todo_list"
	toolset = "todo"
	// Characters of one item's content kept, the rest cut with a mark
	contentChars = 4000
	cutMark      = "… [truncated]"
	// Items a list keeps
	maxItems = 256
	// Largest result of the tool, the most a list's text can take, and the largest one read back from the session
	resultChars = 512_000
)

// Statuses an item can have, as the summary counts them
var statuses = []string{"pending", "in_progress", "completed", "cancelled"}

type Item struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
	// Item this one is a subtask of, empty for one at the top
	Parent string `json:"parent,omitempty"`
}

// Result of every call: the whole list, its revision, which goes up each time a write changes the list, and how many items have each
// status
type Result struct {
	Todos    []Item  `json:"todos"`
	Revision int     `json:"revision"`
	Summary  Summary `json:"summary"`
}

type Summary struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Completed  int `json:"completed"`
	Cancelled  int `json:"cancelled"`
}

type registrant struct {
	config   *config.Config
	sessions *sessions.Registry
}

func Register(registry *tools.Registry, cfg *config.Config, stores *sessions.Registry) {
	registry.Register(registrant{config: cfg, sessions: stores})
}

func (r registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    Name,
		Toolset: toolset,
		Title:   "Tasks",
		Icon:    tools.IconPlan,
		Schema: tools.Schema(
			"Track a task list for multi-step work (3+ steps). Use for complex tasks with 3+ steps or when the user provides multiple tasks. "+
				"For 'all N items' tasks, enumerate every instance as its own checklist item so none are silently dropped. "+
				"Call with no parameters to read the current list.\n"+
				"List order is priority. Only ONE item in_progress at a time. Break large phases into subtasks via parent. "+
				"Mark an item completed only after the work is verified done, never based on intent. "+
				"If something fails, cancel it and add a revised item. Always returns the full current list.",
			[]string{},
			map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "Task items to write.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string"},
							"content": tools.Property("string", "Task description"),
							"status":  map[string]any{"type": "string", "enum": statuses},
							"parent":  tools.Property("string", "Optional id of another item, making this a nested subtask. Omit for top-level."),
						},
						"required": []string{"id", "content", "status"},
					},
				},
				"merge": map[string]any{
					"type":        "boolean",
					"description": "true: update existing items by id, add new ones. false (default): replace the entire list with a fresh plan.",
					"default":     false,
				},
			},
		),
		Handler:        r.handle,
		MaxResultChars: resultChars,
		Effect:         tools.Mutates,
	}}
}

// Writes the list, or reads it for no todos, from the list the session's last result of the tool holds
func (r registrant) handle(_ context.Context, call *tools.Call) string {
	var args struct {
		Todos json.RawMessage `json:"todos"`
		Merge bool            `json:"merge"`
	}
	if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &args); err != nil {
		return tools.ErrorResult(err)
	}
	items, revision, err := r.current(call.SessionID)
	if err != nil {
		return tools.ErrorResult(err)
	}
	if raw := bytes.TrimSpace(args.Todos); len(raw) > 0 && string(raw) != "null" {
		todos, err := decode(raw)
		if err != nil {
			return tools.ErrorResult(err)
		}
		written, err := write(items, todos, args.Merge)
		if err != nil {
			return tools.ErrorResult(err)
		}
		if !slices.Equal(written, items) {
			revision++
		}
		items = written
	}
	result := Result{Todos: items, Revision: revision, Summary: Summary{Total: len(items)}}
	for _, item := range items {
		switch item.Status {
		case "pending":
			result.Summary.Pending++
		case "in_progress":
			result.Summary.InProgress++
		case "completed":
			result.Summary.Completed++
		case "cancelled":
			result.Summary.Cancelled++
		}
	}
	if result.Todos == nil {
		result.Todos = []Item{}
	}
	text, err := json.Marshal(result)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return string(text)
}

// The todos argument as a list of raw items, numbers kept as written
func decode(raw json.RawMessage) ([]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("todos must be a list, got %s", kind(value))
	}
	return list, nil
}

func kind(value any) string {
	switch value.(type) {
	case map[string]any:
		return "an object"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "true or false"
	}
	return "nothing"
}

// The list the session last had: the newest result of the tool among the messages the person sees, of the session and then of those it
// went on from after a compression, paired with a call of the tool by the reply before it, at most the largest result long, and holding a
// list. Results compaction folded away still count, the model being shown the list again after it
func (r registrant) current(session string) ([]Item, int, error) {
	store, err := r.sessions.Store(r.config.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, 0, err
	}
	ctx := context.Background()
	chain, err := store.Lineage(ctx, session)
	if err != nil || !slices.Contains(chain, session) {
		chain = []string{session}
	}
	chain = chain[:slices.Index(chain, session)+1]
	for k := len(chain) - 1; k >= 0; k-- {
		messages, err := store.Messages(ctx, chain[k], sessions.Display)
		if err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return nil, 0, err
		}
		if items, revision, found := held(messages); found {
			return items, revision, nil
		}
	}
	return nil, 0, nil
}

// The list the newest result of the tool among the messages holds, whether one does
func held(messages []*sessionsv1.Message) ([]Item, int, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		text := message.GetText()
		if message.GetRole() != sessionsv1.Role_ROLE_TOOL || !paired(messages[:i], message.GetToolCallId()) ||
			utf8.RuneCountInString(text) > resultChars || !strings.Contains(text, `"todos"`) {
			continue
		}
		var list map[string]any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&list) != nil {
			continue
		}
		todos, ok := list["todos"].([]any)
		if !ok {
			continue
		}
		revision := 1
		if value, ok := list["revision"]; ok {
			revision = revisionOf(value)
		}
		if revision <= 0 {
			return nil, 0, true
		}
		items, err := fresh(todos)
		if err != nil {
			items = nil
		}
		return items[:min(len(items), maxItems)], revision, true
	}
	return nil, 0, false
}

// Header of the list put back after a compaction, which tells it from what the user typed
const carriedHeader = "[Your active task list was preserved across context compression]"

// Marks of the items by status in the list put back
var marks = map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]", "cancelled": "[~]"}

// The session's list as it is put back into the context after a compaction: the items still pending or in progress, and those holding
// such an item, nested under their parents; empty when none is left
func (r registrant) Carried(_ *tools.Env, session string) string {
	items, _, err := r.current(session)
	if err != nil || len(items) == 0 {
		return ""
	}
	children := map[string][]Item{}
	for _, item := range items {
		if item.Parent != "" {
			children[item.Parent] = append(children[item.Parent], item)
		}
	}
	var render func(item Item, depth int, out *[]string) bool
	render = func(item Item, depth int, out *[]string) bool {
		var below []string
		active := false
		for _, child := range children[item.ID] {
			active = render(child, depth+1, &below) || active
		}
		if keep := item.Status == "pending" || item.Status == "in_progress" || active; keep {
			mark, ok := marks[item.Status]
			if !ok {
				mark = "[?]"
			}
			*out = append(*out, fmt.Sprintf("%s- %s %s. %s (%s)", strings.Repeat("  ", depth), mark, item.ID, item.Content, item.Status))
			*out = append(*out, below...)
			return true
		}
		return false
	}
	lines := []string{carriedHeader}
	for _, item := range items {
		if item.Parent == "" {
			render(item, 0, &lines)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// Revision a result holds as a whole number: 0 for an empty value, 1 for one that is not a number
func revisionOf(value any) int {
	if !set(value) {
		return 0
	}
	switch value := value.(type) {
	case bool:
		return 1
	case json.Number:
		if f, err := value.Float64(); err == nil {
			return int(f)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return n
		}
	}
	return 1
}

// Whether the reply nearest before a tool result called the tool with the result's call id
func paired(before []*sessionsv1.Message, id string) bool {
	if id == "" {
		return false
	}
	for i := len(before) - 1; i >= 0; i-- {
		switch before[i].GetRole() {
		case sessionsv1.Role_ROLE_ASSISTANT:
			return slices.ContainsFunc(before[i].GetToolCalls(), func(call *sessionsv1.ToolCall) bool {
				return call.GetId() == id && call.GetName() == Name
			})
		case sessionsv1.Role_ROLE_USER, sessionsv1.Role_ROLE_SYSTEM:
			return false
		}
	}
	return false
}

// The list after a write: the todos in place of it, or merged into it by id. An item that is not valid leaves the list as it was
func write(items []Item, todos []any, merge bool) ([]Item, error) {
	var written []Item
	var err error
	if merge {
		written, err = merged(items, todos)
	} else {
		written, err = fresh(todos)
	}
	if err != nil {
		return nil, err
	}
	written = written[:min(len(written), maxItems)]
	parents(written)
	return written, nil
}

// The todos as a new list: one item for each id, the last one given, checked and in order
func fresh(todos []any) ([]Item, error) {
	var items []Item
	for _, todo := range dedupe(todos) {
		item, err := check(todo)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return ordered(items), nil
}

// The list with the todos merged in: the fields an item gives change the item of its id, an id not there yet adds a checked item
func merged(items []Item, todos []any) ([]Item, error) {
	items = slices.Clone(items)
	for _, todo := range dedupe(todos) {
		fields, _ := todo.(map[string]any)
		id := strings.TrimSpace(text(fields["id"]))
		if id == "" {
			continue
		}
		at := slices.IndexFunc(items, func(item Item) bool { return item.ID == id })
		if at < 0 {
			item, err := check(todo)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
			continue
		}
		if set(fields["content"]) {
			items[at].Content = capped(strings.TrimSpace(text(fields["content"])))
		}
		if set(fields["status"]) {
			if status := strings.ToLower(strings.TrimSpace(text(fields["status"]))); slices.Contains(statuses, status) {
				items[at].Status = status
			}
		}
		if parent, ok := fields["parent"]; ok {
			items[at].Parent = strings.TrimSpace(text(parent))
		}
	}
	return ordered(items), nil
}

// One item as given, made whole: an id of ? when it has none, pending for a missing or unknown status, no parent that is itself; an item
// that is not an object is a placeholder, and one without content is an error the model is told
func check(todo any) (Item, error) {
	fields, ok := todo.(map[string]any)
	if !ok {
		return Item{ID: "?", Content: "(invalid item)", Status: "pending"}, nil
	}
	id := cmp.Or(strings.TrimSpace(text(fields["id"])), "?")
	content := strings.TrimSpace(text(fields["content"]))
	if content == "" {
		return Item{}, fmt.Errorf("Todo item '%s' has empty or missing content. Each item must have a non-empty description.", id)
	}
	status := "pending"
	if value, ok := fields["status"]; ok {
		status = strings.ToLower(strings.TrimSpace(text(value)))
	}
	if !slices.Contains(statuses, status) {
		status = "pending"
	}
	item := Item{ID: id, Content: capped(content), Status: status}
	if parent := strings.TrimSpace(text(fields["parent"])); set(fields["parent"]) && parent != id {
		item.Parent = parent
	}
	return item, nil
}

// Text of an argument's value, nothing for null
func text(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case json.Number:
		return value.String()
	case bool:
		return strconv.FormatBool(value)
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// Whether a field holds something: not null, empty, zero or false
func set(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case string:
		return value != ""
	case bool:
		return value
	case json.Number:
		f, err := value.Float64()
		return err != nil || f != 0
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	}
	return true
}

func capped(content string) string {
	if runes := []rune(content); len(runes) > contentChars {
		return string(runes[:contentChars-utf8.RuneCountInString(cutMark)]) + cutMark
	}
	return content
}

// The todos with one for each id, the last given, where it was given
func dedupe(todos []any) []any {
	last := map[string]int{}
	keys := make([]string, len(todos))
	for i, todo := range todos {
		key := "__invalid_" + strconv.Itoa(i)
		if fields, ok := todo.(map[string]any); ok {
			key = cmp.Or(strings.TrimSpace(text(fields["id"])), "?")
		}
		keys[i], last[key] = key, i
	}
	var out []any
	for i, todo := range todos {
		if last[keys[i]] == i {
			out = append(out, todo)
		}
	}
	return out
}

// A flat list with the item in progress moved ahead of the pending ones before it, so the list reads in the order work happens
func ordered(items []Item) []Item {
	active, pending := -1, -1
	for i, item := range items {
		if item.Parent != "" {
			return items
		}
		if item.Status == "in_progress" && active < 0 {
			active = i
		}
		if item.Status == "pending" && pending < 0 {
			pending = i
		}
	}
	if active < 0 || pending < 0 || pending >= active {
		return items
	}
	moved := items[active]
	out := slices.Delete(slices.Clone(items), active, active+1)
	return slices.Insert(out, pending, moved)
}

// Makes an item whose parent is missing, or that is part of a loop of parents, an item at the top
func parents(items []Item) {
	at := map[string]int{}
	for i, item := range items {
		at[item.ID] = i
	}
	for i := range items {
		if _, ok := at[items[i].Parent]; items[i].Parent != "" && !ok {
			items[i].Parent = ""
		}
	}
	for i := range items {
		seen := map[string]bool{items[i].ID: true}
		for node := items[i]; node.Parent != ""; node = items[at[node.Parent]] {
			if seen[node.Parent] {
				items[i].Parent = ""
				break
			}
			seen[node.Parent] = true
		}
	}
}
