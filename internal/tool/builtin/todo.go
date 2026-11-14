package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	MaxTodoContentChars = 4000
	MaxTodoItems        = 256
	MaxTodoResultChars  = 512_000
	TodoInjectionHeader = "[Your active task list was preserved across context compression]"
	todoTruncation      = "… [truncated]"
)

var TodoLegacyAliases = []string{"todo"}

var todoStatuses = []string{"pending", "in_progress", "completed", "cancelled"}

var todoMarkers = map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]", "cancelled": "[~]"}

const todoDescription = "Track a task list for multi-step work (3+ steps). Use for complex tasks " +
	"with 3+ steps or when the user provides multiple tasks. " +
	"For 'all N items' tasks, enumerate every instance as its own checklist " +
	"item so none are silently dropped. " +
	"Call with no parameters to read the current list.\n" +
	"List order is priority. Only ONE item in_progress at a time. " +
	"Break large phases into subtasks via parent. " +
	"Mark an item completed only after the work is verified done, never " +
	"based on intent. If something fails, cancel it and add a revised " +
	"item. Always returns the full current list."

const todoParameters = `{"type": "object", "properties": {"todos": {"type": "array", "description": "Task items to write.", ` +
	`"items": {"type": "object", "properties": {"id": {"type": "string"}, "content": {"type": "string", "description": "Task description"}, ` +
	`"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]}, ` +
	`"parent": {"type": "string", "description": "Optional id of another item, making this a nested subtask. Omit for top-level."}}, ` +
	`"required": ["id", "content", "status"]}}, "merge": {"type": "boolean", "description": "true: update existing items by id, add new ones. ` +
	`false (default): replace the entire list with a fresh plan.", "default": false}}, "required": []}`

type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Parent  string `json:"parent,omitempty"`
}

type TodoState struct {
	Todos    []json.RawMessage `json:"todos"`
	Revision int64             `json:"revision"`
}

type TodoStore struct {
	mu       sync.Mutex
	items    []TodoItem
	revision int64
}

var todoKey = tool.NewKey[*TodoStore]("todo_list")

func Todos(s *tool.State) *TodoStore {
	return tool.Load(s, todoKey, func() *TodoStore { return &TodoStore{} })
}

func Todo() tool.Tool {
	return tool.Tool{
		Name:        "todo_list",
		Toolset:     "todo",
		Description: todoDescription,
		Parameters:  json.RawMessage(todoParameters),
		Handler:     handleTodo,
		Emoji:       "📋",
		Effects:     tool.Mutating,
	}
}

func IsTodoToolName(name string) bool {
	return name == "todo_list" || contains(TodoLegacyAliases, name)
}

func IsTodoToolCall(tc wire.ToolCall) bool {
	if IsTodoToolName(tc.Name) {
		return true
	}
	if !strings.Contains(tc.Arguments, "todo") || tc.Name != "tool_call" {
		return false
	}
	names, ok := tool.BridgeEntryNames(tc.Arguments)
	return ok && len(names) == 1 && IsTodoToolName(names[0])
}

func handleTodo(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	if c.State == nil {
		return tool.Failure("TodoStore not initialized"), nil
	}
	store := Todos(c.State)
	var items []TodoItem
	raw, present := args["todos"]
	if !present || raw == nil {
		items = store.Read()
	} else {
		list, ok := raw.([]any)
		if !ok {
			return tool.Failure("todos must be a list, got " + pyTypeName(raw)), nil
		}
		var err error
		items, err = store.Write(list, truthy(args["merge"]))
		if err != nil {
			return tool.Failure(err.Error()), nil
		}
	}
	return tool.JSON(todoOutput(items, store.Revision())), nil
}

func todoOutput(items []TodoItem, revision int64) any {
	type summary struct {
		Total      int `json:"total"`
		Pending    int `json:"pending"`
		InProgress int `json:"in_progress"`
		Completed  int `json:"completed"`
		Cancelled  int `json:"cancelled"`
	}
	s := summary{Total: len(items)}
	for _, item := range items {
		switch item.Status {
		case "pending":
			s.Pending++
		case "in_progress":
			s.InProgress++
		case "completed":
			s.Completed++
		case "cancelled":
			s.Cancelled++
		}
	}
	if items == nil {
		items = []TodoItem{}
	}
	return struct {
		Todos    []TodoItem `json:"todos"`
		Revision int64      `json:"revision"`
		Summary  summary    `json:"summary"`
	}{items, revision, s}
}

func (s *TodoStore) Read() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TodoItem(nil), s.items...)
}

func (s *TodoStore) HasItems() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items) > 0
}

func (s *TodoStore) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *TodoStore) State() TodoState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := TodoState{Todos: []json.RawMessage{}, Revision: s.revision}
	for _, item := range s.items {
		raw, err := json.Marshal(item)
		if err != nil {
			continue
		}
		st.Todos = append(st.Todos, raw)
	}
	return st
}

