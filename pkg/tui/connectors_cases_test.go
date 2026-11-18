package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Account row of the reference cases, gmail and active unless the case says otherwise
func accountRow(over map[string]any) map[string]any {
	row := map[string]any{"active": true, "alias": nil, "connection_id": "ca_1", "connector": "gmail", "created_at": "2026-10-08T00:00:00Z",
		"label": "gmail-x1", "status": "active", "updated_at": "2026-10-08T00:00:00Z", "disabled": false}
	for key, value := range over {
		row[key] = value
	}
	return row
}

// Accounts of the reference cases, two named, one retired and one unnamed
func referenceAccounts() []connectorAccount {
	return connectorAccountsOf(map[string]any{"accounts": []any{
		accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"}),
		accountRow(map[string]any{"alias": "home", "connection_id": "ca_h"}),
		accountRow(map[string]any{"connection_id": "ca_old", "disabled": true, "status": "inactive"}),
		accountRow(map[string]any{"connection_id": "ca_n", "label": "sid@example.com"}),
	}})
}

// connectorAccounts.test.ts folds retired accounts and names unnamed ones by their label
func TestConnectorAccountViewFoldsRetiredAccountsAndNamesUnnamedOnesByTheirLabel(t *testing.T) {
	rows := referenceAccounts()
	folded, retired := connectorAccountView(rows, false)
	var ids []string
	for _, row := range folded {
		ids = append(ids, row.ID)
	}
	if retired != 1 || strings.Join(ids, ",") != "ca_h,ca_n,ca_w" {
		t.Fatalf("folded %v retired %d", ids, retired)
	}
	if shown, _ := connectorAccountView(rows, true); shown[len(shown)-1].ID != "ca_old" {
		t.Fatalf("shown %v", shown)
	}
	if cell := connectorNameCell(folded[1]); cell != "sid@example.com (unnamed)" || connectorStatusLabel(folded[1]) != "active" {
		t.Fatalf("unnamed row %q %q", cell, connectorStatusLabel(folded[1]))
	}
}

// connectorAccounts.test.ts refuses a malformed name and a name another live account of the same app holds
func TestConnectorNameProblemRefusesAMalformedNameAndANameAnotherLiveAccountHolds(t *testing.T) {
	rows := referenceAccounts()
	for _, c := range []struct{ name, connector, self, want string }{
		{"Work", "gmail", "", "invalid"},
		{"-x", "gmail", "", "invalid"},
		{"home", "gmail", "", "taken"},
		{"home", "gmail", "ca_h", ""},
		{"home", "slack", "", ""},
	} {
		if got := connectorNameProblem(c.name, c.connector, rows, c.self); got != c.want {
			t.Errorf("%s of %s answered %q", c.name, c.connector, got)
		}
	}
}

// connectorAccounts.test.ts skips a healthy account and a retired one, and offers every other status
func TestReconnectIsOfferedOnlyAsARepair(t *testing.T) {
	account := func(over map[string]any) connectorAccount {
		return connectorAccountsOf(map[string]any{"accounts": []any{accountRow(over)}})[0]
	}
	if account(map[string]any{"status": "active"}).needsRepair() || account(map[string]any{"disabled": true, "status": "inactive"}).needsRepair() {
		t.Fatal("a healthy or retired account offered a repair")
	}
	for _, status := range []string{"failed", "expired", "revoked", "inactive"} {
		row := account(map[string]any{"active": false, "status": status})
		if !row.needsRepair() || row.reconnectRefusal() != "" {
			t.Errorf("%s account refused a repair", status)
		}
	}
	if account(map[string]any{"status": "active"}).reconnectRefusal() != "healthy" || account(map[string]any{"disabled": true, "status": "inactive"}).reconnectRefusal() != "retired" {
		t.Fatal("refusals")
	}
	p := &connectorsPanel{shown: []connectorAccount{account(map[string]any{"status": "active"})}}
	offers := func(i int) bool {
		for _, hint := range p.hints(nil, i) {
			if hint.Action == "tui.connectors.reconnect" {
				return true
			}
		}
		return false
	}
	if offers(0) || !offers(-1) {
		t.Fatal("the legend offered a reconnect for a healthy account or hid it with no account selected")
	}
}

// Connectors backend of the panel cases, the accounts it holds and when its operations settle
type panelBackend struct {
	mu       sync.Mutex
	accounts []any
	renameTo func(params map[string]any) map[string]any
	settle   bool
	signedIn bool
}

// Marks the browser leg of the open operation done
func (b *panelBackend) signIn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signedIn = true
}

func (b *panelBackend) list(map[string]any) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"accounts": append([]any(nil), b.accounts...)}
}

