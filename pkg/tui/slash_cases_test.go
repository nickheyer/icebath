package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Gateway double answering from a table keyed by method, rpc_error as the failure, empty for unknown methods
type scriptedGateway struct {
	s       *fakeSocket
	mu      sync.Mutex
	answers map[string]func(params map[string]any) map[string]any
	calls   []map[string]any
	stop    chan struct{}
}

// Attaches the app to an explicit gateway whose requests the table answers
func attachScripted(t *testing.T, a *app, answers map[string]func(params map[string]any) map[string]any) *scriptedGateway {
	t.Helper()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws")
	d := &fakeDialer{}
	a.gw.dial = d.dial
	a.gw.start()
	eventually(t, "the socket", func() bool { return d.count() == 1 })
	quiet := map[string]func(map[string]any) map[string]any{"command.observe": fixed(map[string]any{}), "shared_metrics.slash_command": fixed(map[string]any{})}
	for method, answer := range answers {
		quiet[method] = answer
	}
	g := &scriptedGateway{s: d.at(0), answers: quiet, stop: make(chan struct{})}
	g.s.open()
	eventually(t, "the open socket", func() bool { return g.s.drained() })
	go g.serve()
	t.Cleanup(func() { close(g.stop); a.gw.kill("test") })
	return g
}

// Answers every new request frame until the case ends
func (g *scriptedGateway) serve() {
	seen := 0
	for {
		select {
		case <-g.stop:
			return
		case <-time.After(time.Millisecond):
		}
		frames := g.s.sentFrames()
		for ; seen < len(frames); seen++ {
			frame := frames[seen]
			method, _ := frame["method"].(string)
			if method == "" || frame["id"] == nil {
				continue
			}
			params, _ := frame["params"].(map[string]any)
			g.mu.Lock()
			g.calls = append(g.calls, map[string]any{"method": method, "params": params})
			answer := g.answers[method]
			g.mu.Unlock()
			out := map[string]any{"jsonrpc": "2.0", "id": frame["id"]}
			if answer == nil {
				out["result"] = map[string]any{}
			} else if body := answer(params); body["rpc_error"] != nil {
				out["error"] = body["rpc_error"]
			} else {
				out["result"] = body
			}
			raw, _ := json.Marshal(out)
			g.s.message(string(raw))
		}
	}
}

// Runs a command like drain with the branch poll of the session cwd held off
func settle(a *app, cmd tea.Cmd) {
	a.seenCwd = a.store.sessionCwd()
	for depth := 0; cmd != nil && depth < 24; depth++ {
		m := cmd()
		batch, isBatch := m.(tea.BatchMsg)
		if !isBatch {
			batch = tea.BatchMsg{func() tea.Msg { return m }}
		}
		var next []tea.Cmd
		for _, c := range batch {
			if c == nil {
				continue
			}
			switch inner := c().(type) {
			case tea.BatchMsg:
				next = append(next, inner...)
			case wm.Intent, asyncDoneMsg, dieMsg:
				a.seenCwd = a.store.sessionCwd()
				_, more := a.Update(inner)
				next = append(next, more)
			}
		}
		cmd = tea.Batch(next...)
	}
}

// Params of every request of the method in order
func (g *scriptedGateway) sent(method string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, call := range g.calls {
		if call["method"] == method {
			params, _ := call["params"].(map[string]any)
			out = append(out, params)
		}
	}
	return out
}

// Answer that returns the same result for any params
func fixed(result map[string]any) func(map[string]any) map[string]any {
	return func(map[string]any) map[string]any { return result }
}

// Fails unless the screen holds every wanted line, a line the transcript wrapped counting as whole
func wantScreen(t *testing.T, a *app, wants ...string) {
	t.Helper()
	got := screen(a)
	joined := strings.Join(strings.Fields(got), "")
	for _, want := range wants {
		if !strings.Contains(got, want) && !strings.Contains(joined, strings.Join(strings.Fields(want), "")) {
			t.Fatalf("screen lacks %q:\n%s", want, got)
		}
	}
}