func (s *TodoStore) Write(todos []any, merge bool) ([]TodoItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next []TodoItem
	var err error
	if merge {
		next, err = mergeTodos(append([]TodoItem(nil), s.items...), todos)
	} else {
		next, err = freshTodos(todos)
	}
	if err != nil {
		return nil, err
	}
	if len(next) > MaxTodoItems {
		next = next[:MaxTodoItems]
	}
	sanitizeParents(next)
	if !equalTodos(next, s.items) {
		s.revision++
	}
	s.items = next
	return append([]TodoItem(nil), next...), nil
}

func (s *TodoStore) Restore(todos []any, revision any) ([]TodoItem, error) {
	items, err := freshTodos(todos)
	if err != nil {
		return nil, err
	}
	if len(items) > MaxTodoItems {
		items = items[:MaxTodoItems]
	}
	if !truthy(revision) {
		revision = nil
	}
	rev, ok := pyInt(revision)
	if !ok || rev < 0 {
		rev = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.revision = items, rev
	return append([]TodoItem(nil), items...), nil
}

func (s *TodoStore) FormatForInjection() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return "", false
	}
	children := map[string][]TodoItem{}
	for _, item := range s.items {
		if item.Parent != "" {
			children[item.Parent] = append(children[item.Parent], item)
		}
	}
	var render func(item TodoItem, depth int, out *[]string) bool
	render = func(item TodoItem, depth int, out *[]string) bool {
		var kids []string
		active := false
		for _, kid := range children[item.ID] {
			if render(kid, depth+1, &kids) {
				active = true
			}
		}
		keep := item.Status == "pending" || item.Status == "in_progress" || active
		if keep {
			marker, ok := todoMarkers[item.Status]
			if !ok {
				marker = "[?]"
			}
			*out = append(*out, fmt.Sprintf("%s- %s %s. %s (%s)", strings.Repeat("  ", depth), marker, item.ID, item.Content, item.Status))
			*out = append(*out, kids...)
		}
		return keep
	}
	lines := []string{TodoInjectionHeader}
	for _, item := range s.items {
		if item.Parent == "" {
			render(item, 0, &lines)
		}
	}
	if len(lines) < 2 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

func (s *TodoStore) Hydrate(ctx context.Context, sessionID string, history []wire.Message) (int, bool, error) {
	todos, rawRevision, found := latestTodoResponse(ctx, sessionID, history)
	if !found {
		return 0, false, nil
	}
	if !truthy(rawRevision) {
		rawRevision = nil
	}
	revision := int64(1)
	if n, ok := pyInt(rawRevision); ok {
		revision = max(0, n)
	}
	if revision <= s.Revision() {
		return 0, false, nil
	}
	items, err := s.Restore(todos, revision)
	if err != nil {
		return 0, false, err
	}
	return len(items), true, nil
}

func latestTodoResponse(ctx context.Context, sessionID string, history []wire.Message) ([]any, any, bool) {
	for idx := len(history) - 1; idx >= 0; idx-- {
		msg := history[idx]
		if msg.Role != wire.RoleTool || msg.Parts != nil || !todoResponsePaired(history, idx) {
			continue
		}
		content := msg.Text
		if n := runeLen(content); n > MaxTodoResultChars {
			if sessionID == "" {
				sessionID = "none"
			}
			slog.WarnContext(ctx, "Skipping oversized todo tool response during hydration", "session", sessionID, "chars", n)
			continue
		}
		if !strings.Contains(content, `"todos"`) {
			continue
		}
		data, ok := decodeObject(content)
		if !ok {
			continue
		}
		list, ok := data["todos"].([]any)
		if !ok {
			continue
		}
		revision, present := data["revision"]
		if !present {
			revision = json.Number("1")
		}
		return list, revision, true
	}
	return nil, nil, false
}

func todoResponsePaired(history []wire.Message, index int) bool {
	callID := history[index].ToolCallID
	if callID == "" {
		return false
	}
	for i := index - 1; i >= 0; i-- {
		switch history[i].Role {
		case wire.RoleAssistant:
			for _, tc := range history[i].ToolCalls {
				if tc.EffectiveID() == callID && IsTodoToolCall(tc) {
					return true
				}
			}
			return false
		case wire.RoleUser, wire.RoleSystem:
			return false
		}
	}
	return false
}

func decodeObject(text string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil || data == nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false
	}
	return data, true
}

func LatestTodoState(ctx context.Context, st *state.Store, sessionID string) (TodoState, bool, error) {
	content, ok, err := st.GetLatestTodoResult(ctx, sessionID, MaxTodoResultChars)
	if err != nil || !ok || runeLen(content) > MaxTodoResultChars {
		return TodoState{}, false, err
	}
	return NormalizeTodoState(content)
}

func NormalizeTodoState(content string) (TodoState, bool, error) {
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	var data map[string]json.RawMessage
	if err := dec.Decode(&data); err != nil {
		return TodoState{}, false, nil
	}
	var todos []json.RawMessage
	if raw, ok := data["todos"]; !ok || json.Unmarshal(raw, &todos) != nil || todos == nil {
		return TodoState{}, false, nil
	}
	var revision any
	if raw, ok := data["revision"]; ok {
		rd := json.NewDecoder(strings.NewReader(string(raw)))
		rd.UseNumber()
		if rd.Decode(&revision) != nil {
			return TodoState{}, false, nil
		}
	}
	if !truthy(revision) {
		revision = nil
	}
	rev, ok := pyInt(revision)
	if !ok {
		return TodoState{}, false, nil
	}
	rev = max(0, rev)
	if len(todos) == 0 && rev == 0 {
		return TodoState{}, false, nil
	}
	return TodoState{Todos: todos, Revision: rev}, true, nil
}

