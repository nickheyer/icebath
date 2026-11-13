package app

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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

// Browser launch and the two card answers, swapped by tests
var (
	openConnectionURL = provider.OpenBrowserURL
	connectionRespond = func(ctx context.Context, c *CLI, opID string, result map[string]any) error {
		_, err := c.agentRef().ConnectionRespond(opID, result)
		return err
	}
	connectionRetryFn = func(ctx context.Context, c *CLI, name string) error {
		_, err := c.agentRef().ConnectionRetry(name)
		return err
	}
)

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
	c.restoreModalInputSnapshot()
	c.paintNow()
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
	c.connectionSyncInputBuffer()
	c.paintNow()
}

// Reports whether the field being edited on the card is a secret
func (c *CLI) connectionActiveFieldIsSecret() bool {
	state := c.connectionModal()
	if state == nil || (state.Phase != "form" && state.Phase != "failed") {
		return false
	}
	return state.FieldIndex >= 0 && state.FieldIndex < len(state.Fields) && state.Fields[state.FieldIndex]["type"] == "secret"
}

// Draft text of the field being edited, empty for a secret or an action row
func (c *CLI) connectionPrefillText() string {
	state := c.connectionModal()
	if state == nil || c.connectionActiveFieldIsSecret() || state.FieldIndex < 0 || state.FieldIndex >= len(state.Fields) {
		return ""
	}
	return state.Drafts[cliStr(state.Target["name"], "")][cliStr(state.Fields[state.FieldIndex]["name"], "")]
}

