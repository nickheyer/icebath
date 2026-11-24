package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
	quiet := map[string]func(map[string]any) map[string]any{"command.observe": fixed(map[string]any{})}
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
	a.git.cwd, a.git.gen = a.store.sessionCwd(), max(a.git.gen, 1)
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
				a.git.cwd, a.git.gen = a.store.sessionCwd(), max(a.git.gen, 1)
				_, more := a.Update(inner)
				next = append(next, more)
			case sessionMsg:
				switch inner.m.(type) {
				case asyncDoneMsg, dieMsg:
					a.git.cwd, a.git.gen = a.store.sessionCwd(), max(a.git.gen, 1)
					_, more := a.Update(inner)
					next = append(next, more)
				}
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
		"tools.configure": func(p map[string]any) map[string]any {
			for _, name := range p["names"].([]any) {
				if name == "discord" {
					return map[string]any{"rpc_error": map[string]any{"code": 4001, "message": "invalid_params",
						"data": map[string]any{"reason": "invalid_params", "detail": "Toolset 'discord' is not available on platform 'cli' (only: discord)"}}}
				}
			}
			return map[string]any{"changed": []any{"web"}, "unknown": []any{"nosuch"}, "missing_servers": []any{}, "enabled_toolsets": []any{}, "reset": true}
		},
		"session.create": fixed(map[string]any{"session_id": "sid-2", "info": map[string]any{"model": "fake-model"}}),
	})
	a.store.ui.Sid = "sid-1"
	drain(a, a.dispatchSubmission("/tools disable nosuch discord web"))
	wantScreen(t, a, "Disabling nosuch, discord, web...", "Toolset 'discord' is not available on platform 'cli' (only: discord)")
	if a.store.ui.Sid != "sid-1" {
		t.Fatalf("session %q after a refused change", a.store.ui.Sid)
	}
	drain(a, a.dispatchSubmission("/tools disable nosuch web"))
	configured := g.sent("tools.configure")
	if len(configured) != 2 {
		t.Fatalf("tools.configure calls %v", configured)
	}
	if names, _ := configured[1]["names"].([]any); len(names) != 2 || names[0] != "nosuch" || names[1] != "web" {
		t.Fatalf("names %v", configured[1]["names"])
	}
	wantScreen(t, a, "✗ Unknown toolset 'nosuch'", "✓ Disabled: web", "Session reset. New tool configuration is active.")
	if a.store.ui.Sid != "sid-2" {
		t.Fatalf("session %q after the reset", a.store.ui.Sid)
	}
}

// Text on screen with the pager's text in front when one is open, short output landing in the transcript
func shownText(a *app) string {
	if pager, isPager := a.wm.Overlay().(*pagerCard); isPager {
		return pager.text + "\n" + screen(a)
	}
	return screen(a)
}