// Scripted gateway of the panel over the backend
func attachPanel(t *testing.T, a *app, b *panelBackend) *scriptedGateway {
	t.Helper()
	snapshot := func(state string, settled bool) map[string]any {
		return map[string]any{"op_id": "op_acc", "seq": float64(1), "deadline_at": float64(time.Now().Add(time.Hour).Unix()), "settled": settled,
			"targets": []any{map[string]any{"action": "connect", "kind": "connector", "name": "gmail", "alias": "office", "state": state,
				"connect_url": "https://connect.example/gmail"}}}
	}
	return attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"connectors.accounts": b.list,
		"connectors.accounts.rename": func(params map[string]any) map[string]any {
			if b.renameTo != nil {
				return b.renameTo(params)
			}
			return accountRow(map[string]any{"connection_id": params["connection_id"], "alias": params["alias"]})
		},
		"connectors.accounts.remove": func(params map[string]any) map[string]any {
			b.mu.Lock()
			defer b.mu.Unlock()
			kept := b.accounts[:0]
			for _, raw := range b.accounts {
				if raw.(map[string]any)["connection_id"] != params["connection_id"] {
					kept = append(kept, raw)
				}
			}
			b.accounts = kept
			return map[string]any{"connection_id": params["connection_id"], "connector": "gmail", "status": "removed"}
		},
		"connectors.list": fixed(map[string]any{"available": true, "connectors": []any{
			map[string]any{"connector": "notion", "enabled": true, "connected": false},
			map[string]any{"connector": "gmail", "enabled": true, "connected": true}}}),
		"connectors.connect": func(map[string]any) map[string]any {
			if b.settle {
				return snapshot("connected", true)
			}
			return snapshot("initiated", false)
		},
		"connectors.operation.status": func(map[string]any) map[string]any {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.signedIn {
				return snapshot("connected", true)
			}
			return snapshot("initiated", false)
		},
	})
}

// Presses a key and runs what it answers
func press(a *app, chord string) {
	_, cmd := a.Update(keys.Press(chord))
	drain(a, cmd)
}

// Pastes text into whatever field takes typing
func typeText(a *app, text string) {
	_, cmd := a.Update(tea.PasteMsg{Content: text})
	drain(a, cmd)
}

// Panel over the session, failing the case when another overlay is up
func openPanel(t *testing.T, a *app) *connectorsPanel {
	t.Helper()
	drain(a, a.dispatchSubmission("/connectors"))
	p, isPanel := a.wm.Overlay().(*connectorsPanel)
	if !isPanel {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	return p
}

// Plain text of the panel as it paints
func panelText(p *connectorsPanel) string { return ansi.Strip(p.View(100, 16)) }

// Fails unless a toast holds the text
func wantToast(t *testing.T, a *app, want string) {
	t.Helper()
	for _, toast := range a.wm.Toasts() {
		if strings.Contains(ansi.Strip(toast), want) {
			return
		}
	}
	t.Fatalf("no toast holds %q in %v", want, a.wm.Toasts())
}

func TestConnectorsPanelListsTheAccountsAndFoldsTheRetiredOnes(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"}),
		accountRow(map[string]any{"connection_id": "ca_old", "disabled": true, "status": "inactive", "label": "old-login"})}}
	attachPanel(t, a, b)
	p := openPanel(t, a)
	view := panelText(p)
	if !strings.Contains(view, "gmail") || !strings.Contains(view, "work") || strings.Contains(view, "old-login") {
		t.Fatalf("panel:\n%s", view)
	}
	if p.Status() != "1 retired (Tab to show)" {
		t.Fatalf("status %q", p.Status())
	}
	press(a, "tab")
	if view := panelText(p); !strings.Contains(view, "old-login (unnamed)") || !strings.Contains(view, "retired") || p.Status() != "1 retired shown (Tab to hide)" {
		t.Fatalf("panel with the retired shown %q:\n%s", p.Status(), view)
	}
}

func TestConnectorsPanelRenamesTheSelectedAccountInPlace(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"})}}
	g := attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "r")
	if !p.Editing() {
		t.Fatal("rename did not open the name cell")
	}
	typeText(a, "Office")
	press(a, "enter")
	if sent := g.sent("connectors.accounts.rename"); len(sent) != 0 {
		t.Fatalf("an invalid name reached the gateway %v", sent)
	}
	wantToast(t, a, "Use 1-32 lowercase letters")
	if !p.Editing() {
		t.Fatal("the refused name closed the name cell")
	}
	press(a, "ctrl+u")
	typeText(a, "office")
	b.mu.Lock()
	b.accounts = []any{accountRow(map[string]any{"alias": "office", "connection_id": "ca_w"})}
	b.mu.Unlock()
	press(a, "enter")
	sent := g.sent("connectors.accounts.rename")
	if len(sent) != 1 || sent[0]["alias"] != "office" || sent[0]["connection_id"] != "ca_w" {
		t.Fatalf("rename %v", sent)
	}
	wantToast(t, a, "Renamed to office (gmail).")
	if view := panelText(p); !strings.Contains(view, "office") {
		t.Fatalf("panel after the rename:\n%s", view)
	}
}

