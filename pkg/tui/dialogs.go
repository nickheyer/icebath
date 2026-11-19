package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Widest a dialog asks to be
const dialogWidth = 84

// One answer a dialog offers
type dialogChoice struct {
	label  string
	detail string
	value  string
}

// One line the user types into a dialog, masked for credentials
type dialogField struct {
	label  string
	value  []rune
	masked bool
}

// Dialog over the session, a message with choices to pick, fields to fill, or both
type dialogCard struct {
	app       *app
	kind      string
	requestID string
	title     string
	body      []string
	choices   []dialogChoice
	sel       int
	multi     bool
	marks     map[int]bool
	fields    []dialogField
	focus     int
	danger    bool
	scroll    int
	bodyRows  int
	settled   bool
	// Answer picked, the marked values of a multiple choice and the fields as typed
	onAnswer func(choice *dialogChoice, marked []string, fields []string) tea.Cmd
	onCancel func() tea.Cmd
	cursor   *tea.Cursor
}

func (d *dialogCard) Title() string  { return d.title }
func (d *dialogCard) Key() string    { return "dialog:" + d.kind + ":" + d.requestID }
func (d *dialogCard) Init() tea.Cmd  { return nil }
func (d *dialogCard) Glyph() string  { return d.app.uiTheme().Glyphs.Ask }
func (d *dialogCard) Closable() bool { return true }

// Approval keys for a verdict dialog, the picker keys the manager adds serve every other kind
func (d *dialogCard) Contexts() []string {
	if d.kind == "approval" {
		return []string{keys.Approval}
	}
	return nil
}

// Rows the dialog needs at its width
func (d *dialogCard) MinSize() (int, int) {
	w, _ := d.app.wm.Size()
	width := min(dialogWidth, max(w-8, 30))
	return width, len(d.rows(width))
}

// Body paragraphs wrapped, then the choices and the fields
func (d *dialogCard) rows(width int) []string {
	t := d.app.store.ui.Theme
	var rows []string
	for _, para := range d.body {
		for _, line := range strings.Split(ansi.Wrap(para, max(width-2, 8), ""), "\n") {
			tone := t.Color.Text
			if d.danger {
				tone = t.Color.Warn
			}
			rows = append(rows, " "+toneStyle(tone).Render(line))
		}
	}
	d.bodyRows = len(rows)
	if len(rows) > 0 && (len(d.choices) > 0 || len(d.fields) > 0) {
		rows = append(rows, "")
	}
	for i, c := range d.choices {
		mark := "  "
		if d.multi {
			mark = "☐ "
			if d.marks[i] {
				mark = "☑ "
			}
		}
		label := fmt.Sprintf(" %d %s%s", i+1, mark, c.label)
		if c.detail != "" {
			label += toneStyle(t.Color.Muted).Render("  " + truncateEnd(c.detail, max(width-ansi.StringWidth(label)-4, 4)))
		}
		if i == d.sel && d.focus < 0 {
			rows = append(rows, toneStyle(t.Color.Accent).Bold(true).Render("▸")+toneStyle(t.Color.Text).Bold(true).Render(label))
		} else {
			rows = append(rows, " "+toneStyle(t.Color.Muted).Render(label))
		}
	}
	d.cursor = nil
	for i, f := range d.fields {
		shown := string(f.value)
		if f.masked {
			shown = strings.Repeat("•", len(f.value))
		}
		lead := " " + f.label + " "
		room := max(width-ansi.StringWidth(lead)-2, 4)
		if ansi.StringWidth(shown) > room {
			shown = ansi.TruncateLeft(shown, ansi.StringWidth(shown)-room, "")
		}
		tone := t.Color.Muted
		if i == d.focus {
			tone = t.Color.Accent
			d.cursor = tea.NewCursor(ansi.StringWidth(lead)+ansi.StringWidth(shown), len(rows))
		}
		rows = append(rows, toneStyle(tone).Render(lead)+toneStyle(t.Color.Text).Render(shown))
	}
	return rows
}

