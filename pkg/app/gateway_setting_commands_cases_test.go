package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Owner answering every slash.exec with one exec reply and recording each call with its deadline
type settingOwner struct {
	mu      sync.Mutex
	output  string
	calls   []settingCall
	events  *gateway.EventQueue
	seen    func(call settingCall)
	replies map[string]any
}

type settingCall struct {
	method  string
	params  map[string]any
	timeout time.Duration
}

func (o *settingOwner) RPCTimeout(_ context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, settingCall{method: method, params: params, timeout: timeout})
	if o.seen != nil {
		o.seen(o.calls[len(o.calls)-1])
	}
	if o.replies != nil {
		if reply, ok := o.replies[method]; ok {
			if refused, isErr := reply.(error); isErr {
				return nil, refused
			}
			return reply, nil
		}
		return map[string]any{}, nil
	}
	return map[string]any{"type": "exec", "output": o.output}, nil
}

func (o *settingOwner) EventQueue() *gateway.EventQueue { return o.events }
func (o *settingOwner) SendFrame(string) error          { return nil }
func (o *settingOwner) Close() error                    { return nil }

// Recorded calls as method and params pairs
func (o *settingOwner) pairs() [][2]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([][2]any, len(o.calls))
	for i, call := range o.calls {
		out[i] = [2]any{call.method, call.params}
	}
	return out
}

// CLI of session local-s whose gateway session is open on an owner replying with output
func settingCLI(t *testing.T, output string) (*CLI, *settingOwner) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	owner := &settingOwner{output: output, events: gateway.NewEventQueue(0)}
	connection := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return owner, nil })
	if err := connection.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-s", gatewaycli.NewGatewaySessionStore(connection), true
	return c, owner
}