func TestToolsDisableRefusesAToolsetThePlatformCannotHost(t *testing.T) {
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"tools.configure": fixed(map[string]any{"changed": []any{"web"}, "unknown": []any{"nosuch"}, "missing_servers": []any{}, "enabled_toolsets": []any{}, "reset": true}),
		"session.create":  fixed(map[string]any{"session_id": "sid-2", "info": map[string]any{"model": "fake-model"}}),
	})
	a.store.ui.Sid = "sid-1"
	drain(a, a.dispatchSubmission("/tools disable nosuch discord web"))
	configured := g.sent("tools.configure")
	if len(configured) != 1 {
		t.Fatalf("tools.configure calls %v", configured)
	}
	if names, _ := configured[0]["names"].([]any); len(names) != 2 || names[0] != "nosuch" || names[1] != "web" {
		t.Fatalf("names %v", configured[0]["names"])
	}
	wantScreen(t, a, "Disabling nosuch, discord, web...", "✗ Unknown toolset 'nosuch'",
		"✗ Toolset 'discord' is not available on platform 'cli' (only: discord)", "✓ Disabled: web",
		"Session reset. New tool configuration is active.")
	if a.store.ui.Sid != "sid-2" {
		t.Fatalf("session %q after the reset", a.store.ui.Sid)
	}
}

func TestToolsEnableOfOnlyRestrictedToolsetsResetsWithoutAConfigureCall(t *testing.T) {
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.create": fixed(map[string]any{"session_id": "sid-2", "info": map[string]any{"model": "fake-model"}}),
	})
	a.store.ui.Sid = "sid-1"
	drain(a, a.dispatchSubmission("/tools enable discord_admin"))
	if calls := g.sent("tools.configure"); len(calls) != 0 {
		t.Fatalf("tools.configure sent %v", calls)
	}
	wantScreen(t, a, "Enabling discord_admin...", "✗ Toolset 'discord_admin' is not available on platform 'cli' (only: discord)",
		"Session reset. New tool configuration is active.")
	if got := toolsetPlatformRefusal(t.Context(), "web", "cli"); got != "" {
		t.Fatalf("web refused %q", got)
	}
	if got := toolsetPlatformRefusal(t.Context(), "discord", "discord"); got != "" {
		t.Fatalf("discord refused on discord %q", got)
	}
}

func TestExportAndImportMoveAProfileThroughAnArchive(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("ICEBATH_HOME", filepath.Join(userHome, ".icebath"))
	if err := os.MkdirAll(filepath.Join(userHome, ".icebath", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".icebath", "config.yaml"), []byte("model:\n  default: fake-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if got := exportProfileLines(ctx, "-o"); len(got) != 1 || got[0] != "Usage: /export [profile] [-o output.tar.gz]" {
		t.Fatalf("export usage %q", got)
	}
	archive := filepath.Join(t.TempDir(), "shared.tar.gz")
	got := exportProfileLines(ctx, "-o "+archive)
	if len(got) != 2 || got[0] != "✓ Exported 'default' to "+archive || !strings.Contains(got[1], "/import") {
		t.Fatalf("export lines %q", got)
	}
	if got := exportProfileLines(ctx, "nosuch -o "+archive); len(got) != 1 || !strings.HasPrefix(got[0], "Error: ") {
		t.Fatalf("export of a missing profile %q", got)
	}
	if got := importProfileLines(ctx, ""); len(got) != 1 || got[0] != "Usage: /import <archive.tar.gz> [--name <name>]" {
		t.Fatalf("import usage %q", got)
	}
	if got := importProfileLines(ctx, archive+" --name"); len(got) != 1 || !strings.HasPrefix(got[0], "Usage: /import") {
		t.Fatalf("import without a name value %q", got)
	}
	if got := importProfileLines(ctx, "/nowhere/missing.tar.gz"); len(got) != 1 || got[0] != "Error: Archive not found: /nowhere/missing.tar.gz" {
		t.Fatalf("import of a missing archive %q", got)
	}
	got = importProfileLines(ctx, archive+" --name copied")
	profileDir := filepath.Join(userHome, ".icebath", "profiles", "copied")
	if got[0] != "✓ Imported profile 'copied' at "+profileDir || got[len(got)-1] != "Use it: icebath -p copied" {
		t.Fatalf("import lines %q", got)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "config.yaml")); err != nil {
		t.Fatalf("imported profile lacks its config: %v", err)
	}
}