func (d *dialogCard) View(w, h int) string {
	rows := d.rows(w)
	if len(rows) > h {
		keep := len(rows) - d.bodyRows
		room := max(h-keep, 1)
		d.scroll = min(max(d.scroll, 0), max(d.bodyRows-room, 0))
		body := rows[d.scroll:min(d.scroll+room, d.bodyRows)]
		rows = append(append([]string{}, body...), rows[d.bodyRows:]...)
		if d.cursor != nil {
			d.cursor.Y -= d.bodyRows - len(body)
		}
	}
	return strings.Join(rows[:min(len(rows), h)], "\n")
}

func (d *dialogCard) Cursor() *tea.Cursor { return d.cursor }

// Legend of the dialog keys
func (d *dialogCard) Hints() []wm.Hint {
	if d.kind == "approval" {
		return []wm.Hint{{Action: "approval.allow"}, {Action: "approval.deny"}, {Action: "palette.pick", Label: "choose"}}
	}
	return []wm.Hint{{Action: "palette.pick", Label: "answer"}, {Action: "palette.close", Label: "cancel"}}
}

// Moves the highlight through the choices and then the fields
func (d *dialogCard) move(step int) {
	total := len(d.choices) + len(d.fields)
	if total == 0 {
		return
	}
	at := d.sel
	if d.focus >= 0 {
		at = len(d.choices) + d.focus
	}
	at = (at + step + total) % total
	if at < len(d.choices) {
		d.sel, d.focus = at, -1
		return
	}
	d.focus = at - len(d.choices)
}

// Field holding the focus, nil while a choice is highlighted
func (d *dialogCard) field() *dialogField {
	if d.focus < 0 || d.focus >= len(d.fields) {
		return nil
	}
	return &d.fields[d.focus]
}

// Answers with the highlighted choice or the typed fields
func (d *dialogCard) answer() tea.Cmd {
	if d.settled {
		return nil
	}
	values := make([]string, len(d.fields))
	for i, f := range d.fields {
		values[i] = string(f.value)
	}
	if d.focus >= 0 && d.focus < len(d.fields)-1 && len(d.choices) == 0 {
		d.focus++
		return nil
	}
	var marked []string
	for i, c := range d.choices {
		if d.marks[i] {
			marked = append(marked, c.value)
		}
	}
	var choice *dialogChoice
	if d.focus < 0 && d.sel < len(d.choices) {
		choice = &d.choices[d.sel]
	}
	d.settled = true
	return tea.Batch(d.onAnswer(choice, marked, values), wm.RequestDismissOf(d))
}

// Answers with the choice of a value, as the verdict keys of an approval do
func (d *dialogCard) pick(value string) tea.Cmd {
	for i, c := range d.choices {
		if c.value == value {
			d.sel, d.focus = i, -1
			return d.answer()
		}
	}
	return nil
}

func (d *dialogCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.FocusMsg:
		if !m.Focused && !d.settled {
			d.settled = true
			if d.onCancel != nil {
				return d, d.onCancel()
			}
		}
	case wm.ActionMsg:
		switch m.ID {
		case "palette.up", "approval.up":
			d.move(-1)
		case "palette.down", "approval.down":
			d.move(1)
		case "palette.page.up", "approval.page.up":
			d.scroll -= 5
		case "palette.page.down", "approval.page.down":
			d.scroll += 5
		case "palette.pick":
			return d, d.answer()
		case "approval.allow":
			return d, d.pick("once")
		case "approval.deny":
			return d, d.pick("deny")
		case "approval.always":
			return d, d.pick("always")
		case "palette.delete.back":
			if f := d.field(); f != nil && len(f.value) > 0 {
				f.value = f.value[:len(f.value)-1]
			}
		case "palette.clear":
			if f := d.field(); f != nil {
				f.value = nil
			}
		case "palette.delete.word":
			if f := d.field(); f != nil {
				f.value = []rune(strings.TrimRight(string(f.value), " "))
				for len(f.value) > 0 && f.value[len(f.value)-1] != ' ' {
					f.value = f.value[:len(f.value)-1]
				}
			}
		}
	case tea.PasteMsg:
		if f := d.field(); f != nil {
			f.value = append(f.value, []rune(strings.ReplaceAll(strings.TrimRight(m.Content, "\r\n"), "\n", " "))...)
		}
	case tea.KeyPressMsg:
		if m.Text == "" {
			return d, nil
		}
		if f := d.field(); f != nil {
			f.value = append(f.value, []rune(m.Text)...)
			return d, nil
		}
		if m.Text == " " && d.multi {
			d.marks[d.sel] = !d.marks[d.sel]
			return d, nil
		}
		if n, err := strconv.Atoi(m.Text); err == nil && n >= 1 && n <= len(d.choices) {
			d.sel = n - 1
			if d.multi {
				d.marks[d.sel] = !d.marks[d.sel]
				return d, nil
			}
			return d, d.answer()
		}
		if len(d.fields) > 0 {
			d.focus = 0
			d.fields[0].value = append(d.fields[0].value, []rune(m.Text)...)
		}
	case tea.MouseWheelMsg:
		if m.Button == tea.MouseWheelUp {
			d.scroll--
		} else if m.Button == tea.MouseWheelDown {
			d.scroll++
		}
	case tea.MouseClickMsg:
		row := m.Y + d.scroll
		first := d.bodyRows
		if d.bodyRows > 0 {
			first++
		}
		if i := row - first; i >= 0 && i < len(d.choices) {
			d.sel, d.focus = i, -1
			if d.multi {
				d.marks[i] = !d.marks[i]
				return d, nil
			}
			return d, d.answer()
		}
	}
	return d, nil
}