func TestARenameToANameAnotherAccountHoldsSaysTheNameIsTaken(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"})},
		renameTo: func(map[string]any) map[string]any {
			return map[string]any{"rpc_error": map[string]any{"code": 4090, "message": "That name is already used.", "data": map[string]any{"reason": "ALIAS_TAKEN"}}}
		}}
	attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "r")
	typeText(a, "home")
	press(a, "enter")
	wantToast(t, a, "error: That name is already used.")
	if !p.Editing() {
		t.Fatal("a refused rename closed the name cell")
	}
}

func TestConnectorsPanelRemovesAnAccountAfterTheConfirmation(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"})}}
	g := attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "x")
	if view := panelText(p); !strings.Contains(view, "Remove gmail account work? This signs Icebath out of it.") {
		t.Fatalf("no confirmation:\n%s", view)
	}
	if len(g.sent("connectors.accounts.remove")) != 0 {
		t.Fatal("removed before the confirmation")
	}
	press(a, "y")
	if sent := g.sent("connectors.accounts.remove"); len(sent) != 1 || sent[0]["connection_id"] != "ca_w" {
		t.Fatalf("remove %v", sent)
	}
	wantToast(t, a, "Removed gmail account work.")
	if view := panelText(p); !strings.Contains(view, "No connector accounts yet") {
		t.Fatalf("panel after the remove:\n%s", view)
	}
}

// Polls of the open link every few milliseconds for the length of a case
func fastPolls(t *testing.T) {
	saved := connectorsPollEvery
	connectorsPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { connectorsPollEvery = saved })
}

func TestConnectorsPanelAddsANamedAccountAndWaitsOnItsLink(t *testing.T) {
	fastPolls(t)
	a := testApp(t)
	b := &panelBackend{}
	g := attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "a")
	picker, isPalette := a.wm.Overlay().(*wm.Palette)
	if !isPalette || len(picker.Matches()) != 2 || picker.Matches()[0].Title != "gmail" {
		t.Fatalf("app picker %T", a.wm.Overlay())
	}
	if sent := g.sent("connectors.list"); len(sent) != 1 || sent[0]["owner"].(map[string]any)["type"] != "account" {
		t.Fatalf("list %v", sent)
	}
	press(a, "enter")
	prompt, isPrompt := a.wm.Overlay().(*wm.Palette)
	if !isPrompt || prompt == picker || prompt.Placeholder != "Name the new gmail account" {
		t.Fatalf("name prompt %T", a.wm.Overlay())
	}
	typeText(a, "office")
	press(a, "enter")
	connects := g.sent("connectors.connect")
	if len(connects) != 1 || connects[0]["alias"] != "office" || connects[0]["reconnect"] != false || connects[0]["owner"].(map[string]any)["type"] != "account" {
		t.Fatalf("connect %v", connects)
	}
	link, isLink := a.wm.Overlay().(*connectorLinkCard)
	if !isLink {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := ansi.Strip(link.View(90, 8))
	for _, want := range []string{"Connect gmail account office", "https://connect.example/gmail", "Waiting for you to finish in the browser"} {
		if !strings.Contains(view, want) {
			t.Fatalf("link card lacks %q:\n%s", want, view)
		}
	}
	b.mu.Lock()
	b.accounts = []any{accountRow(map[string]any{"alias": "office", "connection_id": "ca_o"})}
	b.mu.Unlock()
	b.signIn()
	drain(a, link.poll())
	if polls := g.sent("connectors.operation.status"); len(polls) == 0 || polls[0]["op_id"] != "op_acc" || polls[0]["owner"].(map[string]any)["type"] != "account" {
		t.Fatalf("polls %v", polls)
	}
	if a.wm.Overlay() != p {
		t.Fatalf("overlay after the settle %T", a.wm.Overlay())
	}
	wantToast(t, a, "gmail account office is connected.")
	if view := panelText(p); !strings.Contains(view, "office") {
		t.Fatalf("panel after the connect:\n%s", view)
	}
}

