package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// connectionModalState is the open connection setup card
type connectionModalState struct {
	Payload     map[string]any
	TargetIndex int
	Target      map[string]any
	Fields      []map[string]any
	FieldIndex  int
	Selected    int
	Phase       string
	Drafts      map[string]map[string]string
}

// Rows of a payload list as maps
func connectionRows(v any) []map[string]any {
	var out []map[string]any
	switch rows := pyjson.Plain(v).(type) {
	case []any:
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case []map[string]any:
		out = rows
	}
	return out
}

// Integer sequence number of a payload, zero when absent
func connectionSeq(payload map[string]any) int {
	n, _ := pyIntOf(payload["seq"])
	return n
}

// Closes the connection card and restores the draft
func (c *CLI) connectionClose() {
	c.uiMu.Lock()
	c.connectionState = nil
	c.uiMu.Unlock()
}

// Copies of the input fields a target requires
func connectionFields(target map[string]any) []map[string]any {
	var out []map[string]any
	for _, field := range connectionRows(target["required_env"]) {
		copied := make(map[string]any, len(field))
		for k, v := range field {
			copied[k] = v
		}
		out = append(out, copied)
	}
	return out
}

// Phase a target opens in, a pending install waiting for Connect and a pending authorize waiting on the backend
func connectionOpeningPhase(target map[string]any) string {
	state := cliStr(target["state"], "")
	switch {
	case state == "initiated" && cliStr(target["connect_url"], "") != "":
		return "url"
	case state == "failed":
		return "failed"
	case state == "pending" && cliStr(target["action"], "") != "authorize":
		return "form"
	}
	return "waiting"
}

// Shows the target at index on the card, earlier drafts kept and plain fields seeded with their defaults
func (c *CLI) connectionShowTarget(payload map[string]any, index int) {
	targets := connectionRows(payload["targets"])
	if len(targets) == 0 {
		c.connectionClose()
		return
	}
	index = max(0, min(index, len(targets)-1))
	target := targets[index]
	fields := connectionFields(target)
	drafts := map[string]map[string]string{}
	if previous := c.connectionModal(); previous != nil && previous.Drafts != nil {
		drafts = previous.Drafts
	}
	name := cliStr(target["name"], "")
	if drafts[name] == nil {
		drafts[name] = map[string]string{}
	}
	for _, field := range fields {
		if _, typed := field["type"]; !typed {
			field["type"] = "plain"
			if pyTruthy(field["secret"]) {
				field["type"] = "secret"
			}
		}
		fieldName := cliStr(field["name"], "")
		if _, drafted := drafts[name][fieldName]; field["type"] != "secret" && !drafted {
			drafts[name][fieldName] = ""
			if pyTruthy(field["default"]) {
				drafts[name][fieldName] = cliStr(field["default"], "")
			}
		}
	}
	c.uiMu.Lock()
	c.connectionState = &connectionModalState{Payload: payload, TargetIndex: index, Target: target, Fields: fields,
		Phase: connectionOpeningPhase(target), Drafts: drafts}
	c.uiMu.Unlock()
}

// Opens the card for a connection request, a replay of the open card applied as an update
func (c *CLI) connectionCallback(args ...any) {
	if len(args) == 0 {
		return
	}
	payload, ok := pyjson.Plain(args[0]).(map[string]any)
	if !ok {
		return
	}
	state := c.connectionModal()
	if state != nil && state.Payload["op_id"] == payload["op_id"] {
		if connectionSeq(payload) > connectionSeq(state.Payload) {
			c.connectionOnChange(payload)
		}
		return
	}
	index := 0
	for i, item := range connectionRows(payload["targets"]) {
		if s := cliStr(item["state"], ""); s != "connected" && s != "skipped" {
			index = i
			break
		}
	}
	c.connectionShowTarget(payload, index)
	if state = c.connectionModal(); state != nil && state.Phase != "waiting" {
		c.ringBell(true, c.t("cli.connect.bell_context", nil), "")
	}
}

// Closes the card when the turn is interrupted, the operation itself ended by the interrupt
func (c *CLI) connectionInterrupt() { c.connectionClose() }

// Applies an operation frame to the open card, moving phase, reopening the form or closing on settle
func (c *CLI) connectionOnChange(snapshot map[string]any) {
	state := c.connectionModal()
	if state == nil || snapshot == nil {
		return
	}
	_, hasOp := snapshot["op_id"]
	if !hasOp && pyTruthy(snapshot["settled"]) {
		c.connectionClose()
		return
	}
	if snapshot["op_id"] != state.Payload["op_id"] || connectionSeq(snapshot) < connectionSeq(state.Payload) {
		return
	}
	if pyTruthy(snapshot["settled"]) || snapshot["settled_at"] != nil {
		c.connectionClose()
		return
	}
	targets := connectionRows(snapshot["targets"])
	var target map[string]any
	for _, item := range targets {
		if item["name"] == state.Target["name"] {
			target = item
			break
		}
	}
	if target == nil {
		return
	}
	state.Payload, state.Target = snapshot, target
	targetState := cliStr(target["state"], "")
	switch {
	case targetState == "initiated" && cliStr(target["connect_url"], "") != "":
		state.Phase = "url"
	case targetState == "failed":
		state.Phase = "failed"
	case targetState == "pending" && len(connectionFields(target)) > 0:
		if state.Phase == "form" {
			return
		}
		missing := map[string]bool{}
		for _, field := range connectionFields(target) {
			missing[cliStr(field["name"], "")] = true
		}
		state.FieldIndex = 0
		for i, field := range state.Fields {
			if missing[cliStr(field["name"], "")] {
				state.FieldIndex = i
				break
			}
		}
		state.Phase = "form"
	case targetState == "connected" && pyTruthy(target["discovery_error"]):
		state.Phase = "authorized"
	case targetState == "connected" || targetState == "skipped":
		for i, item := range targets {
			if s := cliStr(item["state"], ""); s != "connected" && s != "skipped" {
				c.connectionShowTarget(snapshot, i)
				return
			}
		}
		state.Phase = "connected"
	default:
		state.Phase = "waiting"
	}
}