// Dialogs of the session prompts, the one showing and what the bar says while one waits
type dialogState struct {
	current *dialogCard
}

// Kind and request of the prompt that should be showing, by urgency
func (a *app) wantedDialog() (kind, id string) {
	o := a.store.overlay
	switch {
	case o.Approval != nil:
		return "approval", o.Approval.RequestID
	case o.Sudo != nil:
		return "sudo", o.Sudo.RequestID
	case o.Secret != nil:
		return "secret", o.Secret.RequestID
	case o.VaultUnlock != nil:
		return "vaultUnlock", o.VaultUnlock.RequestID
	case o.VaultSaveLogin != nil:
		return "vaultSaveLogin", o.VaultSaveLogin.RequestID
	case o.VaultCode != nil:
		return "vaultCode", o.VaultCode.RequestID
	case o.Connection != nil && a.store.connectionOp != nil && a.store.connectionOp.OpID == o.Connection.OpID:
		op := a.store.connectionOp
		key := op.OpID + ":" + strconv.FormatInt(op.Seq, 10)
		if target := a.activeConnectionTarget(); target != nil {
			key += ":" + target.Name + ":" + string(target.State) + ":" + val(target.ConnectURL)
		}
		return "connection", key
	case o.Clarify != nil:
		return "clarify", o.Clarify.RequestID + ":" + a.activeClarifyQuestion()
	case o.Confirm != nil:
		return "confirm", o.Confirm.Title
	}
	return "", ""
}

// Whether a prompt waits on the user
func (s *dialogState) waiting() bool { return s.current != nil && !s.current.settled }

// Bar notice of the waiting prompt
func (s *dialogState) notice() (string, wm.Signal, bool) {
	if !s.waiting() {
		return "", wm.Idle, false
	}
	return s.current.title, wm.Blocked, true
}

// Declines the waiting prompt the way the interrupt key does
func (s *dialogState) decline(a *app) tea.Cmd {
	if !s.waiting() {
		return nil
	}
	return a.wm.Dismiss()
}

// Opens the dialog of the prompt the store holds and closes one whose prompt settled elsewhere
func (a *app) syncDialogs() tea.Cmd {
	kind, id := a.wantedDialog()
	cur := a.dialogs.current
	if cur != nil && (cur.settled || a.wm.Overlay() != wm.Card(cur)) {
		a.dialogs.current, cur = nil, nil
	}
	if cur != nil && (cur.kind != kind || cur.requestID != id) {
		cur.settled = true
		a.dialogs.current = nil
		if kind == "" {
			return a.wm.Dismiss()
		}
	}
	if kind == "" || a.dialogs.current != nil {
		return nil
	}
	d := a.buildDialog(kind, id)
	if d == nil {
		return nil
	}
	a.dialogs.current = d
	return a.wm.ShowOverlay(d)
}

// First clarify question that has no locked answer yet
func (a *app) activeClarifyQuestion() string {
	c := a.store.overlay.Clarify
	if c == nil {
		return ""
	}
	for _, q := range c.Questions {
		if _, answered := c.Answers[q.QID]; !answered {
			return q.QID
		}
	}
	return ""
}

