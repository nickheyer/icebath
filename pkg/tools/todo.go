package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	MaxTodoContentChars = 4000
	MaxTodoItems        = 256
	MaxTodoResultChars  = 512_000
	todoTruncationMark  = "… [truncated]"
	// TodoInjectionHeader is the stable header the compressor keys on to tell the synthetic row from a user message
	TodoInjectionHeader = "[Your active task list was preserved across context compression]"
	TodoToolName        = "todo_list"
)

var (
	todoValidStatuses  = map[string]bool{"pending": true, "in_progress": true, "completed": true, "cancelled": true}
	todoStatusMarkers  = map[string]string{"completed": "[x]", "in_progress": "[>]", "pending": "[ ]", "cancelled": "[~]"}
	todoActiveStatuses = map[string]bool{"pending": true, "in_progress": true}
	// TodoLegacyAliases are pre rename names that replay as the todo tool
	TodoLegacyAliases = []string{"todo"}
)

// TodoItem is one task list row, Parent empty for a top level item
type TodoItem struct {
	ID      string
	Content string
	Status  string
	Parent  string
}

// JSON shape of the item, parent only when set
func (t TodoItem) asMap() map[string]any {
	m := map[string]any{"id": t.ID, "content": t.Content, "status": t.Status}
	if t.Parent != "" {
		m["parent"] = t.Parent
	}
	return m
}

// TodoStore is the in memory revisioned task list, one per agent session
type TodoStore struct {
	mu       sync.Mutex
	items    []TodoItem
	revision int
}

// NewTodoStore returns an empty store at revision zero
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Copies of the items in priority order
func (s *TodoStore) readLocked() []TodoItem { return append([]TodoItem(nil), s.items...) }

// Read returns copies of the items in priority order
func (s *TodoStore) Read() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

// HasItems reports whether the list is non empty
func (s *TodoStore) HasItems() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items) > 0
}

// Revision is the monotonic write counter clients use to reject stale updates
func (s *TodoStore) Revision() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Snapshot is the full state clients reconcile atomically
func (s *TodoStore) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"todos": todoItemsJSON(s.items), "revision": s.revision}
}

// Items as their JSON row maps
func todoItemsJSON(items []TodoItem) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.asMap()
	}
	return out
}

