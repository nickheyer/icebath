package kanban

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Copies the creator session and notify subscriptions inside the creating transaction without adding dependencies
func inheritCreatorOrigin(ctx context.Context, conn state.SQLiteConn, taskID string, creatorTaskID *string, createdAt int64) error {
	if creatorTaskID == nil || *creatorTaskID == "" {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET session_id = COALESCE(session_id, (SELECT session_id FROM tasks WHERE id = ?)) WHERE id = ?", *creatorTaskID, taskID); err != nil {
		return err
	}
	return inheritNotifySubs(ctx, conn, taskID, []string{*creatorTaskID}, &createdAt)
}

// Initial status and tenant of a new task, the first parent tenant filling an absent one and unknown parents refused
func initialTaskState(ctx context.Context, conn state.SQLiteConn, parents []string, initialStatus string, triage bool, tenant *string) (string, *string, error) {
	rows := map[string]*Row{}
	if len(parents) > 0 {
		args := make([]any, len(parents))
		for i, p := range parents {
			args[i] = p
		}
		found, err := QueryRows(ctx, conn, "SELECT id, status, tenant FROM tasks WHERE id IN ("+sqlMarks(len(parents))+")", args...)
		if err != nil {
			return "", nil, err
		}
		for _, r := range found {
			rows[cellStr(r.Get("id"))] = r
		}
		var missing []string
		for _, p := range parents {
			if rows[p] == nil {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return "", nil, &ValueError{"unknown parent task(s): " + strings.Join(missing, ", ")}
		}
		if tenant == nil {
			for _, p := range parents {
				if v := rows[p].Get("tenant"); truthy(v) {
					tenant = cellOptStr(v)
					break
				}
			}
		}
	}
	if initialStatus == "blocked" {
		return "blocked", tenant, nil
	}
	if triage {
		return "triage", tenant, nil
	}
	for _, r := range rows {
		if r.Get("status") != "done" {
			return "todo", tenant, nil
		}
	}
	return "ready", tenant, nil
}

// Value of a key in a decoded JSON object or a Go map
func dictGet(d any, key string) (any, bool) {
	switch m := d.(type) {
	case *pyjson.Object:
		return m.Get(key)
	case map[string]any:
		v, ok := m[key]
		return v, ok
	}
	return nil, false
}

// Reports whether a value is a dict
func isDict(d any) bool {
	switch d.(type) {
	case *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// Python int value of an integer or bool, false for any other type
func pyIndex(v any) (*big.Int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(n)), true
	case int64:
		return big.NewInt(n), true
	case *big.Int:
		return n, true
	}
	return nil, false
}

// Parent index list of a child spec, the falsy value counting as empty
func childParents(child any) any {
	v, _ := dictGet(child, "parents")
	if !truthy(v) {
		return []any{}
	}
	return v
}

// Shape check of decomposed children and a Kahn cycle check over their sibling graph
func validateChildrenGraph(children []any) error {
	n := len(children)
	for idx, child := range children {
		if !isDict(child) {
			return &ValueError{fmt.Sprintf("child[%d] is not a dict", idx)}
		}
		title, _ := dictGet(child, "title")
		if s, ok := title.(string); !ok || pystr.Strip(s) == "" {
			return &ValueError{fmt.Sprintf("child[%d].title is required", idx)}
		}
		list, ok := childParents(child).([]any)
		if !ok {
			return &ValueError{fmt.Sprintf("child[%d].parents must be a list", idx)}
		}
		for _, p := range list {
			i, ok := pyIndex(p)
			if !ok || i.Sign() < 0 || i.Cmp(big.NewInt(int64(n))) >= 0 {
				return &ValueError{fmt.Sprintf("child[%d].parents[%s] is not a valid index into children", idx, pyrepr.Str(p))}
			}
			if i.Int64() == int64(idx) {
				return &ValueError{fmt.Sprintf("child[%d] cannot list itself as a parent", idx)}
			}
		}
	}
	inDeg := make([]int, n)
	adj := make([][]int, n)
	for i, c := range children {
		for _, p := range childParents(c).([]any) {
			pi, _ := pyIndex(p)
			adj[pi.Int64()] = append(adj[pi.Int64()], i)
			inDeg[i]++
		}
	}
	var queue []int
	for i := range n {
		if inDeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	seen := 0
	for len(queue) > 0 {
		seen++
		node := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, nb := range adj[node] {
			inDeg[nb]--
			if inDeg[nb] == 0 {
				queue = append(queue, nb)
			}
		}
	}
	if seen != n {
		return &ValueError{"cyclic dependency detected in decomposed children list"}
	}
	return nil
}

// Fans a triage task out into children and parks the root in todo until every child is done, nil when it cannot decompose
func DecomposeTriageTask(ctx context.Context, conn state.SQLiteConn, taskID string, rootAssignee *string, children []any, author *string, autoPromote bool) ([]string, error) {
	if len(children) == 0 {
		return nil, nil
	}
	if rootAssignee != nil {
		var err error
		if rootAssignee, err = canonicalAssignee(rootAssignee); err != nil {
			return nil, err
		}
	}
	if err := validateChildrenGraph(children); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var childIDs []string
	err := WriteTxn(ctx, conn, false, func() error {
		root, err := QueryOne(ctx, conn, "SELECT id, status, tenant, workspace_kind, workspace_path FROM tasks WHERE id = ?", taskID)
		if err != nil || root == nil || root.Get("status") != "triage" {
			return err
		}
		done, err := QueryOne(ctx, conn, "SELECT 1 FROM task_events WHERE task_id = ? AND kind = 'decomposed' LIMIT 1", taskID)
		if err != nil || done != nil {
			return err
		}
		ids := make([]string, 0, len(children))
		for _, child := range children {
			id, err := insertDecomposedChild(ctx, conn, taskID, root, child, author, now)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		for idx, child := range children {
			for _, p := range childParents(child).([]any) {
				pi, _ := pyIndex(p)
				parentID, childID := ids[pi.Int64()], ids[idx]
				if err := linkRow(ctx, conn, parentID, childID); err != nil {
					return err
				}
				if err := appendEvent(ctx, conn, childID, "linked", payload("parent", parentID, "child", childID), nil); err != nil {
					return err
				}
			}
		}
		for _, cid := range ids {
			if err := linkRow(ctx, conn, cid, taskID); err != nil {
				return err
			}
		}
		sets := []string{"status = 'todo'"}
		var params []any
		if rootAssignee != nil {
			sets = append(sets, "assignee = ?")
			params = append(params, *rootAssignee)
		}
		params = append(params, taskID)
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ?", params...); err != nil {
			return err
		}
		if author != nil && pystr.Strip(*author) != "" {
			if err := insertComment(ctx, conn, taskID, pystr.Strip(*author), "Decomposed into "+strings.Join(ids, ", ")+". Root will wake when all children complete.", now); err != nil {
				return err
			}
		}
		idList := make([]any, len(ids))
		for i, id := range ids {
			idList[i] = id
		}
		if err := appendEvent(ctx, conn, taskID, "decomposed", payload("child_ids", idList, "root_assignee", rootAssignee), nil); err != nil {
			return err
		}
		childIDs = ids
		return nil
	})
	if err != nil || childIDs == nil {
		return nil, err
	}
	if autoPromote {
		if _, err := RecomputeReady(ctx, conn, nil); err != nil {
			return nil, err
		}
	}
	return childIDs, nil
}

// Optional string of a child spec value, an attribute error for a non string
func childAssignee(v any) (*string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case string:
		return canonicalAssignee(&s)
	}
	return nil, &RuntimeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))}
}