// Learned skill and two memories in the home of the app
func seedJourney(t *testing.T) string {
	t.Helper()
	h := os.Getenv("ICEBATH_HOME")
	for path, body := range map[string]string{
		filepath.Join(h, "memories", "MEMORY.md"):                    "Project uses pytest with xdist\n§\nUser prefers concise responses",
		filepath.Join(h, "skills", "demo", "tidy-notes", "SKILL.md"): "---\nname: tidy-notes\ndescription: keep notes tidy\n---\nSort the notes.\n",
		filepath.Join(h, "skills", ".usage.json"):                    `{"tidy-notes": {"use_count": 3, "created_by": "agent"}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Gateway answering the learning verbs from the seeded home the way the server does
func journeyGateway(t *testing.T, a *app) *scriptedGateway {
	t.Helper()
	env := &tools.Env{Home: os.Getenv("ICEBATH_HOME")}
	answer := func(res map[string]any, err error) map[string]any {
		if err != nil {
			return map[string]any{"rpc_error": map[string]any{"code": 5000, "message": err.Error()}}
		}
		return res
	}
	text := func(p map[string]any, key string) string { s, _ := p[key].(string); return s }
	return attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"learning.graph": func(map[string]any) map[string]any { return answer(memory.BuildLearningGraph(t.Context(), env)) },
		"learning.detail": func(p map[string]any) map[string]any {
			return answer(memory.NodeDetail(t.Context(), env, text(p, "id")))
		},
		"learning.delete": func(p map[string]any) map[string]any {
			return answer(memory.DeleteNode(t.Context(), env, text(p, "id")))
		},
		"learning.edit": func(p map[string]any) map[string]any {
			return answer(memory.EditNode(t.Context(), env, text(p, "id"), text(p, "content")))
		},
	})
}

func TestJourneyArgumentsFollowTheJourneyParser(t *testing.T) {
	show := parseJourneyArgs("--reveal 0.5 --no-color --width 30 --height 12")
	if show.output != "" || show.action != "" || show.opts.reveal != 0.5 || show.opts.color || show.opts.width != 30 || show.opts.height != 12 {
		t.Fatalf("show %+v", show)
	}
	if full := parseJourneyArgs("--reveal 0"); full.opts.reveal != 1 || full.opts.fps != 12 || !full.opts.color {
		t.Fatalf("a zero reveal %+v", full)
	}
	if play := parseJourneyArgs("--play --fps 30"); !play.opts.play || play.opts.fps != 30 {
		t.Fatalf("play %+v", play)
	}
	if del := parseJourneyArgs("delete tidy-notes -y"); del.action != "delete" || del.node != "tidy-notes" || !del.yes {
		t.Fatalf("delete %+v", del)
	}
	if edit := parseJourneyArgs("edit tidy-notes"); edit.action != "edit" || edit.node != "tidy-notes" || edit.yes {
		t.Fatalf("edit %+v", edit)
	}
	if listed := parseJourneyArgs("list"); listed.action != "list" || listed.output != "" {
		t.Fatalf("list %+v", listed)
	}
	if js := parseJourneyArgs("--json"); !js.json {
		t.Fatalf("json %+v", js)
	}
	usage := "usage: /journey [--reveal 0..1] [--play] [--fps FPS] [--width WIDTH]\n                [--height HEIGHT] [--no-color] [--json]\n                {list,delete,edit} ...\n"
	flat := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	for arg, want := range map[string]string{
		"frob":       usage + "/journey: error: argument journey_action: invalid choice: 'frob' (choose from 'list', 'delete', 'edit')",
		"list extra": usage + "/journey: error: unrecognized arguments: extra",
		"--fps 3 x":  usage + "/journey: error: argument journey_action: invalid choice: 'x' (choose from 'list', 'delete', 'edit')",
		"--fps x":    usage + "/journey: error: argument --fps: invalid int value: 'x'",
		"delete":     "usage: /journey delete [-h] [-y] node\n/journey delete: error: the following arguments are required: node",
	} {
		if got := parseJourneyArgs(arg).output; flat(got) != flat(want) {
			t.Fatalf("%s\n%s\nwant\n%s", arg, got, want)
		}
	}
}

func TestJourneyWindowWalksDeletesAndReloadsTheTimeline(t *testing.T) {
	a := testApp(t)
	seedJourney(t)
	journeyGateway(t, a)
	drain(a, a.dispatchSubmission("/journey"))
	card, isJourney := a.wm.Overlay().(*journeyCard)
	if !isJourney {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := ansi.Strip(card.View(100, 40))
	for _, want := range []string{"✦ Journey learned skills & memories over time", "skills (1)", "memories (2)", "tidy-notes",
		"User prefers concise responses", "3/3 revealed · 100%", "↑↓/jk move"} {
		if !strings.Contains(view, want) {
			t.Fatalf("journey lacks %q:\n%s", want, view)
		}
	}
	for card.activeNode() == nil || card.activeNode().Body == "" {
		card.Update(keys.Press("k"))
	}
	memoryLabel := card.activeNode().FullLabel
	card.Update(wm.ActionMsg{ID: "palette.pick"})
	if !card.item || !strings.Contains(ansi.Strip(card.View(100, 40)), memoryLabel) {
		t.Fatalf("item view of %q:\n%s", memoryLabel, ansi.Strip(card.View(100, 40)))
	}
	card.Update(wm.ActionMsg{ID: "palette.left"})
	if card.item {
		t.Fatal("left kept the item view open")
	}
	card.Update(keys.Press("d"))
	if !strings.Contains(ansi.Strip(card.View(100, 40)), "delete "+memoryLabel+"? y/N") {
		t.Fatalf("no delete confirmation:\n%s", ansi.Strip(card.View(100, 40)))
	}
	_, cmd := card.Update(keys.Press("y"))
	drain(a, cmd)
	after := ansi.Strip(card.View(100, 40))
	if strings.Contains(after, memoryLabel) && !strings.Contains(after, "memories (1)") {
		t.Fatalf("memory survived the delete:\n%s", after)
	}
	_, cmd = card.Update(keys.Press("q"))
	if cmd == nil {
		t.Fatal("q left the journey open")
	}
}

func TestJourneyPlayAnimatesTheRevealToTheEnd(t *testing.T) {
	a := testApp(t)
	seedJourney(t)
	journeyGateway(t, a)
	drain(a, a.dispatchSubmission("/journey --play --fps 60 --no-color"))
	card := a.wm.Overlay().(*journeyCard)
	if card.reveal >= 1 || card.opts.color {
		t.Fatalf("reveal %v color %v while the animation runs", card.reveal, card.opts.color)
	}
	if strings.Contains(card.View(100, 40), "\x1b[") {
		t.Fatal("no-color journey painted escapes")
	}
	tick := card.tick(card.gen, journeyPlayFrames-1)
	drain(a, tick)
	if card.reveal != 1 {
		t.Fatalf("reveal %v after the last frame", card.reveal)
	}
}

func TestJourneyListDeleteAndJSONRunFromTheCommand(t *testing.T) {
	a := testApp(t)
	seedJourney(t)
	journeyGateway(t, a)
	drain(a, a.dispatchSubmission("/journey list"))
	listed := screen(a)
	if pager, isPager := a.wm.Overlay().(*pagerCard); isPager {
		listed = pager.text
	}
	for _, want := range []string{"tidy-notes", "● tidy-notes", "◆ Project uses pytest with xdist"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("list lacks %q:\n%s", want, listed)
		}
	}
	drain(a, a.dispatchSubmission("/journey delete tidy-notes -y"))
	wantScreen(t, a, "archived")
	drain(a, a.dispatchSubmission("/journey delete nosuch -y"))
	drain(a, a.dispatchSubmission("/journey --json"))
	pager, isPager := a.wm.Overlay().(*pagerCard)
	if !isPager || !strings.Contains(pager.text, `"memory_nodes": 2`) {
		t.Fatalf("json overlay %T", a.wm.Overlay())
	}
	drain(a, a.dispatchSubmission("/journey delete"))
}

// Clone of the fixture bundle in a fresh directory, a repository with history and no remote
func gitRepo(t *testing.T) string {
	t.Helper()
	bundle, err := filepath.Abs(filepath.Join("testdata", "worktree-fixture.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"clone", "-q", bundle, dir}, {"-C", dir, "remote", "remove", "origin"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestWorktreeOutsideARepositoryRefusesEachForm(t *testing.T) {
	t.Chdir(t.TempDir())
	a := testApp(t)
	attachScripted(t, a, nil)
	a.store.ui.Info = map[string]any{"cwd": t.TempDir()}
	for _, line := range []string{"/worktree", "/worktree new", "/worktree list", "/worktree frob"} {
		settle(a, a.dispatchSubmission(line))
	}
	wantScreen(t, a, "No active worktree for this session.", "(not inside a git repository)",
		"❌ /worktree new requires being inside a git repository.", "Not inside a git repository.",
		"Unknown /worktree subcommand: frob", "Usage: /worktree [new [name] | list]")
}

// App whose profile creates trees from the local HEAD without reaching a remote
func worktreeApp(t *testing.T) *app {
	t.Helper()
	a := testApp(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("worktree_sync: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWorktreeNewMovesTheSessionAndQuitRemovesTheCleanTree(t *testing.T) {
	repo := gitRepo(t)
	t.Chdir(t.TempDir())
	a := worktreeApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.workspace.move": func(p map[string]any) map[string]any { return map[string]any{"cwd": p["cwd"]} },
	})
	a.store.ui.Sid, a.store.ui.StoredSid = "sid-1", "stored-1"
	a.store.ui.Info = map[string]any{"cwd": repo}
	t.Cleanup(func() { newWorktrees.active, newWorktrees.created = nil, nil })
	settle(a, a.dispatchSubmission("/worktree"))
	wantScreen(t, a, "No active worktree for this session.", "/worktree new [name]")
	settle(a, a.dispatchSubmission("/worktree new feat"))
	tree := filepath.Join(repo, ".worktrees", "feat")
	wantScreen(t, a, "✓ Worktree created: "+tree, "✅ Worktree ready: "+tree, "Branch: icebath/feat", "Terminal and file tools now operate in the worktree.")
	moved := g.sent("session.workspace.move")
	if len(moved) != 1 || moved[0]["cwd"] != tree || moved[0]["session_key"] != "stored-1" {
		t.Fatalf("workspace moves %v", moved)
	}
	if cwd, _ := os.Getwd(); cwd != tree || os.Getenv("TERMINAL_CWD") != tree || a.store.sessionCwd() != tree {
		t.Fatalf("process cwd %q terminal %q session %q", cwd, os.Getenv("TERMINAL_CWD"), a.store.sessionCwd())
	}
	settle(a, a.dispatchSubmission("/worktree status"))
	wantScreen(t, a, "Active worktree: "+tree)
	settle(a, a.dispatchSubmission("/worktree list"))
	listed := screen(a)
	if pager, isPager := a.wm.Overlay().(*pagerCard); isPager {
		listed = pager.text
	}
	if !strings.Contains(listed, tree) || !strings.Contains(listed, "[icebath/feat]") {
		t.Fatalf("list:\n%s", listed)
	}
	settle(a, a.dispatchSubmission("/worktree prune --dry-run"))
	var out bytes.Buffer
	settleNewWorktrees(t.Context(), &out)
	if _, err := os.Stat(tree); !os.IsNotExist(err) || !strings.Contains(out.String(), tree) {
		t.Fatalf("tree after quit: %v, printed %q", err, out.String())
	}
}

func TestWorktreeNewRefusedByTheGatewayRemovesTheFreshTree(t *testing.T) {
	repo := gitRepo(t)
	t.Chdir(t.TempDir())
	a := worktreeApp(t)
	attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.workspace.move": fixed(map[string]any{"rpc_error": map[string]any{"code": 4009, "message": "session busy"}}),
	})
	a.store.ui.Sid = "sid-1"
	a.store.ui.Info = map[string]any{"cwd": repo}
	t.Cleanup(func() { newWorktrees.active, newWorktrees.created = nil, nil })
	settle(a, a.dispatchSubmission("/worktree new busy"))
	wantScreen(t, a, "⚠ Created worktree but could not enter it: session busy")
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "busy")); !os.IsNotExist(err) {
		t.Fatalf("refused tree kept: %v", err)
	}
	if newWorktrees.active != nil {
		t.Fatalf("refused tree became active %v", newWorktrees.active)
	}
}

// Answer that walks through the given results one call at a time, the last repeating
func sequence(results ...map[string]any) func(map[string]any) map[string]any {
	var mu sync.Mutex
	return func(map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		next := results[0]
		if len(results) > 1 {
			results = results[1:]
		}
		return next
	}
}

func TestHandoffWaitsForTheGatewayAndOpensAFreshSession(t *testing.T) {
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"handoff.request": fixed(map[string]any{"queued": true, "session_key": "sid-1", "platform": "telegram", "home_name": "Home"}),
		"handoff.state": sequence(map[string]any{"state": "pending", "platform": "telegram", "error": ""},
			map[string]any{"state": "running", "platform": "telegram", "error": ""}, map[string]any{"state": "completed", "platform": "telegram", "error": ""}),
		"session.create": fixed(map[string]any{"session_id": "sid-2", "info": map[string]any{"model": "fake-model"}}),
		"session.title":  fixed(map[string]any{"title": "notes"}),
	})
	a.store.ui.Sid = "sid-1"
	settle(a, a.dispatchSubmission("/handoff"))
	wantScreen(t, a, "Usage: /handoff <platform>", "Hands the current session off to that platform's home channel.",
		"This session moves there and a fresh one opens here; resume it later with /resume.")
	settle(a, a.dispatchSubmission("/handoff Telegram"))
	if sent := g.sent("handoff.request"); len(sent) != 1 || sent[0]["platform"] != "telegram" || sent[0]["session_id"] != "sid-1" {
		t.Fatalf("requests %v", sent)
	}
	wantScreen(t, a, "↻ Handoff complete. The session is now active on telegram.", "Resume it here later with: /resume notes")
	if a.store.ui.Sid != "sid-2" {
		t.Fatalf("session %q after the handoff", a.store.ui.Sid)
	}
}

func TestHandoffReportsTheGatewayRefusalAndAFailedTransfer(t *testing.T) {
	a := testApp(t)
	attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"handoff.request": sequence(map[string]any{"rpc_error": map[string]any{"code": 4001, "message": "invalid_params",
			"data": map[string]any{"reason": "invalid_params", "detail": "Platform 'discord' is not configured/enabled in the gateway."}}},
			map[string]any{"queued": true, "session_key": "sid-1", "platform": "telegram", "home_name": "Home"}),
		"handoff.state": fixed(map[string]any{"state": "failed", "platform": "telegram", "error": "adapter.send failed: boom"}),
	})
	a.store.ui.Sid = "sid-1"
	settle(a, a.dispatchSubmission("/handoff discord"))
	wantScreen(t, a, "Platform 'discord' is not configured/enabled in the gateway.")
	settle(a, a.dispatchSubmission("/handoff telegram"))
	wantScreen(t, a, "Handoff failed: adapter.send failed: boom", "This session is intact. Try /handoff again, or /resume on the platform manually.")
	if a.store.ui.Sid != "sid-1" {
		t.Fatalf("session %q after a failed handoff", a.store.ui.Sid)
	}
	a.store.ui.Busy = true
	settle(a, a.dispatchSubmission("/handoff telegram"))
	wantScreen(t, a, "Agent is busy. Wait for the current turn to finish, then retry /handoff.")
}

func TestUsageReadsSessionUsageWithItsCreditLines(t *testing.T) {
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.usage": fixed(map[string]any{"calls": 3, "input": 1200, "output": 80, "total": 1280, "model": "fake-model",
			"context_max": 128000, "context_used": 1280, "context_percent": 1,
			"credits_lines": []any{"Subscription credits: $9.00", "Top-up credits: $3.00"}}),
	})
	a.store.ui.Sid = "sid-1"
	drain(a, a.dispatchSubmission("/usage"))
	if sent := g.sent("session.usage"); len(sent) != 1 || sent[0]["session_id"] != "sid-1" {
		t.Fatalf("session.usage calls %v", sent)
	}
	wantScreen(t, a, "Subscription credits: $9.00", "Top-up credits: $3.00", "1,280", "Context: 1,280 / 128,000 (1%)",
		"Run /subscription to change plan · /topup to add to your balance")
}