func TestBusyRunsOnTheGatewayAndMirrorsTheMode(t *testing.T) {
	recordPrints(t)
	c, owner := settingCLI(t, "Busy input mode: queue")
	c.busyInputMode = "interrupt"
	if !runGatewaySetting(c, "busy", "/busy queue") {
		t.Fatal("/busy did not run on the gateway")
	}
	want := [][2]any{{"slash.exec", map[string]any{"session_id": "local-s", "command": "busy queue"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if c.busyInputMode != "queue" {
		t.Fatalf("busy mode %q", c.busyInputMode)
	}
	if runGatewaySetting(c, "history", "/history") {
		t.Fatal("a local read ran on the gateway")
	}
}

func TestRefineAndReviewRunOnTheGatewaySessionsAgent(t *testing.T) {
	rec := recordPrints(t)
	c, owner := settingCLI(t, "Busy input mode: queue")
	if !runGatewaySetting(c, "refine", "/refine naming") || !runGatewaySetting(c, "review", "/review the diff") {
		t.Fatal("/refine or /review did not run on the gateway")
	}
	want := [][2]any{{"slash.exec", map[string]any{"session_id": "local-s", "command": "refine naming"}},
		{"slash.exec", map[string]any{"session_id": "local-s", "command": "review the diff"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if !strings.Contains(strings.Join(rec.plain(), "\n"), "Busy input mode: queue") {
		t.Fatalf("the owner's reply was not printed %v", rec.plain())
	}
}

func TestCronReadsKeepTheAnswerDeadline(t *testing.T) {
	recordPrints(t)
	c, owner := settingCLI(t, "Busy input mode: queue")
	for _, typed := range []string{"/cron", "/cron list", "/cron LIST -a", `/cron add "every 2h`} {
		if !runGatewaySetting(c, "cron", typed) {
			t.Fatalf("%s did not run on the gateway", typed)
		}
	}
	var commands []string
	for _, call := range owner.calls {
		if call.timeout < 0 {
			t.Fatalf("a /cron read must keep the answer deadline: %v", call.params)
		}
		commands = append(commands, call.params["command"].(string))
	}
	if want := []string{"cron", "cron list", "cron LIST -a", `cron add "every 2h`}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands %v", commands)
	}
}

func TestToolsetsIsTheGatewaySessionsReportPrintedAsIs(t *testing.T) {
	out, _ := captureStreams(t)
	report := "+----+\n|  (^_^)b Available Toolsets  |\n\n  (*) mcp-zeta           [11 tools] - MCP server zeta"
	c, owner := settingCLI(t, report)
	if !runGatewaySetting(c, "toolsets", "/toolsets") {
		t.Fatal("/toolsets did not run on the gateway")
	}
	want := [][2]any{{"slash.exec", map[string]any{"session_id": "local-s", "command": "toolsets"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if !strings.Contains(out.String(), "\n  (*) mcp-zeta           [11 tools]") {
		t.Fatalf("report re-indented %q", out.String())
	}
}

func TestCronWritesRunOnTheGatewayNotInTheCli(t *testing.T) {
	rec := recordPrints(t)
	c, owner := settingCLI(t, "(^_^)b Paused job: probe (probe)")
	var woken []bool
	owner.seen = func(call settingCall) {
		c.uiMu.Lock()
		wake := c.gatewayCommandWait
		c.uiMu.Unlock()
		if call.timeout >= 0 || wake == nil {
			t.Errorf("a /cron write waited with a deadline or no wait handle: %v %v", call.timeout, wake)
			return
		}
		woken = append(woken, wake.IsSet())
	}
	if !runGatewaySetting(c, "cron", "/cron pause probe") {
		t.Fatal("/cron pause did not run on the gateway")
	}
	want := [][2]any{{"slash.exec", map[string]any{"session_id": "local-s", "command": "cron pause probe"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(woken, []bool{false}) {
		t.Fatalf("calls %v woken %v", got, woken)
	}
	c.uiMu.Lock()
	wait := c.gatewayCommandWait
	c.uiMu.Unlock()
	if !strings.Contains(strings.Join(rec.plain(), "\n"), "Paused job: probe") || wait != nil {
		t.Fatalf("printed %v wait %v", rec.plain(), wait)
	}
}

// Command observe notice a command the CLI runs itself sends first
func observedCall(name, arg string) [2]any {
	return [2]any{"command.observe", map[string]any{"session_id": "local-s", "name": name, "arg": arg}}
}

// CLI driven through processCommand whose gateway answers each method from replies
func gatewayCommandCLI(t *testing.T, replies map[string]any) (*CLI, *settingOwner, func() string) {
	t.Helper()
	out, _ := captureStreams(t)
	c, owner := settingCLI(t, "")
	owner.replies = replies
	read := func() string {
		text := out.String()
		out.Reset()
		return text
	}
	return c, owner, read
}

func TestDiffSessionRendersTheGatewaySessionsCheckpointDiff(t *testing.T) {
	lines := make([]string, 450)
	for i := range lines {
		lines[i] = fmt.Sprintf("+edited %d", i)
	}
	c, owner, read := gatewayCommandCLI(t, map[string]any{"rollback.session_diff": map[string]any{
		"enabled": true, "success": true, "stat": " a.py | 450 +", "diff": strings.Join(lines, "\n"), "diff_lines": 450}})
	if !c.processCommand("/diff session") {
		t.Fatal("/diff session ended the CLI")
	}
	want := [][2]any{observedCall("diff", "session"), {"rollback.session_diff", map[string]any{"session_id": "local-s"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	out := read()
	if !strings.Contains(out, "a.py | 450 +") || !strings.Contains(out, "+edited 399") || strings.Contains(out, "+edited 400") {
		t.Fatalf("diff body %q", out)
	}
	if !strings.Contains(out, "50 more lines") || strings.Contains(out, "```") {
		t.Fatalf("diff tail %q", out)
	}
	c.processCommand("/diff session --stat")
	if out := read(); !strings.Contains(out, "a.py | 450 +") || strings.Contains(out, "+edited 0") {
		t.Fatalf("stat only %q", out)
	}
}

func TestDiffSessionCountsLinesTheGatewayCutToFitItsFrame(t *testing.T) {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("+edited %d", i)
	}
	c, _, read := gatewayCommandCLI(t, map[string]any{"rollback.session_diff": map[string]any{
		"enabled": true, "success": true, "stat": " a.py | 5000 +", "diff": strings.Join(lines, "\n"), "diff_lines": 5000}})
	c.processCommand("/diff session")
	if out := read(); !strings.Contains(out, "+edited 99") || !strings.Contains(out, "4900 more lines") {
		t.Fatalf("cut body %q", out)
	}
	c, _, read = gatewayCommandCLI(t, map[string]any{"rollback.session_diff": map[string]any{
		"enabled": true, "success": true, "stat": " a.py | 1 +", "diff": "", "diff_lines": 3}})
	c.processCommand("/diff session")
	if out := read(); !strings.Contains(out, "3 more lines") {
		t.Fatalf("empty cut body %q", out)
	}
}

func TestDiffSessionReportsASessionWithoutCheckpoints(t *testing.T) {
	c, _, read := gatewayCommandCLI(t, map[string]any{"rollback.session_diff": map[string]any{"enabled": false}})
	if !c.processCommand("/diff session") {
		t.Fatal("/diff session ended the CLI")
	}
	if out := read(); !strings.Contains(out, "Checkpoints are not enabled") {
		t.Fatalf("output %q", out)
	}
}

func TestReloadRereadsEnvInTheGatewayAndReportsItsCount(t *testing.T) {
	c, owner, read := gatewayCommandCLI(t, map[string]any{"reload.env": map[string]any{"updated": 3}})
	if !c.processCommand("/reload") {
		t.Fatal("/reload ended the CLI")
	}
	want := [][2]any{observedCall("reload", ""), {"reload.env", map[string]any{"session_id": "local-s"}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if out := read(); !strings.Contains(out, "3") {
		t.Fatalf("output %q", out)
	}
}

// Owner whose slash.exec is cut by a SIGINT while it runs, answering only once the call gives up
type interruptedOwner struct {
	settingOwner
	interrupt func()
}

func (o *interruptedOwner) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	o.interrupt()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCtrlCDuringACronWaitPrintsTheStoppedLineAndPropagates(t *testing.T) {
	rec := recordPrints(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := bareCLI()
	owner := &interruptedOwner{settingOwner: settingOwner{events: gateway.NewEventQueue(0)}, interrupt: c.turnCancel}
	connection := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return owner, nil })
	if err := connection.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-s", gatewaycli.NewGatewaySessionStore(connection), true
	var raised any
	func() {
		defer func() { raised = recover() }()
		runGatewaySetting(c, "cron", "/cron run probe")
	}()
	if err, ok := raised.(error); !ok || !errors.Is(err, tools.ErrKeyboardInterrupt) {
		t.Fatalf("the interrupt did not reach the CLI: %v", raised)
	}
	c.uiMu.Lock()
	wait := c.gatewayCommandWait
	c.uiMu.Unlock()
	if !strings.Contains(strings.Join(rec.plain(), "\n"), "/cron: stopped waiting") || wait != nil {
		t.Fatalf("printed %v wait %v", rec.plain(), wait)
	}
}

func TestDiffOfTheWorkingTreeStaysALocalGitRead(t *testing.T) {
	c, owner, read := gatewayCommandCLI(t, nil)
	repo := t.TempDir()
	bannerGit(t, repo, "init", "-q")
	bannerGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bannerGit(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "loose.txt"), []byte("loose line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_CWD", repo)
	c.processCommand("/diff staged")
	out := read()
	if !strings.Contains(out, "staged.txt") || strings.Contains(out, "loose.txt") {
		t.Fatalf("the staged diff was not read locally: %q", out)
	}
	if got := owner.pairs(); !reflect.DeepEqual(got, [][2]any{observedCall("diff", "staged")}) {
		t.Fatalf("calls %v", got)
	}
}

func TestHandoffIsRefusedWithAMessageOnTheGateway(t *testing.T) {
	c, owner, read := gatewayCommandCLI(t, nil)
	if !c.processCommand("/handoff telegram") {
		t.Fatal("/handoff ended the CLI")
	}
	if out := read(); !strings.Contains(out, "/handoff is not available on the shared gateway yet") {
		t.Fatalf("output %q", out)
	}
	if got := owner.pairs(); len(got) != 0 {
		t.Fatalf("a named refusal ran %v", got)
	}
}

// Gateway backed CLI with one said line in its mirror, run from a work folder
func saveCLI(t *testing.T, replies map[string]any) (*CLI, *settingOwner, func() string, string) {
	t.Helper()
	c, owner, read := gatewayCommandCLI(t, replies)
	c.conversationHistory = []map[string]any{{"role": "user", "content": "in-memory only"}}
	c.model, c.sessionStart = "fake-model", time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	return c, owner, read, work
}

// Names of the entries of a folder
func folderNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveWritesTheGatewayOwnersExportOnThisMachine(t *testing.T) {
	replies := map[string]any{"session.export": map[string]any{"format": "md", "filename": "icebath_session_s.md", "content": "# owner export"}}
	c, owner, read, work := saveCLI(t, replies)
	c.saveConversation("/save markdown notes.md redact")
	want := [][2]any{{"session.export", map[string]any{"session_id": "local-s", "format": "md", "redact": true}}}
	if got := owner.pairs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if raw, err := os.ReadFile(filepath.Join(work, "notes.md")); err != nil || string(raw) != "# owner export" {
		t.Fatalf("notes.md %q %v", raw, err)
	}
	if out := read(); !strings.Contains(out, filepath.Join(work, "notes.md")) {
		t.Fatalf("output %q", out)
	}
	c.saveConversation("/save md")
	saved, _ := filepath.Glob(filepath.Join(os.Getenv("ICEBATH_HOME"), "sessions", "saved", "icebath_conversation_*.md"))
	if len(saved) != 1 {
		t.Fatalf("default saves %v", saved)
	}
	raw, _ := os.ReadFile(saved[0])
	if out := read(); string(raw) != "# owner export" || !strings.Contains(out, saved[0]) {
		t.Fatalf("default save %q output %q", raw, out)
	}
	owner.mu.Lock()
	owner.replies["session.export"] = map[string]any{}
	owner.mu.Unlock()
	c.saveConversation("/save json x.json")
	if out := read(); !strings.Contains(out, "Failed to save") {
		t.Fatalf("output %q", out)
	}
	c.conversationHistory = nil
	c.saveConversation("/save json")
	if out := read(); !strings.Contains(out, "No conversation to save") || len(owner.pairs()) != 3 {
		t.Fatalf("output %q calls %v", out, owner.pairs())
	}
	if names := folderNames(t, work); !reflect.DeepEqual(names, []string{"notes.md"}) {
		t.Fatalf("work folder %v", names)
	}
}

func TestSaveOnAnOlderGatewayFallsBackOnlyForAPlainJsonSave(t *testing.T) {
	code := int64(-32601)
	replies := map[string]any{"session.export": &gateway.GatewayRPCError{Msg: "unknown_method", Code: &code},
		"session.save": map[string]any{"file": "/srv/gw/sessions/saved/icebath_conversation_x.json"}}
	c, owner, read, work := saveCLI(t, replies)
	for _, cmd := range []string{"/save md", "/save json mine.json", "/save json redact"} {
		c.saveConversation(cmd)
		if out := read(); !strings.Contains(out, "predates this CLI") {
			t.Fatalf("%s output %q", cmd, out)
		}
	}
	methods := []string{}
	for _, pair := range owner.pairs() {
		methods = append(methods, pair[0].(string))
	}
	if !reflect.DeepEqual(methods, []string{"session.export", "session.export", "session.export"}) {
		t.Fatalf("methods %v", methods)
	}
	c.saveConversation("/save json")
	pairs := owner.pairs()
	if last := pairs[len(pairs)-1]; !reflect.DeepEqual(last, [2]any{"session.save", map[string]any{"session_id": "local-s"}}) {
		t.Fatalf("last call %v", last)
	}
	if out := read(); !strings.Contains(out, "saved on the gateway host: /srv/gw/sessions/saved/icebath_conversation_x.json") {
		t.Fatalf("output %q", out)
	}
	if names := folderNames(t, work); len(names) != 0 {
		t.Fatalf("work folder %v", names)
	}
}