// Loads the draft of the field being edited into the prompt buffer
func (c *CLI) connectionSyncInputBuffer() {
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if app == nil {
		return
	}
	text := c.connectionPrefillText()
	app.SetBuffer(text, len([]rune(text)))
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
	if state == nil {
		c.captureModalInputSnapshot()
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

// Sends one card answer off the loop, a refusal shown on the card as failed
func (c *CLI) connectionSend(call func(ctx context.Context) error) {
	state := c.connectionModal()
	if state == nil || c.agentRef() == nil {
		c.connectionClose()
		return
	}
	opID := state.Payload["op_id"]
	memory.SpawnContextThread(c.ctx, "icebath-connection-answer", func(ctx context.Context) {
		err := call(ctx)
		if err == nil {
			return
		}
		current := c.connectionModal()
		if current == nil || current.Payload["op_id"] != opID {
			return
		}
		detail := err.Error()
		if detail == "" {
			detail = c.t("cli.connect.cannot_restart", nil)
		}
		target := make(map[string]any, len(current.Target)+1)
		for k, v := range current.Target {
			target[k] = v
		}
		target["detail"] = detail
		current.Target, current.Phase = target, "failed"
		c.paintNow()
	}).Start()
}

// Answers the active target, approved with its draft values or skipped
func (c *CLI) connectionAnswer(approve bool) {
	state := c.connectionModal()
	if state == nil {
		return
	}
	name := cliStr(state.Target["name"], "")
	answer := map[string]any{"name": name, "status": "skipped"}
	if approve {
		env := map[string]any{}
		for k, v := range state.Drafts[name] {
			env[k] = v
		}
		answer["status"], answer["env"] = "approved", env
	}
	state.Phase = "waiting"
	c.paintNow()
	opID := cliStr(state.Payload["op_id"], "")
	c.connectionSend(func(ctx context.Context) error {
		return connectionRespond(ctx, c, opID, map[string]any{"targets": []any{answer}})
	})
}

// Retries the active target, a pending or failed install row answered again and a dead sign in link minted anew
func (c *CLI) connectionRetry() {
	state := c.connectionModal()
	if state == nil {
		return
	}
	targetState := cliStr(state.Target["state"], "")
	retryable := targetState == "pending" || targetState == "failed" || targetState == "expired"
	deadLink := cliStr(state.Target["kind"], "") == "connector" && (targetState == "failed" || targetState == "expired")
	if !deadLink && retryable {
		c.connectionAnswer(true)
		return
	}
	state.Phase = "waiting"
	c.paintNow()
	name := cliStr(state.Target["name"], "")
	c.connectionSend(func(ctx context.Context) error { return connectionRetryFn(ctx, c, name) })
}

// Settles the operation by Continue, the card staying open until the settling update closes it
func (c *CLI) connectionContinue() {
	state := c.connectionModal()
	if state == nil {
		return
	}
	state.Phase = "waiting"
	c.paintNow()
	opID := cliStr(state.Payload["op_id"], "")
	c.connectionSend(func(ctx context.Context) error {
		return connectionRespond(ctx, c, opID, map[string]any{"settled_by": "continue"})
	})
}

// Opens the sign in link of the active target in the browser
func (c *CLI) connectionOpenURL() {
	if state := c.connectionModal(); state != nil && state.Phase == "url" {
		if url := cliStr(state.Target["connect_url"], ""); url != "" {
			_ = openConnectionURL(url)
		}
	}
}

// Skips the active target
func (c *CLI) connectionCancel() { c.connectionAnswer(false) }

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
		c.connectionSyncInputBuffer()
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
	c.paintNow()
}

// Saves the typed value of the active field in the draft and moves to the next row
func (c *CLI) connectionSetField(value string) {
	state := c.connectionModal()
	if state == nil || (state.Phase != "form" && state.Phase != "failed") || state.FieldIndex < 0 || state.FieldIndex >= len(state.Fields) {
		return
	}
	name := cliStr(state.Target["name"], "")
	if state.Drafts[name] == nil {
		state.Drafts[name] = map[string]string{}
	}
	state.Drafts[name][cliStr(state.Fields[state.FieldIndex]["name"], "")] = value
	state.FieldIndex = min(state.FieldIndex+1, len(state.Fields))
	c.connectionSyncInputBuffer()
	c.paintNow()
}

// Enter on the card, opening the link, continuing, or running the selected action
func (c *CLI) connectionSubmit() {
	state := c.connectionModal()
	if state == nil {
		return
	}
	switch {
	case state.Phase == "url":
		c.connectionOpenURL()
	case state.Phase == "authorized":
		c.connectionContinue()
	case (state.Phase == "form" || state.Phase == "failed") && state.FieldIndex >= len(state.Fields):
		if state.Selected == 0 {
			c.connectionRetry()
		} else {
			c.connectionCancel()
		}
	}
}

// One line per input field with its draft value, a set secret never shown
func (c *CLI) connectionFieldLines(state *connectionModalState, target map[string]any) []string {
	draft := state.Drafts[cliStr(target["name"], "")]
	var lines []string
	for _, field := range state.Fields {
		marker := ""
		if pyTruthy(field["required"]) {
			marker = "*"
		}
		name := cliStr(field["name"], "")
		value := draft[name]
		if field["type"] == "secret" && value != "" {
			value = c.t("cli.connect.secret_set", nil)
		}
		label := cliStr(field["prompt"], "")
		if label == "" {
			label = name
		}
		lines = append(lines, fmt.Sprintf("%s%s: %s", label, marker, value))
	}
	return lines
}

// Rows of the card copy for its phase, the title first and empty rows dropped
func (c *CLI) connectionRenderLines() []string {
	state := c.connectionModal()
	if state == nil {
		return nil
	}
	target := state.Target
	buttons := c.t("cli.connect.button_connect", nil) + "    " + c.t("cli.connect.button_cancel", nil)
	lines := []string{c.t("cli.connect.set_up", map[string]any{"name": cliStr(target["name"], "")})}
	if instructions := cliStr(target["instructions"], ""); instructions != "" {
		lines = append(lines, instructions)
	}
	detailOr := func(key string) string {
		if detail := cliStr(target["detail"], ""); detail != "" {
			return detail
		}
		return c.t(key, nil)
	}
	switch state.Phase {
	case "form":
		lines = append(append(lines, c.connectionFieldLines(state, target)...), buttons)
	case "url":
		lines = append(lines, cliStr(target["connect_url"], ""), cliStr(target["detail"], ""), c.t("cli.connect.press_enter_open", nil))
	case "failed":
		lines = append(lines, detailOr("cli.connect.failed"))
		lines = append(append(lines, c.connectionFieldLines(state, target)...), buttons)
	case "authorized":
		lines = append(lines, c.t("cli.connect.authorized_no_tools", nil), c.t("cli.connect.button_continue", nil))
	case "connected":
		lines = append(lines, c.t("cli.connect.connected", nil))
		if alias := cliStr(target["alias"], ""); alias != "" {
			lines = append(lines, c.t("cli.connect.named_account", map[string]any{"name": tools.AppTitle(cliStr(target["name"], "")), "alias": alias}))
		}
	default:
		lines = append(lines, detailOr("cli.connect.waiting"))
	}
	out := lines[:0]
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