// Answers a one value server request, the status back to running while a turn runs and to ready otherwise
func (a *app) respondPrompt(key, requestID, value string) tea.Cmd {
	a.store.respondToServerRequest(requestID, tuigateway.ValueAnswer{Value: value})
	a.store.overlay.closePrompt(key)
	a.store.ui.Status = "ready"
	if a.store.ui.Busy {
		a.store.ui.Status = "running…"
	}
	return nil
}

// Dialog of one prompt kind, wired to the answer the gateway expects
func (a *app) buildDialog(kind, id string) *dialogCard {
	o := &a.store.overlay
	d := &dialogCard{app: a, kind: kind, requestID: id, focus: -1, marks: map[int]bool{}}
	masked := func(key, title string, body []string, label string, plain bool) *dialogCard {
		d.title, d.body, d.fields, d.focus = title, body, []dialogField{{label: label, masked: !plain}}, 0
		requestID := id
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			value := fields[0]
			if plain {
				value = trimJS(value)
			}
			return a.respondPrompt(key, requestID, value)
		}
		d.onCancel = func() tea.Cmd { return a.respondPrompt(key, requestID, "") }
		return d
	}
	switch kind {
	case "approval":
		req := o.Approval
		d.title, d.danger = tr("prompt.approval.title"), true
		d.body = []string{req.Description, req.Command}
		if req.SmartDenied {
			d.body = append(d.body, tr("prompt.approval.smartDenied"))
		}
		choices := req.Choices
		if len(choices) == 0 {
			choices = []string{"once", "session", "always", "deny"}
		}
		for _, c := range choices {
			if c == "always" && !req.AllowPermanent {
				continue
			}
			d.choices = append(d.choices, dialogChoice{label: approvalLabel(c), value: c})
		}
		d.onAnswer = func(choice *dialogChoice, _ []string, _ []string) tea.Cmd { return a.answerApproval(req, choice.value) }
		d.onCancel = func() tea.Cmd { return a.answerApproval(req, "deny") }
	case "sudo":
		req := o.Sudo
		body := []string{tr("secure.sudo.body")}
		if req.Command != "" {
			body = []string{req.Command, tr("secure.sudo.body")}
		}
		if req.ProfileKey != "" {
			body = []string{tr("secure.sudo.installBody", req.ProfileKey)}
		}
		return masked("sudo", tr("secure.sudo.title"), body, tr("secure.passwordLabel"), false)
	case "secret":
		req := o.Secret
		return masked("secret", "secret "+tr("secure.secret.forEnvVar", req.EnvVar), []string{req.Prompt}, req.EnvVar, false)
	case "vaultUnlock":
		req := o.VaultUnlock
		name := req.DisplayName
		if name == "" {
			name = req.Backend
		}
		return masked("vaultUnlock", tr("secure.vault.unlockTitle", name), []string{tr("secure.vault.unlockBody", name)}, tr("secure.passwordLabel"), false)
	case "vaultCode":
		req := o.VaultCode
		body := []string{tr("secure.vault.codeBody", req.Site)}
		if req.Hint != "" {
			body = []string{req.Hint, tr("secure.vault.codeBody", req.Site)}
		}
		return masked("vaultCode", tr("secure.vault.codeTitle", req.Site), body, tr("secure.vault.codeLabel"), true)
	case "vaultSaveLogin":
		req := o.VaultSaveLogin
		d.title = tr("secure.vault.saveTitle", req.Site)
		d.body = []string{tr("secure.vault.saveBody", req.Origin)}
		d.fields, d.focus = []dialogField{{label: tr("secure.vault.usernameLabel")}, {label: tr("secure.passwordLabel"), masked: true}}, 0
		requestID := id
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			value := ""
			if fields[0] != "" && fields[1] != "" {
				raw, _ := json.Marshal(map[string]string{"identifier": fields[0], "password": fields[1]})
				value = string(raw)
			}
			return a.respondPrompt("vaultSaveLogin", requestID, value)
		}
		d.onCancel = func() tea.Cmd {
			return a.respondPrompt("vaultSaveLogin", requestID, "")
		}
	case "clarify":
		req := o.Clarify
		qid := a.activeClarifyQuestion()
		var q *clarifyQuestion
		for i := range req.Questions {
			if req.Questions[i].QID == qid {
				q = &req.Questions[i]
			}
		}
		if q == nil {
			return nil
		}
		d.title = tr("prompt.clarify.title")
		if n := len(req.Questions); n > 1 {
			d.title = tr("prompt.clarify.titleOf", len(req.Answers)+1, n)
		}
		d.body = []string{q.Question}
		d.multi = q.MultiSelect
		for _, c := range q.Choices {
			d.choices = append(d.choices, dialogChoice{label: c, value: c})
		}
		d.fields = []dialogField{{label: tr("prompt.clarify.answerLabel")}}
		if len(d.choices) == 0 {
			d.focus = 0
		}
		question := *q
		d.onAnswer = func(choice *dialogChoice, marked []string, fields []string) tea.Cmd {
			answer := trimJS(fields[0])
			switch {
			case question.MultiSelect:
				if answer != "" {
					marked = append(marked, answer)
				}
				raw, _ := json.Marshal(marked)
				answer = string(raw)
				if len(marked) == 0 {
					answer = ""
				}
			case answer == "" && choice != nil:
				answer = choice.value
			}
			return a.answerClarifyQuestion(req, question, answer)
		}
		d.onCancel = func() tea.Cmd { return a.answerClarifyQuestion(req, question, "") }
	case "connection":
		return a.connectionDialog(d)
	case "confirm":
		req := o.Confirm
		d.title, d.danger = req.Title, req.Danger
		if req.Detail != "" {
			d.body = []string{req.Detail}
		}
		yes, no := req.ConfirmLabel, req.CancelLabel
		if yes == "" {
			yes = "yes"
		}
		if no == "" {
			no = "no"
		}
		d.choices = []dialogChoice{{label: no, value: "no"}, {label: yes, value: "yes"}}
		d.onAnswer = func(choice *dialogChoice, _ []string, _ []string) tea.Cmd {
			o.Confirm = nil
			if choice != nil && choice.value == "yes" && req.OnConfirm != nil {
				return req.OnConfirm()
			}
			return nil
		}
		d.onCancel = func() tea.Cmd { o.Confirm = nil; return nil }
	}
	return d
}