func capContent(content string) string {
	runes := []rune(content)
	if len(runes) > MaxTodoContentChars {
		return string(runes[:MaxTodoContentChars-runeLen(todoTruncation)]) + todoTruncation
	}
	return content
}

func todoField(item map[string]any, key, def string) string {
	v, ok := item[key]
	if !ok {
		return def
	}
	return pyStr(v)
}

func validateTodo(raw any) (TodoItem, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		return TodoItem{ID: "?", Content: "(invalid item)", Status: "pending"}, nil
	}
	id := strip(todoField(item, "id", ""))
	if id == "" {
		id = "?"
	}
	content := strip(todoField(item, "content", ""))
	if content == "" {
		return TodoItem{}, errors.New("Todo item '" + id + "' has empty or missing content. Each item must have a non-empty description.")
	}
	status := strings.ToLower(strip(todoField(item, "status", "pending")))
	if !contains(todoStatuses, status) {
		status = "pending"
	}
	out := TodoItem{ID: id, Content: capContent(content), Status: status}
	parent := ""
	if p := item["parent"]; truthy(p) {
		parent = strip(pyStr(p))
	}
	if parent != "" && parent != id {
		out.Parent = parent
	}
	return out, nil
}

func dedupeTodos(todos []any) []any {
	last := map[string]int{}
	var order []string
	for i, raw := range todos {
		key := "__invalid_" + strconv.Itoa(i)
		if item, ok := raw.(map[string]any); ok {
			key = strip(todoField(item, "id", ""))
		}
		if key == "" {
			key = "?"
		}
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = i
	}
	indices := make([]int, 0, len(order))
	for _, k := range order {
		indices = append(indices, last[k])
	}
	sortInts(indices)
	out := make([]any, len(indices))
	for i, idx := range indices {
		out[i] = todos[idx]
	}
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func freshTodos(todos []any) ([]TodoItem, error) {
	deduped := dedupeTodos(todos)
	items := make([]TodoItem, 0, len(deduped))
	for _, raw := range deduped {
		item, err := validateTodo(raw)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return normalizeTodoOrder(items), nil
}

func mergeTodos(items []TodoItem, todos []any) ([]TodoItem, error) {
	existing := map[string]int{}
	for i, item := range items {
		existing[item.ID] = i
	}
	for _, raw := range dedupeTodos(todos) {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := strip(todoField(t, "id", ""))
		if id == "" {
			continue
		}
		at, found := existing[id]
		if !found {
			item, err := validateTodo(t)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
			existing[item.ID] = len(items) - 1
			continue
		}
		cur := &items[at]
		if v := t["content"]; truthy(v) {
			cur.Content = capContent(strip(pyStr(v)))
		}
		if v := t["status"]; truthy(v) {
			if status := strings.ToLower(strip(pyStr(v))); contains(todoStatuses, status) {
				cur.Status = status
			}
		}
		if v, present := t["parent"]; present {
			parent := ""
			if truthy(v) {
				parent = strip(pyStr(v))
			}
			cur.Parent = parent
		}
	}
	var rebuilt []TodoItem
	placed := map[string]bool{}
	for _, item := range items {
		if placed[item.ID] {
			continue
		}
		placed[item.ID] = true
		rebuilt = append(rebuilt, items[existing[item.ID]])
	}
	return normalizeTodoOrder(rebuilt), nil
}

func sanitizeParents(items []TodoItem) {
	byID := map[string]int{}
	for i, item := range items {
		byID[item.ID] = i
	}
	for i := range items {
		if items[i].Parent != "" {
			if _, ok := byID[items[i].Parent]; !ok {
				items[i].Parent = ""
			}
		}
	}
	for i := range items {
		seen := map[string]bool{items[i].ID: true}
		node := items[i]
		for node.Parent != "" {
			if seen[node.Parent] {
				items[i].Parent = ""
				break
			}
			seen[node.Parent] = true
			node = items[byID[node.Parent]]
		}
	}
}

func normalizeTodoOrder(items []TodoItem) []TodoItem {
	active, pending := -1, -1
	for i, item := range items {
		if item.Parent != "" {
			return items
		}
		if item.Status == "in_progress" && active < 0 {
			active = i
		}
	}
	if active < 0 {
		return items
	}
	for i := 0; i < active; i++ {
		if items[i].Status == "pending" {
			pending = i
			break
		}
	}
	if pending < 0 {
		return items
	}
	out := make([]TodoItem, 0, len(items))
	out = append(out, items[:pending]...)
	out = append(out, items[active])
	for i := pending; i < len(items); i++ {
		if i != active {
			out = append(out, items[i])
		}
	}
	return out
}

func equalTodos(a, b []TodoItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
