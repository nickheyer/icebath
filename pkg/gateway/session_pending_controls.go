package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One projected prompt with the approval route or clarify entry its native waiter blocks on
type pendingPrompt struct {
	route  any
	prompt *pyjson.Object
}

// Generation bound projection and control of the blocking approval and clarify prompts of one session
type PendingControls struct {
	events           *SessionEvents
	pending          map[string]pendingPrompt
	order            []string
	RemoteResponders map[string]func(kind, promptID, value string)
}

// Controls publishing on a session's event stream
func NewPendingControls(events *SessionEvents) *PendingControls {
	return &PendingControls{events: events, pending: map[string]pendingPrompt{}, RemoteResponders: map[string]func(kind, promptID, value string){}}
}

// Controls of a live session, created with its stream on first use
func (l *LiveSession) pendingControls() *PendingControls {
	l.controlsOnce.Do(func() { l.controls = NewPendingControls(l.EventStream) })
	return l.controls
}

// Approval text with credentials redacted and hidden characters surfaced, whatever the redaction setting
func redactApprovalCommand(ctx context.Context, text any) string {
	command := ""
	if text != nil {
		command = pyrepr.Str(text)
	}
	return tools.RedactApprovalDisplay(urlSafetyEnv(ctx), command, true)
}

// Records a prompt in registration order
func (c *PendingControls) hold(promptID string, route any, prompt *pyjson.Object) {
	if _, held := c.pending[promptID]; !held {
		c.order = append(c.order, promptID)
	}
	c.pending[promptID] = pendingPrompt{route, prompt}
}