// States a connection target no longer waits in
var resolvedTargetStates = map[tuigateway.ConnectionTargetState]bool{"connected": true, "skipped": true, "not_connected": true}

// First target of the live connection operation that still waits on the user, nil once every target resolved
func (a *app) activeConnectionTarget() *tuigateway.ConnectionOperationTarget {
	op := a.store.connectionOp
	if op == nil {
		return nil
	}
	for i := range op.Targets {
		if target := &op.Targets[i]; !resolvedTargetStates[target.State] && !val(target.Resolved) {
			return target
		}
	}
	return nil
}

// Answer of the connection card, one target approved with its credentials or skipped, or the operation continued
func connectionAnswer(target *tuigateway.ConnectionOperationTarget, status tuigateway.ConnectionAnswerStatus, fields []string) tuigateway.ConnectionAnswer {
	if target == nil {
		return tuigateway.ConnectionAnswer{SettledBy: ptr(tuigateway.ConnectionSettleReason("continue"))}
	}
	row := tuigateway.ConnectionAnswerTarget{Name: target.Name, Status: status}
	if status == "approved" && len(target.RequiredEnv) > 0 {
		row.Env = map[string]string{}
		for i, field := range target.RequiredEnv {
			if i < len(fields) && field.Name != "" {
				row.Env[field.Name] = fields[i]
			}
		}
	}
	return tuigateway.ConnectionAnswer{Targets: []tuigateway.ConnectionAnswerTarget{row}}
}

// Owner of the live connection operation, the viewed session
func (a *app) connectionOwner() tuigateway.SessionOwner {
	return tuigateway.SessionOwner{Type: "session", SessionID: a.store.ui.Sid}
}

// Sends an answer of the connection card to the owner of the operation, a lost answer noticed
func (a *app) respondConnection(answer tuigateway.ConnectionAnswer) tea.Cmd {
	op := a.store.connectionOp
	if op == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectionRespondParams{Owner: a.connectionOwner(), OpID: op.OpID, Result: answer}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectionRespond, params, func(r *tuigateway.ConnectionRespondResult) tea.Cmd {
		if r == nil {
			return wm.Notify(tr("connection.notice.answerNotDelivered"), wm.Warn)
		}
		return nil
	})
}