func TestAnAddNameAnotherAccountHoldsIsRefusedBeforeTheConnect(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w"})}}
	g := attachPanel(t, a, b)
	openPanel(t, a)
	press(a, "a")
	press(a, "enter")
	typeText(a, "work")
	press(a, "enter")
	if len(g.sent("connectors.connect")) != 0 {
		t.Fatal("a taken name reached the gateway")
	}
	wantToast(t, a, "That name is already used.")
	if prompt, isPrompt := a.wm.Overlay().(*wm.Palette); !isPrompt || prompt.Query() != "work" {
		t.Fatalf("the name prompt was not kept with the draft: %T", a.wm.Overlay())
	}
}

func TestStoppingTheLinkWaitReturnsToThePanel(t *testing.T) {
	fastPolls(t)
	a := testApp(t)
	b := &panelBackend{}
	attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "a")
	press(a, "enter")
	typeText(a, "office")
	press(a, "enter")
	link, isLink := a.wm.Overlay().(*connectorLinkCard)
	if !isLink {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	press(a, "esc")
	if a.wm.Overlay() != p || !link.done {
		t.Fatalf("overlay %T done %v", a.wm.Overlay(), link.done)
	}
	wantToast(t, a, "Stopped waiting. The link still works until it expires.")
	if link.poll() != nil {
		t.Fatal("a stopped wait still reads the operation")
	}
}

func TestConnectorsPanelReconnectRepairsOnlyAStoppedAccount(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w", "status": "expired", "active": false}),
		accountRow(map[string]any{"alias": "zeta", "connection_id": "ca_z"})}, settle: true}
	g := attachPanel(t, a, b)
	p := openPanel(t, a)
	press(a, "c")
	connects := g.sent("connectors.connect")
	if len(connects) != 1 || connects[0]["connection_id"] != "ca_w" || connects[0]["reconnect"] != true || connects[0]["alias"] != nil {
		t.Fatalf("reconnect %v", connects)
	}
	if a.wm.Overlay() != p {
		t.Fatalf("a settled reconnect left %T over the panel", a.wm.Overlay())
	}
	wantToast(t, a, "gmail account work is connected.")
	press(a, "down")
	press(a, "c")
	if len(g.sent("connectors.connect")) != 1 {
		t.Fatal("a healthy account was signed in again")
	}
	wantToast(t, a, "gmail account zeta is connected.")
}

// connectorAccounts.test.ts maps a bare letter to its action and never a Ctrl or Meta chord
func TestCtrlCOnThePanelNeverStartsAReconnect(t *testing.T) {
	a := testApp(t)
	b := &panelBackend{accounts: []any{accountRow(map[string]any{"alias": "work", "connection_id": "ca_w", "status": "failed", "active": false})}}
	g := attachPanel(t, a, b)
	openPanel(t, a)
	press(a, "ctrl+c")
	press(a, "alt+x")
	if len(g.sent("connectors.connect")) != 0 || len(g.sent("connectors.accounts.remove")) != 0 {
		t.Fatal("a chord ran a panel action")
	}
}

// connectorAccounts.test.ts counts an agent question or a confirm as open, and the accounts panel alone as not
func TestAPromptTakesTheKeyboardFromThePanel(t *testing.T) {
	a := testApp(t)
	attachPanel(t, a, &panelBackend{})
	openPanel(t, a)
	if a.dialogs.waiting() {
		t.Fatal("the panel alone counts as a prompt")
	}
	a.store.overlay.Clarify = &clarifyReq{RequestID: "r1", Questions: []clarifyQuestion{{QID: "q1", Question: "Which?"}}}
	a.Update(struct{}{})
	if _, isDialog := a.wm.Overlay().(*dialogCard); !isDialog || !a.dialogs.waiting() {
		t.Fatalf("overlay %T waiting %v", a.wm.Overlay(), a.dialogs.waiting())
	}
}

// connectorAccounts.test.ts tell the user the name the agent chose once an aliased account connects
func TestSettledConnectionLinesNameTheAccountTheAgentChose(t *testing.T) {
	a := testApp(t)
	target := map[string]any{"action": "connect", "alias": "work", "kind": "connector", "name": "gmail", "state": "initiated"}
	a.store.applyConnectionRequest(map[string]any{"deadline_at": float64(0), "op_id": "op", "seq": float64(1), "targets": []any{target}})
	connected := map[string]any{"action": "connect", "alias": "work", "kind": "connector", "name": "gmail", "state": "connected"}
	lines := a.store.applyConnectionUpdate(map[string]any{"deadline_at": float64(0), "op_id": "op", "seq": float64(2), "settled": true, "targets": []any{connected}})
	if strings.Join(lines, "\n") != "gmail (work): connected\nIcebath named this gmail account work. Rename it anytime." {
		t.Fatalf("lines %q", lines)
	}
}