// Validates, dedupes and orders a whole new list
func todoFreshItems(todos []any) ([]TodoItem, error) {
	deduped := todoDedupeByID(todos)
	items := make([]TodoItem, 0, len(deduped))
	for _, t := range deduped {
		item, err := todoValidate(t)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return todoNormalizeOrder(items), nil
}

// Write replaces the list or merges by id and returns the full list, an invalid item leaves the list untouched
func (s *TodoStore) Write(todos []any, merge bool) ([]TodoItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.readLocked()
	var err error
	if merge {
		err = s.mergeLocked(todos)
	} else {
		s.items, err = todoFreshItems(todos)
	}
	if err != nil {
		s.items = before
		return nil, err
	}
	if len(s.items) > MaxTodoItems {
		s.items = s.items[:MaxTodoItems]
	}
	todoSanitizeParents(s.items)
	if !todoItemsEqual(s.items, before) {
		s.revision++
	}
	return s.readLocked(), nil
}

// Reports whether two lists hold the same rows in the same order
func todoItemsEqual(a, b []TodoItem) bool {
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

// Updates existing items only in the fields provided and appends validated new ones
func (s *TodoStore) mergeLocked(todos []any) error {
	existing := map[string]*TodoItem{}
	for i := range s.items {
		existing[s.items[i].ID] = &s.items[i]
	}
	for _, raw := range todoDedupeByID(todos) {
		t, _ := raw.(map[string]any)
		itemID := strings.TrimSpace(todoField(t, "id"))
		if itemID == "" {
			continue
		}
		cur := existing[itemID]
		if cur == nil {
			validated, err := todoValidate(raw)
			if err != nil {
				return err
			}
			s.items = append(s.items, validated)
			existing[validated.ID] = &s.items[len(s.items)-1]
			for i := range s.items {
				existing[s.items[i].ID] = &s.items[i]
			}
			continue
		}
		if truthy(t["content"]) {
			cur.Content = todoCapContent(strings.TrimSpace(fmt.Sprint(t["content"])))
		}
		if truthy(t["status"]) {
			if st := strings.ToLower(strings.TrimSpace(fmt.Sprint(t["status"]))); todoValidStatuses[st] {
				cur.Status = st
			}
		}
		if _, has := t["parent"]; has {
			parent := ""
			if t["parent"] != nil {
				parent = strings.TrimSpace(fmt.Sprint(t["parent"]))
			}
			cur.Parent = parent
		}
	}
	seen := map[string]bool{}
	rebuilt := make([]TodoItem, 0, len(s.items))
	for _, item := range s.items {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		rebuilt = append(rebuilt, *existing[item.ID])
	}
	s.items = todoNormalizeOrder(rebuilt)
	return nil
}

// Restore loads a trusted snapshot without manufacturing a new revision
func (s *TodoStore) Restore(todos []any, revision any) []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := todoFreshItems(todos)
	if err != nil {
		items = nil
	}
	if len(items) > MaxTodoItems {
		items = items[:MaxTodoItems]
	}
	s.items = items
	s.revision = 0
	if n, ok := pyIntValue(revision); ok && n > 0 {
		s.revision = n
	}
	return s.readLocked()
}

// FormatForInjection renders the active items for post compression injection, empty when nothing is active
func (s *TodoStore) FormatForInjection() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return ""
	}
	children := map[string][]TodoItem{}
	for _, item := range s.items {
		if item.Parent != "" {
			children[item.Parent] = append(children[item.Parent], item)
		}
	}
	var render func(item TodoItem, depth int, out *[]string) bool
	render = func(item TodoItem, depth int, out *[]string) bool {
		var kidLines []string
		hasActiveKid := false
		for _, kid := range children[item.ID] {
			if render(kid, depth+1, &kidLines) {
				hasActiveKid = true
			}
		}
		keep := todoActiveStatuses[item.Status] || hasActiveKid
		if keep {
			marker, ok := todoStatusMarkers[item.Status]
			if !ok {
				marker = "[?]"
			}
			*out = append(*out, fmt.Sprintf("%s- %s %s. %s (%s)", strings.Repeat("  ", depth), marker, item.ID, item.Content, item.Status))
			*out = append(*out, kidLines...)
		}
		return keep
	}
	lines := []string{TodoInjectionHeader}
	for _, item := range s.items {
		if item.Parent == "" {
			render(item, 0, &lines)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// Truncates to the content cap keeping the head plus the marker
func todoCapContent(content string) string {
	runes := []rune(content)
	if len(runes) > MaxTodoContentChars {
		return string(runes[:MaxTodoContentChars-len([]rune(todoTruncationMark))]) + todoTruncationMark
	}
	return content
}

// String field of a raw item the way str(item.get(key, "")) reads it
func todoField(t map[string]any, key string) string {
	v, ok := t[key]
	if !ok || v == nil {
		if ok {
			return "None"
		}
		return ""
	}
	return fmt.Sprint(v)
}

// Normalizes one raw item to its row, placeholders when fields are missing
func todoValidate(raw any) (TodoItem, error) {
	t, ok := raw.(map[string]any)
	if !ok {
		return TodoItem{ID: "?", Content: "(invalid item)", Status: "pending"}, nil
	}
	itemID := strings.TrimSpace(todoField(t, "id"))
	if itemID == "" {
		itemID = "?"
	}
	content := strings.TrimSpace(todoField(t, "content"))
	if content == "" {
		return TodoItem{}, fmt.Errorf("Todo item '%s' has empty or missing content. Each item must have a non-empty description.", itemID)
	}
	status := "pending"
	if v, ok := t["status"]; ok {
		status = strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	}
	if !todoValidStatuses[status] {
		status = "pending"
	}
	item := TodoItem{ID: itemID, Content: todoCapContent(content), Status: status}
	if truthy(t["parent"]) {
		if parent := strings.TrimSpace(fmt.Sprint(t["parent"])); parent != "" && parent != itemID {
			item.Parent = parent
		}
	}
	return item, nil
}

// Drops dangling parent refs and breaks cycles in place so such items become roots
func todoSanitizeParents(items []TodoItem) {
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

// Collapses duplicate ids keeping the last occurrence in its position
func todoDedupeByID(todos []any) []any {
	lastIndex := map[string]int{}
	for i, item := range todos {
		key := fmt.Sprintf("__invalid_%d", i)
		if m, ok := item.(map[string]any); ok {
			key = strings.TrimSpace(todoField(m, "id"))
		}
		if key == "" {
			key = "?"
		}
		lastIndex[key] = i
	}
	idx := make([]int, 0, len(lastIndex))
	for _, i := range lastIndex {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]any, len(idx))
	for i, j := range idx {
		out[i] = todos[j]
	}
	return out
}

// Lifts the in_progress step ahead of any earlier pending placeholder on flat lists
func todoNormalizeOrder(items []TodoItem) []TodoItem {
	active, firstPending := -1, -1
	for i, item := range items {
		if item.Parent != "" {
			return items
		}
		if item.Status == "in_progress" && active < 0 {
			active = i
		}
		if item.Status == "pending" && firstPending < 0 {
			firstPending = i
		}
	}
	if active < 0 || firstPending < 0 || firstPending >= active {
		return items
	}
	normalized := append([]TodoItem(nil), items...)
	moved := normalized[active]
	normalized = append(normalized[:active], normalized[active+1:]...)
	normalized = append(normalized[:firstPending], append([]TodoItem{moved}, normalized[firstPending:]...)...)
	return normalized
}

// TodoStoreOf returns the session store kept under env.Values["todo_list"], nil when absent
func TodoStoreOf(env *Env) *TodoStore {
	if env == nil || env.Values == nil {
		return nil
	}
	v, _ := env.Values.Load(TodoToolName)
	store, _ := v.(*TodoStore)
	return store
}

// Writes todos or reads the list, returning the list, revision and status summary JSON
func todoTool(store *TodoStore, todos any, merge bool) string {
	if store == nil {
		return toolErrorJSON("TodoStore not initialized", nil)
	}
	var items []TodoItem
	if todos == nil {
		items = store.Read()
	} else {
		list, ok := todos.([]any)
		if !ok {
			return toolErrorJSON(fmt.Sprintf("todos must be a list, got %s", pyTypeName(todos)), nil)
		}
		var err error
		items, err = store.Write(list, merge)
		if err != nil {
			return toolErrorJSON(err.Error(), nil)
		}
	}
	summary := map[string]any{"total": len(items)}
	for _, status := range []string{"pending", "in_progress", "completed", "cancelled"} {
		n := 0
		for _, it := range items {
			if it.Status == status {
				n++
			}
		}
		summary[status] = n
	}
	return jsonCompact(map[string]any{"todos": todoItemsJSON(items), "revision": store.Revision(), "summary": summary})
}

var todoSchema = map[string]any{
	"name": TodoToolName,
	"description": "Track a task list for multi-step work (3+ steps). Use for complex tasks " +
		"with 3+ steps or when the user provides multiple tasks. " +
		"For 'all N items' tasks, enumerate every instance as its own checklist " +
		"item so none are silently dropped. " +
		"Call with no parameters to read the current list.\n" +
		"List order is priority. Only ONE item in_progress at a time. " +
		"Break large phases into subtasks via parent. " +
		"Mark an item completed only after the work is verified done, never " +
		"based on intent. If something fails, cancel it and add a revised " +
		"item. Always returns the full current list.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "Task items to write.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "string"},
						"content": map[string]any{"type": "string", "description": "Task description"},
						"status":  map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed", "cancelled"}},
						"parent":  map[string]any{"type": "string", "description": "Optional id of another item, making this a nested subtask. Omit for top-level."},
					},
					"required": []any{"id", "content", "status"},
				},
			},
			"merge": map[string]any{
				"type":        "boolean",
				"description": "true: update existing items by id, add new ones. false (default): replace the entire list with a fresh plan.",
				"default":     false,
			},
		},
		"required": []any{},
	},
}