// Runs a failed target of the live operation again, the card following the fresh snapshot
func (a *app) retryConnection(name string) tea.Cmd {
	if a.store.connectionOp == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectorsConnectParams{Owner: a.connectionOwner(), Connectors: []string{name}, Reconnect: true}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectorsConnect, params, func(r *tuigateway.ConnectionOperationSnapshot) tea.Cmd {
		if r == nil {
			return wm.Notify(tr("connection.notice.restartFailed"), wm.Warn)
		}
		for _, line := range a.store.applyConnectionSnapshot(*r) {
			a.sys(line)
		}
		return nil
	})
}

// Asks the watcher of the live operation to read the account now, after the browser step
func (a *app) wakeConnection() tea.Cmd {
	op := a.store.connectionOp
	if op == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectionOperationParams{Owner: a.connectionOwner(), OpID: op.OpID}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectorsOperationWake, params, func(*tuigateway.ConnectionWakeResult) tea.Cmd { return nil })
}

// Verb of the step a target needs
func connectionVerb(action tuigateway.ConnectionTargetAction) string {
	switch action {
	case "authorize":
		return tr("connection.verb.authorize")
	case "enable":
		return tr("connection.verb.enable")
	case "install":
		return tr("connection.verb.install")
	}
	return tr("connection.verb.connect")
}

// Connection card, every target with its state and the step the first waiting one needs
func (a *app) connectionDialog(d *dialogCard) *dialogCard {
	op := a.store.connectionOp
	target := a.activeConnectionTarget()
	trimmed := func(s *string) string { return strings.TrimFunc(val(s), isJSSpace) }
	d.title = tr("connection.title")
	for _, row := range op.Targets {
		mark := "○"
		switch row.State {
		case "connected":
			mark = "●"
		case "skipped", "not_connected":
			mark = "–"
		case "initiated":
			mark = "◌"
		case "failed":
			mark = "✗"
		}
		line := mark + " " + connectionTargetName(row) + " · " + connectionStateWord(row.State)
		if detail := trimmed(row.Detail); detail != "" {
			line += " · " + detail
		}
		d.body = append(d.body, line)
	}
	if target == nil {
		d.choices = []dialogChoice{{label: tr("connection.authorized.continue"), value: "continue"}}
		d.onAnswer = func(*dialogChoice, []string, []string) tea.Cmd {
			return a.respondConnection(connectionAnswer(nil, "", nil))
		}
		d.onCancel = func() tea.Cmd { return a.respondConnection(connectionAnswer(nil, "", nil)) }
		return d
	}
	for _, line := range []string{trimmed(target.Description), trimmed(target.Instructions), trimmed(target.Hint), trimmed(target.DiscoveryError)} {
		if line != "" {
			d.body = append(d.body, line)
		}
	}
	name, link := target.Name, trimmed(target.ConnectURL)
	skip := dialogChoice{label: tr("connection.selector.skip"), value: "skip"}
	switch {
	case target.State == "failed":
		if trimmed(target.Detail) == "" {
			d.body = append(d.body, tr("connection.failure.failed"))
		}
		d.choices = []dialogChoice{{label: tr("connection.selector.tryAgain"), value: "retry"}, skip}
	case link != "":
		d.body = append(d.body, tr("connection.finishInBrowser", link))
		d.choices = []dialogChoice{{label: tr("connection.selector.open"), value: "open"}, {label: tr("connection.selector.checkNow"), value: "wake"}, skip}
	case target.State == "initiated":
		d.body = append(d.body, tr("connection.waiting", connectionTargetName(*target)))
		d.choices = []dialogChoice{{label: tr("connection.selector.checkNow"), value: "wake"}, skip}
	default:
		for _, field := range target.RequiredEnv {
			label := firstText(trimmed(field.Prompt), field.Name)
			d.fields = append(d.fields, dialogField{label: label, masked: field.Secret, value: []rune(field.Default)})
		}
		d.choices = []dialogChoice{{label: connectionVerb(target.Action) + " " + connectionTargetName(*target), value: "approved"}, skip}
	}
	required := target.RequiredEnv
	d.onAnswer = func(choice *dialogChoice, _ []string, fields []string) tea.Cmd {
		value := "approved"
		if choice != nil {
			value = choice.value
		}
		switch value {
		case "open":
			if !openExternalURL(link) {
				return wm.Notify(tr("connection.notice.browserDidNotOpen"), wm.Warn)
			}
			return nil
		case "wake":
			return a.wakeConnection()
		case "retry":
			return a.retryConnection(name)
		case "skip":
			return a.respondConnection(connectionAnswer(target, "skipped", nil))
		}
		for i, field := range required {
			if field.Required && (i >= len(fields) || strings.TrimFunc(fields[i], isJSSpace) == "") {
				return wm.Notify(tr("connection.field.required", firstText(trimmed(field.Prompt), field.Name)), wm.Warn)
			}
		}
		return a.respondConnection(connectionAnswer(target, "approved", fields))
	}
	d.onCancel = func() tea.Cmd { return a.respondConnection(connectionAnswer(target, "skipped", nil)) }
	return d
}

