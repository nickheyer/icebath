package acp

import (
	"context"
	"sync"

	"github.com/google/uuid"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Client side of the connection that receives session updates
type SessionUpdater interface {
	SessionUpdate(ctx context.Context, sessionID string, update acputil.SessionUpdate, meta *jsonutil.Object) error
}

var planStatus = map[string]string{"pending": "pending", "in_progress": "in_progress", "completed": "completed", "cancelled": "completed"}

// Native ACP plan update of a task list tool result, nil when the result carries no task list
func buildPlanUpdateFromTodoResult(result any) *acputil.AgentPlanUpdate {
	s, ok := result.(string)
	if !ok || strip(s) == "" {
		return nil
	}
	data, ok := isDict(jsonLoadsMaybe(&s))
	if !ok {
		return nil
	}
	todos, ok := isList(dget(data, "todos"))
	if !ok {
		return nil
	}
	entries := []acputil.PlanEntry{}
	for _, it := range todos {
		item, ok := isDict(it)
		if !ok {
			continue
		}
		content := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(item, "content"), dget(item, "id")), "")))
		if content == "" {
			continue
		}
		raw := strip(anyutil.String(anyutil.Or(dget(item, "status"), "pending")))
		if raw == "cancelled" {
			content = "[cancelled] " + content
		}
		st, ok := planStatus[raw]
		if !ok {
			st = "pending"
		}
		entries = append(entries, acputil.NewPlanEntry(content, "medium", st))
	}
	return acputil.UpdatePlan(entries)
}

// Stable uuid per streamed assistant message, a new id after each close
type AssistantMessageIDAllocator struct {
	mu     sync.Mutex
	active string
	last   string
}

// Active message id, allocating one when none is open
func (a *AssistantMessageIDAllocator) Current() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == "" {
		a.active = uuid.NewString()
		a.last = a.active
	}
	return a.active
}

// Most recently allocated id, empty before the first
func (a *AssistantMessageIDAllocator) Last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Ends the active message so the next chunk starts a new id
func (a *AssistantMessageIDAllocator) Close() {
	a.mu.Lock()
	a.active = ""
	a.mu.Unlock()
}