// Forgets a prompt and its remote responder
func (c *PendingControls) drop(promptID string) {
	delete(c.pending, promptID)
	delete(c.RemoteResponders, promptID)
	for i, id := range c.order {
		if id == promptID {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// Truth of an approval option that defaults on
func approvalOption(data map[string]any, key string, def bool) bool {
	if value, ok := data[key]; ok {
		return pyTruthy(value)
	}
	return def
}

// Projects an approval request of the generation to the session's viewers
func (c *PendingControls) register(ctx context.Context, sessionID, route string, generation int64, data map[string]any, timeoutS *float64) error {
	c.events.Lock.Lock()
	defer c.events.Lock.Unlock()
	requestID, ok := data["request_id"]
	if !ok {
		return &pyKeyError{"request_id"}
	}
	promptID := pyrepr.Str(requestID)
	choices := []any{"once", "deny"}
	if approvalOption(data, "allow_session", true) && !approvalOption(data, "smart_denied", false) {
		choices = append(choices, "session")
	}
	if approvalOption(data, "allow_permanent", true) && !approvalOption(data, "smart_denied", false) {
		choices = append(choices, "always")
	}
	command, hasCommand := data["command"]
	if !hasCommand {
		command = ""
	}
	description, hasDescription := data["description"]
	if !hasDescription {
		description = ""
	}
	prompt := pyjson.ObjectOf("kind", "approval", "prompt_id", promptID, "execution_generation", generation,
		"command", redactApprovalCommand(ctx, command), "description", redactApprovalCommand(ctx, description), "choices", choices)
	if edit, ok := data["edit"]; ok {
		prompt.Set("edit", pyjson.Clone(edit))
		prompt.Set("choices", []any{"once", "deny"})
	}
	if timeoutS != nil {
		prompt.Set("timeout_s", *timeoutS)
	}
	c.hold(promptID, route, prompt)
	c.events.Publish(sessionID, prompt, "approval.request", nil)
	return nil
}

// Projects a clarify question of the generation to the session's viewers
func (c *PendingControls) registerClarify(ctx context.Context, sessionID string, generation int64, entry *tools.ClarifyEntry, timeoutS *float64) error {
	c.events.Lock.Lock()
	defer c.events.Lock.Unlock()
	choices := make([]any, 0, len(entry.Choices))
	for _, choice := range entry.Choices {
		choices = append(choices, redactApprovalCommand(ctx, choice))
	}
	prompt := pyjson.ObjectOf("kind", "clarify", "prompt_id", entry.ClarifyID, "execution_generation", generation,
		"question", redactApprovalCommand(ctx, entry.Question), "choices", choices, "multi_select", entry.MultiSelect)
	if timeoutS != nil {
		prompt.Set("timeout_s", *timeoutS)
	}
	c.hold(entry.ClarifyID, entry, prompt)
	c.events.Publish(sessionID, prompt, "clarify.request", nil)
	return nil
}

// True while a prompt's native waiter still blocks or a remote responder holds it
func (c *PendingControls) active(promptID string, held pendingPrompt) bool {
	if _, remote := c.RemoteResponders[promptID]; remote {
		return true
	}
	if kind, _ := held.prompt.Get("kind"); kind == "clarify" {
		entry, _ := held.route.(*tools.ClarifyEntry)
		return entry != nil && !entry.Resolved()
	}
	route, _ := held.route.(string)
	for _, approval := range tools.ListGatewayApprovals(route) {
		if approval["request_id"] == promptID {
			return true
		}
	}
	return false
}

// Settles every prompt that is not the generation's or no longer blocks, returning copies of the rest
func (c *PendingControls) snapshot(sessionID string, generation *int64) []map[string]any {
	c.events.Lock.Lock()
	defer c.events.Lock.Unlock()
	for _, promptID := range append([]string(nil), c.order...) {
		held := c.pending[promptID]
		stamped, _ := held.prompt.Get("execution_generation")
		if generation == nil || stamped != *generation || !c.active(promptID, held) {
			kind, _ := held.prompt.Get("kind")
			c.drop(promptID)
			c.events.Publish(sessionID, pyjson.ObjectOf("prompt_id", promptID, "execution_generation", stamped), pyrepr.Str(kind)+".settled", nil)
		}
	}
	out := make([]map[string]any, 0, len(c.order))
	for _, promptID := range c.order {
		out = append(out, pyjson.Plain(pyjson.Clone(c.pending[promptID].prompt)).(map[string]any))
	}
	return out
}

// The single string field a response mapping carries, false for any other shape
func responseField(response any, field string) (string, bool) {
	item, ok := attachmentItem(response)
	if !ok || len(item) != 1 {
		return "", false
	}
	value, ok := item[field].(string)
	return value, ok
}

// Answers a prompt of the generation, already resolved when its waiter stopped blocking first
func (c *PendingControls) respond(sessionID string, generation int64, promptID string, response any, kind string) (*pyjson.Object, error) {
	c.events.Lock.Lock()
	defer c.events.Lock.Unlock()
	c.snapshot(sessionID, &generation)
	held, ok := c.pending[promptID]
	if !ok {
		return pyjson.ObjectOf("status", "already_resolved", "prompt_id", promptID), nil
	}
	if promptKind, _ := held.prompt.Get("kind"); promptKind != kind {
		return nil, runtimeRefusal("invalid_params")
	}
	choices, _ := held.prompt.Get("choices")
	offered := func(choice string) bool {
		list, _ := choices.([]any)
		for _, item := range list {
			if item == choice {
				return true
			}
		}
		return false
	}
	status := func(resolved bool) *pyjson.Object {
		if resolved {
			return pyjson.ObjectOf("status", "resolved", "prompt_id", promptID)
		}
		return pyjson.ObjectOf("status", "already_resolved", "prompt_id", promptID)
	}
	if responder, remote := c.RemoteResponders[promptID]; remote {
		field := "choice"
		if kind == "clarify" {
			field = "answer"
		}
		value, ok := responseField(response, field)
		if !ok || pystr.Len(value) > 16384 || (kind == "approval" && !offered(value)) {
			return nil, runtimeRefusal("invalid_params")
		}
		responder(kind, promptID, value)
		c.drop(promptID)
		c.events.Publish(sessionID, pyjson.ObjectOf("prompt_id", promptID, "execution_generation", generation), kind+".settled", nil)
		return status(true), nil
	}
	if kind == "clarify" {
		answer, ok := responseField(response, "answer")
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		resolved := tools.ResolveGatewayClarify(promptID, answer)
		c.snapshot(sessionID, &generation)
		return status(resolved), nil
	}
	choice, ok := responseField(response, "choice")
	if !ok || !offered(choice) {
		return nil, runtimeRefusal("invalid_params")
	}
	route, _ := held.route.(string)
	resolved := tools.ResolveGatewayApproval(route, choice, false, "", promptID)
	c.snapshot(sessionID, &generation)
	return status(resolved != 0), nil
}

// Prompts of a live session still pending for the generation
func pendingControlsSnapshot(live *LiveSession, sessionID string, generation *int64) []map[string]any {
	return live.pendingControls().snapshot(sessionID, generation)
}

// Registers an approval request with a live session's controls
func pendingControlsRegister(ctx context.Context, live *LiveSession, sessionID, route string, generation int64, data map[string]any, timeoutS *float64) error {
	return live.pendingControls().register(ctx, sessionID, route, generation, data, timeoutS)
}

// Registers a clarify question with a live session's controls
func pendingControlsRegisterClarify(ctx context.Context, live *LiveSession, sessionID string, generation int64, entry *tools.ClarifyEntry, timeoutS *float64) error {
	return live.pendingControls().registerClarify(ctx, sessionID, generation, entry, timeoutS)
}

// Answers a prompt through a live session's controls
func pendingControlsRespond(live *LiveSession, sessionID string, generation int64, promptID string, response any, kind string) (*pyjson.Object, error) {
	return live.pendingControls().respond(sessionID, generation, promptID, response, kind)
}