// Fails unless the pager or the screen holds every wanted line, a wrapped line counting as whole
func wantShown(t *testing.T, a *app, wants ...string) {
	t.Helper()
	got := shownText(a)
	joined := strings.Join(strings.Fields(got), "")
	for _, want := range wants {
		if !strings.Contains(got, want) && !strings.Contains(joined, strings.Join(strings.Fields(want), "")) {
			t.Fatalf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestExportWritesAProfileArchive(t *testing.T) {
	a := testApp(t)
	work := t.TempDir()
	t.Chdir(work)
	archive := filepath.Join(work, "shared.tar.gz")
	profileDir := filepath.Join(t.TempDir(), "profiles", "copied")
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"profiles.export": func(p map[string]any) map[string]any {
			if p["name"] == "nosuch" {
				return map[string]any{"rpc_error": map[string]any{"code": 5000, "message": "Profile 'nosuch' does not exist."}}
			}
			return map[string]any{"name": "default", "path": p["output"]}
		},
		"profiles.import": func(p map[string]any) map[string]any {
			if p["archive"] != archive {
				return map[string]any{"rpc_error": map[string]any{"code": 5000, "message": "Archive not found: " + p["archive"].(string)}}
			}
			return map[string]any{"name": "copied", "path": profileDir, "wrapper": nil}
		},
	})
	drain(a, a.dispatchSubmission("/export -o"))
	wantShown(t, a, "Usage: /export [profile] [-o output.tar.gz]")
	drain(a, a.dispatchSubmission("/export -o shared.tar.gz"))
	if sent := g.sent("profiles.export"); len(sent) != 1 || sent[0]["output"] != archive || sent[0]["name"] != nil {
		t.Fatalf("export params %v", sent)
	}
	wantShown(t, a, "✓ Exported 'default' to "+archive, "/import")
	drain(a, a.dispatchSubmission("/export nosuch -o "+archive))
	wantShown(t, a, "Error: Profile 'nosuch' does not exist.")
	// Archive import lives in the Profiles screen; /import restores a home backup after exit
	drain(a, a.dispatchSubmission("/import"))
	wantShown(t, a, tr(txt.HubsProfilesRestoreUsage))
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

// Gateway answering the learning verbs over the seeded nodes the way the server shapes its answers
func journeyGateway(t *testing.T, a *app) *scriptedGateway {
	t.Helper()
	var mu sync.Mutex
	nodes := []map[string]any{
		{"id": "tidy-notes", "kind": "skill", "label": "tidy-notes", "timestamp": 1.7e9, "content": "Sort the notes."},
		{"id": "memory:0", "kind": "memory", "label": "Project uses pytest with xdist", "timestamp": 1.7e9 + 60, "content": "Project uses pytest with xdist"},
		{"id": "memory:1", "kind": "memory", "label": "User prefers concise responses", "timestamp": 1.7e9 + 120, "content": "User prefers concise responses"},
	}
	find := func(id string) map[string]any {
		for _, n := range nodes {
			if n["id"] == id {
				return n
			}
		}
		return nil
	}
	return attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"learning.graph": func(map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			list, memories := []any{}, 0
			for _, n := range nodes {
				list = append(list, n)
				if n["kind"] == "memory" {
					memories++
				}
			}
			return map[string]any{"nodes": list, "edges": []any{}, "clusters": []any{}, "memory": []any{},
				"stats": map[string]any{"memory_nodes": memories, "skill_nodes": len(nodes) - memories}}
		},
		"learning.frames": func(p map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			count, _ := p["frames"].(float64)
			memories, rows := 0, []any{}
			for _, n := range nodes {
				glyph, style := "●", "skill"
				if n["kind"] == "memory" {
					glyph, style = "◆", "memory"
					memories++
				}
				rows = append(rows, map[string]any{"id": n["id"], "glyph": glyph, "label": n["label"], "fullLabel": n["label"],
					"meta": "17 Nov 2023", "body": n["content"], "style": style})
			}
			frames := []any{}
			for i := 0; i < max(1, int(count)); i++ {
				reveal := 1.0
				if count > 1 {
					reveal = float64(i) / (count - 1)
				}
				frames = append(frames, map[string]any{"grid": []any{[]any{[]any{"██", "skill"}}}, "date": "17 Nov 2023",
					"reveal": reveal, "visible": int(math.Round(reveal * float64(len(nodes))))})
			}
			return map[string]any{"frames": frames, "count": len(nodes), "axis": map[string]any{"start": "17 Nov 2023", "end": "now"},
				"legend": []any{map[string]any{"glyph": "●", "style": "skill", "label": fmt.Sprintf("skills (%d)", len(nodes)-memories)},
					map[string]any{"glyph": "◆", "style": "memory", "label": fmt.Sprintf("memories (%d)", memories)}},
				"buckets": []any{map[string]any{"label": "Nov 2023", "date": "17 Nov 2023", "skills": len(nodes) - memories, "memories": memories, "nodes": rows}}}
		},
		"learning.detail": func(p map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			n := find(p["id"].(string))
			if n == nil {
				return map[string]any{"ok": false, "message": "not found"}
			}
			return map[string]any{"ok": true, "id": n["id"], "kind": n["kind"], "label": n["label"], "content": n["content"]}
		},
		"learning.delete": func(p map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			for i, n := range nodes {
				if n["id"] == p["id"] {
					nodes = append(nodes[:i:i], nodes[i+1:]...)
					return map[string]any{"ok": true, "message": fmt.Sprintf("'%s' archived", p["id"])}
				}
			}
			return map[string]any{"ok": false, "message": "not found"}
		},
		"learning.edit": func(p map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			if n := find(p["id"].(string)); n != nil {
				n["content"] = p["content"]
				return map[string]any{"ok": true, "message": "updated"}
			}
			return map[string]any{"ok": false, "message": "not found"}
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
	card.View(100, 40)
	drain(a, card.refetch())
	view := ansi.Strip(card.View(100, 40))
	for _, want := range []string{"✦ Journey learned skills & memories over time", "skills (1)", "memories (2)", "tidy-notes",
		"User prefers concise responses", "3/3 revealed · 100%", "up/down move"} {
		if !strings.Contains(view, want) {
			t.Fatalf("journey lacks %q:\n%s", want, view)
		}
	}
	for card.activeNode() == nil || card.activeNode().Body == "" {
		card.Update(keys.Press("k"))
	}
	memoryLabel := card.activeNode().FullLabel
	card.Update(keys.Press("enter"))
	if !card.item || !strings.Contains(ansi.Strip(card.View(100, 40)), memoryLabel) {
		t.Fatalf("item view of %q:\n%s", memoryLabel, ansi.Strip(card.View(100, 40)))
	}
	card.Update(keys.Press("left"))
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
	card.View(100, 40)
	drain(a, card.refetch())
	if len(card.frames) != journeyPlayFrames || card.frame >= len(card.frames)-1 || card.opts.color {
		t.Fatalf("frame %d of %d color %v while the animation runs", card.frame, len(card.frames), card.opts.color)
	}
	if strings.Contains(card.View(100, 40), "\x1b[") {
		t.Fatal("no-color journey painted escapes")
	}
	tick := card.tick(card.gen, journeyPlayFrames-1)
	drain(a, tick)
	if card.frame != len(card.frames)-1 {
		t.Fatalf("frame %d after the last frame of %d", card.frame, len(card.frames))
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

func TestWorktreeOutsideARepositoryRefusesEachForm(t *testing.T) {
	t.Chdir(t.TempDir())
	a := testApp(t)
	outside := map[string]any{"repo_root": nil, "launch": nil, "lines": []any{}, "tree": nil}
	attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"worktree.status": fixed(outside), "worktree.create": fixed(outside), "worktree.list": fixed(outside),
	})
	a.store.ui.Info = &tuigateway.SessionInfo{Cwd: ptr(t.TempDir())}
	for _, line := range []string{"/worktree", "/worktree new", "/worktree list", "/worktree frob"} {
		settle(a, a.dispatchSubmission(line))
	}
	wantScreen(t, a, "No active worktree for this session.", "(not inside a git repository)",
		"❌ /worktree new requires being inside a git repository.", "Not inside a git repository.",
		"Unknown /worktree subcommand: frob", "Usage: /worktree [new [name] | list]")
}

// Gateway answering the worktree verbs for one repository, creating the tree directory a create names
func worktreeGateway(t *testing.T, a *app, repo string, moved map[string]any) *scriptedGateway {
	t.Helper()
	tree := filepath.Join(repo, ".worktrees", "feat")
	return attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.revision": fixed(map[string]any{"session_id": "sid-1", "revision": 1, "execution_generation": 0}),
		"session.mutate":   fixed(moved),
		"worktree.status":  fixed(map[string]any{"repo_root": repo, "launch": nil}),
		"worktree.create": func(p map[string]any) map[string]any {
			if err := os.MkdirAll(tree, 0o755); err != nil {
				t.Error(err)
			}
			return map[string]any{"repo_root": repo, "lines": []any{"✓ Worktree created: " + tree, "  Branch: icebath/feat", "  Base:   HEAD"},
				"tree": map[string]any{"path": tree, "branch": "icebath/feat", "repo_root": repo}}
		},
		"worktree.list":   fixed(map[string]any{"repo_root": repo, "lines": []any{repo + "  0000000 [main]", tree + "  0000000 [icebath/feat]"}}),
		"worktree.prune":  fixed(map[string]any{"repo_root": repo, "actions": []any{}, "preserved": []any{}, "error": nil}),
		"worktree.record": fixed(map[string]any{"ok": true}),
		"worktree.settle": func(map[string]any) map[string]any {
			if err := os.RemoveAll(tree); err != nil {
				t.Error(err)
			}
			return map[string]any{"lines": []any{"✓ Worktree cleaned up: " + tree}}
		},
	})
}

func TestWorktreeNewMovesTheSessionAndQuitHandsTheTreeToTheLauncher(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(t.TempDir())
	a := testApp(t)
	g := worktreeGateway(t, a, repo, map[string]any{"session_id": "sid-1", "revision": 2})
	var settled []Worktree
	var settledActive string
	a.host.SettleWorktrees = func(_ context.Context, trees []Worktree, active string) { settled, settledActive = trees, active }
	a.store.ui.Sid, a.store.ui.StoredSid = "sid-1", "stored-1"
	a.store.ui.Info = &tuigateway.SessionInfo{Cwd: ptr(repo)}
	t.Cleanup(func() { newWorktrees.active, newWorktrees.created = nil, nil })
	t.Cleanup(func() {
		launchFolder.Lock()
		launchFolder.launched, launchFolder.moved, launchFolder.pinned = nil, "", false
		launchFolder.Unlock()
	})
	settle(a, a.dispatchSubmission("/worktree"))
	wantScreen(t, a, "No active worktree for this session.", "/worktree new [name]")
	settle(a, a.dispatchSubmission("/worktree new feat"))
	tree := filepath.Join(repo, ".worktrees", "feat")
	wantScreen(t, a, "✓ Worktree created: "+tree, "✅ Worktree ready: "+tree, "Branch: icebath/feat", "Terminal and file tools now operate in the worktree.")
	if created := g.sent("worktree.create"); len(created) != 1 || created[0]["name"] != "feat" || created[0]["cwd"] != repo || created[0]["pid"] != float64(os.Getpid()) {
		t.Fatalf("worktree.create calls %v", created)
	}
	moved := g.sent("session.mutate")
	if payload, _ := moved[0]["payload"].(map[string]any); len(moved) != 1 || moved[0]["operation"] != "workspace" || payload["cwd"] != tree {
		t.Fatalf("workspace moves %v", moved)
	}
	if recorded := g.sent("worktree.record"); len(recorded) != 1 || recorded[0]["path"] != tree {
		t.Fatalf("worktree.record calls %v", recorded)
	}
	if cwd, _ := os.Getwd(); cwd != tree || launchCwd() != tree || !slices.Contains(movedFolderEnv(), "TERMINAL_CWD="+tree) || a.store.sessionCwd() != tree {
		t.Fatalf("process cwd %q window %q commands %q session %q", cwd, launchCwd(), movedFolderEnv(), a.store.sessionCwd())
	}
	settle(a, a.dispatchSubmission("/worktree status"))
	wantScreen(t, a, "Active worktree: "+tree)
	settle(a, a.dispatchSubmission("/worktree list"))
	wantShown(t, a, tree, "[icebath/feat]")
	settle(a, a.dispatchSubmission("/worktree prune --dry-run"))
	if pruned := g.sent("worktree.prune"); len(pruned) != 1 || pruned[0]["dry_run"] != true || pruned[0]["keep"] != tree {
		t.Fatalf("worktree.prune calls %v", pruned)
	}
	a.settleNewWorktrees()
	if len(settled) != 1 || settled[0].Path != tree || settled[0].Branch != "icebath/feat" || settledActive != tree {
		t.Fatalf("trees handed to the launcher %v active %q", settled, settledActive)
	}
}

func TestWorktreeNewRefusedByTheGatewayRemovesTheFreshTree(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(t.TempDir())
	a := testApp(t)
	g := worktreeGateway(t, a, repo, map[string]any{"rpc_error": map[string]any{"code": 4009, "message": "session busy"}})
	a.store.ui.Sid = "sid-1"
	a.store.ui.Info = &tuigateway.SessionInfo{Cwd: ptr(repo)}
	t.Cleanup(func() { newWorktrees.active, newWorktrees.created = nil, nil })
	settle(a, a.dispatchSubmission("/worktree new busy"))
	wantScreen(t, a, "⚠ Created worktree but could not enter it: session busy")
	if settles := g.sent("worktree.settle"); len(settles) != 1 || settles[0]["path"] != filepath.Join(repo, ".worktrees", "feat") {
		t.Fatalf("worktree.settle calls %v", settles)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feat")); !os.IsNotExist(err) {
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
		"session.list":   fixed(map[string]any{"sessions": []any{map[string]any{"id": "sid-1", "session_id": "stored-1", "title": "notes"}}}),
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