// Label of an approval verdict
func approvalLabel(choice string) string {
	switch choice {
	case "once", "session", "always", "deny":
		return tr("prompt.approval." + choice)
	}
	return choice
}

// Sends the verdict of a dangerous command, through the shared prompt or the server request it came as
func (a *app) answerApproval(req *approvalReq, choice string) tea.Cmd {
	settle := func() {
		if a.store.overlay.Approval == req {
			a.store.overlay.Approval = nil
		}
		a.store.turn.Outcome = tr("session.approval.approved", choice)
		if choice == "deny" {
			a.store.turn.Outcome = tr("session.approval.denied")
		}
		a.store.ui.Status = "running…"
	}
	openLink := func() tea.Cmd {
		if choice == "deny" || req.OpenURL == "" || openExternalURL(req.OpenURL) {
			return nil
		}
		a.sys(req.OpenURL)
		return wm.Notify(tr("mcp.notice.browserDidNotOpen"), wm.Warn)
	}
	if req.Shared != nil {
		params := tuigateway.CanonicalApprovalParams{SessionID: req.Shared.SessionID, ExecutionGeneration: req.Shared.ExecutionGeneration, PromptID: req.Shared.PromptID, Choice: tuigateway.ApprovalChoice(choice)}
		return ask(a.events.rpc, a.sys, tuigateway.CanonicalApprovalRespond, params, func(r *tuigateway.PromptResponseResult) tea.Cmd {
			if r != nil {
				settle()
				return openLink()
			}
			return nil
		})
	}
	a.store.respondToServerRequest(req.RequestID, tuigateway.ApprovalAnswer{Choice: tuigateway.ApprovalChoice(choice)})
	settle()
	return openLink()
}

// Settles a clarify batch in the transcript, one trail row and the questions with their answers
func (a *app) settleClarify(req *clarifyReq, answers map[string]string) {
	label := toolTrailLabel("clarify")
	kept := a.turn.turnTools[:0]
	for _, line := range a.turn.turnTools {
		if !sameToolTrailGroup(label, line) {
			kept = append(kept, line)
		}
	}
	a.turn.turnTools = kept
	a.store.turn.TurnTrail = append([]string(nil), kept...)
	a.turn.persistedToolLabels[label] = true
	rows := make([]string, len(req.Questions))
	for i, q := range req.Questions {
		answer := tr("session.main.skipped")
		if trimJS(answers[q.QID]) != "" {
			answer = clarifyAnswerText(answers[q.QID], q.MultiSelect)
		}
		rows[i] = q.Question + " → " + answer
	}
	a.appendMessage(msg{Kind: "trail", Role: "system", Tools: []string{buildToolTrailLine("clarify", countNoun(len(req.Questions), "question"), toolVerdict{}, "", nil)}})
	a.appendMessage(msg{Role: "user", Text: strings.Join(rows, "\n")})
	a.store.ui.Status = "running…"
	if a.store.overlay.Clarify == req {
		a.store.overlay.Clarify = nil
	}
}