// Inserts one decomposed child as todo, inheriting the root workspace kind and its path only for a matching non worktree kind
func insertDecomposedChild(ctx context.Context, conn state.SQLiteConn, rootID string, root *Row, child any, author *string, now int64) (string, error) {
	rootKind := root.Get("workspace_kind")
	if !truthy(rootKind) {
		rootKind = "scratch"
	}
	childKind, _ := dictGet(child, "workspace_kind")
	if !truthy(childKind) {
		childKind = rootKind
	}
	var childPath any
	if p, _ := dictGet(child, "workspace_path"); truthy(p) {
		childPath = p
	} else if childKind == "worktree" {
		childPath = nil
	} else if childKind == rootKind {
		childPath = root.Get("workspace_path")
	}
	newID := newTaskID()
	var body any
	if b, _ := dictGet(child, "body"); b != nil {
		if s, ok := b.(string); ok {
			body = s
		}
	}
	rawAssignee, _ := dictGet(child, "assignee")
	assignee, err := childAssignee(rawAssignee)
	if err != nil {
		return "", err
	}
	by := "decomposer"
	if author != nil && *author != "" {
		by = *author
	}
	title, _ := dictGet(child, "title")
	if _, err := conn.ExecContext(ctx, "INSERT INTO tasks (id, title, body, assignee, status, workspace_kind,  workspace_path, tenant, created_at, created_by) VALUES (?, ?, ?, ?, 'todo', ?, ?, ?, ?, ?)",
		newID, pystr.Strip(title.(string)), body, optStrValue(assignee), childKind, childPath, root.Get("tenant"), now, by); err != nil {
		return "", err
	}
	if err := appendEvent(ctx, conn, newID, "created", payload("by", by, "from_decompose_of", rootID), nil); err != nil {
		return "", err
	}
	return newID, inheritCreatorOrigin(ctx, conn, newID, &rootID, now)
}