// IsTodoToolName is true for the todo tool's current name or a legacy alias
func IsTodoToolName(name string) bool {
	return name == TodoToolName || containsString(TodoLegacyAliases, name)
}

// IsTodoToolCall reports whether a transcript tool call invoked the todo tool directly or through the tool_call bridge
func IsTodoToolCall(name string, rawArgs any) bool {
	if IsTodoToolName(name) {
		return true
	}
	if s, ok := rawArgs.(string); ok && !strings.Contains(s, "todo") {
		return false
	}
	if name != ToolCallName {
		return false
	}
	var args map[string]any
	switch v := rawArgs.(type) {
	case string:
		decoded, err := pyjson.DecodeWTF8([]byte(v))
		if err != nil {
			return false
		}
		args, _ = decoded.(map[string]any)
	case map[string]any:
		args = v
	default:
		return false
	}
	if args == nil {
		return false
	}
	entries, errText := NormalizeToolCallEntries(args)
	return errText == "" && len(entries) == 1 && IsTodoToolName(entries[0].Name)
}

// Registers the todo tool, always available, reading the store from the session values
func registerTodoTool() {
	Register(Def{Name: TodoToolName, Toolset: "todo", Schema: todoSchema, Emoji: "\U0001F4CB",
		Check: func(*Env) bool { return true },
		Handler: func(ctx context.Context, call *Call) string {
			return todoTool(TodoStoreOf(call.Env), call.Args["todos"], argB(call.Args, "merge", false))
		}})
}