// Answers one clarify question, a single shared prompt at once and a batch by locking each answer
func (a *app) answerClarifyQuestion(req *clarifyReq, q clarifyQuestion, answer string) tea.Cmd {
	if req.Shared != nil && len(req.Questions) == 1 {
		params := tuigateway.CanonicalClarifyParams{SessionID: req.Shared.SessionID, ExecutionGeneration: req.Shared.ExecutionGeneration, PromptID: req.Shared.PromptID, Answer: answer}
		return ask(a.events.rpc, a.sys, tuigateway.CanonicalClarifyRespond, params, func(r *tuigateway.PromptResponseResult) tea.Cmd {
			if r != nil && r.Status == "resolved" {
				a.settleClarify(req, map[string]string{q.QID: answer})
			}
			return nil
		})
	}
	params := tuigateway.ClarifyLockParams{RequestID: req.RequestID, QuestionID: q.QID}
	if trimJS(answer) != "" {
		params.Answer = &answer
	}
	return ask(a.events.rpc, a.sys, tuigateway.ClarifyLock, params, func(r *tuigateway.ClarifyLockResult) tea.Cmd {
		if r == nil || a.store.overlay.Clarify != req {
			return nil
		}
		if r.Status == "expired" {
			a.store.overlay.Clarify = nil
			return nil
		}
		if req.Answers == nil {
			req.Answers = map[string]string{}
		}
		req.Answers[q.QID] = answer
		if len(r.Remaining) > 0 {
			return nil
		}
		a.settleClarify(req, req.Answers)
		return nil
	})
}

// Scrollable text over the session, for logs, history and long command output
type pagerCard struct {
	app    *app
	title  string
	lines  []string
	offset int
	height int
	width  int
	text   string
}

// Pager over text, opened at its start
func (a *app) page(text, title string) tea.Cmd {
	return a.wm.ShowOverlay(&pagerCard{app: a, title: title, text: strings.TrimRight(stripAnsi(text), "\n")})
}

func (p *pagerCard) Title() string       { return p.title }
func (p *pagerCard) Key() string         { return "pager" }
func (p *pagerCard) Init() tea.Cmd       { return nil }
func (p *pagerCard) Selectable() bool    { return true }
func (p *pagerCard) Contexts() []string  { return []string{keys.Transcript} }
func (p *pagerCard) MinSize() (int, int) { w, h := p.app.wm.Size(); return w * 8 / 10, h * 6 / 10 }

func (p *pagerCard) Status() string {
	if len(p.lines) <= p.height {
		return countNoun(len(p.lines), "line")
	}
	return fmt.Sprintf("%d-%d of %d", p.offset+1, min(p.offset+p.height, len(p.lines)), len(p.lines))
}

func (p *pagerCard) Hints() []wm.Hint {
	return []wm.Hint{{Action: "transcript.down", Pair: "transcript.up"}, {Action: "transcript.page.down"}, {Action: "transcript.top"}, {Action: "transcript.bottom"}, {Action: "palette.close", Label: "close"}}
}

func (p *pagerCard) View(w, h int) string {
	if w != p.width {
		p.width = w
		p.lines = strings.Split(ansi.Wrap(p.text, max(w-2, 8), ""), "\n")
	}
	p.height = h
	p.offset = min(max(p.offset, 0), max(len(p.lines)-h, 0))
	tone := toneStyle(p.app.store.ui.Theme.Color.Text)
	rows := make([]string, 0, h)
	for i := p.offset; i < min(p.offset+h, len(p.lines)); i++ {
		rows = append(rows, " "+tone.Render(p.lines[i]))
	}
	return strings.Join(rows, "\n")
}

func (p *pagerCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.ActionMsg:
		switch m.ID {
		case "transcript.down", "palette.down":
			p.offset++
		case "transcript.up", "palette.up":
			p.offset--
		case "transcript.page.down", "palette.page.down":
			p.offset += max(p.height-1, 1)
		case "transcript.page.up", "palette.page.up":
			p.offset -= max(p.height-1, 1)
		case "transcript.top":
			p.offset = 0
		case "transcript.bottom":
			p.offset = len(p.lines)
		case "palette.pick":
			return p, wm.RequestDismissOf(p)
		}
	case tea.KeyPressMsg:
		switch m.Text {
		case "q":
			return p, wm.RequestDismissOf(p)
		case "b":
			p.offset -= max(p.height-1, 1)
		}
	case tea.MouseWheelMsg:
		if m.Button == tea.MouseWheelUp {
			p.offset -= 3
		} else if m.Button == tea.MouseWheelDown {
			p.offset += 3
		}
	}
	return p, nil
}
