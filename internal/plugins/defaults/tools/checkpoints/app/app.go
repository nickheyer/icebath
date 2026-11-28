package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	checkpointsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1/checkpointsv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the checkpoints of a session and of the checkpoint storage
const (
	rollbackContext = "checkpoints.rollback"
	storageContext  = "checkpoints.storage"
)

var rollbackKeys = []keys.Action{
	{ID: "checkpoints.restore", Context: rollbackContext, Label: "restore", Doc: "Undo the agent's changes since the checkpoint, keeping your edits", Default: []string{"enter"}},
	{ID: "checkpoints.restore.all", Context: rollbackContext, Label: "restore all", Doc: "Restore every file changed since the checkpoint, your edits too", Default: []string{"a"}},
	{ID: "checkpoints.diff", Context: rollbackContext, Label: "show changes", Doc: "Show what changed since the checkpoint", Default: []string{"d"}},
	{ID: "checkpoints.file", Context: rollbackContext, Label: "restore file", Doc: "Restore one file from the checkpoint", Default: []string{"f"}},
	{ID: "checkpoints.session", Context: rollbackContext, Label: "compare oldest", Doc: "Show everything changed since the oldest checkpoint kept", Default: []string{"s"}},
}

var storageKeys = []keys.Action{
	{ID: "checkpoints.prune", Context: storageContext, Label: "prune", Doc: "Delete old checkpoints and those of projects that are gone", Default: []string{"p"}},
	{ID: "checkpoints.clear.legacy", Context: storageContext, Label: "clear archives", Doc: "Delete the archives left from the old checkpoint folder", Default: []string{"l"}},
	{ID: "checkpoints.clear", Context: storageContext, Label: "clear all", Doc: "Delete every checkpoint", Default: []string{"x"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	c := &clients{checkpoints: checkpointsv1connect.NewCheckpointServiceClient(conn), sessions: sessionsv1connect.NewSessionServiceClient(conn),
		agents: agentv1connect.NewAgentServiceClient(conn), tools: toolsv1connect.NewToolServiceClient(conn), settings: configv1connect.NewConfigServiceClient(conn)}
	return []tui.Screen{
		{
			ID:      "rollback",
			Group:   tui.GroupSessions,
			Order:   30,
			Title:   "Rollback",
			Short:   "Restore files from a checkpoint",
			Slash:   []string{"rollback"},
			Usage:   "[list|diff [N]|<N> [file] [--all]]",
			Actions: rollbackKeys,
			Where:   map[string]string{rollbackContext: "checkpoints of the session"},
			Open:    c.rollback,
		},
		{
			ID:      "checkpoints",
			Group:   tui.GroupAbilities,
			Order:   120,
			Title:   "Checkpoint storage",
			Short:   "Free the disk space that checkpoints take",
			Slash:   []string{"checkpoints"},
			Usage:   "[@profile]",
			Actions: storageKeys,
			Where:   map[string]string{storageContext: "checkpoint storage"},
			Open:    c.storage,
		},
	}
}

type clients struct {
	checkpoints checkpointsv1connect.CheckpointServiceClient
	sessions    sessionsv1connect.SessionServiceClient
	agents      agentv1connect.AgentServiceClient
	tools       toolsv1connect.ToolServiceClient
	settings    configv1connect.ConfigServiceClient
}

// Length of time in words, rounded down to its largest whole unit: 45 seconds, 3 minutes, 1 hour, 2 days
func span(d time.Duration) string {
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if d >= unit {
			return config.Span(d.Truncate(unit))
		}
	}
	return config.Span(max(d, 0).Truncate(time.Second))
}

// How long ago a moment was, in words
func ago(at time.Time) string {
	return span(time.Since(at)) + " ago"
}

// Size of a count of bytes in the largest unit that fits
func size(n int64) string {
	return config.Size(float64(max(n, 0)))
}

// Said with the settings of checkpoints opened, when /rollback finds them off
const turnOn = "Checkpoints are off, so none were saved. Turn on Save checkpoints to have the agent save one before it changes your files."

// Lists, diffs or restores the checkpoints of the session the focused chat shows
func (c *clients) rollback(kit *tui.Kit, argument string) tea.Cmd {
	shown, chat := kit.Focused().(tui.Sessioned)
	if !chat {
		return wm.Notify("Focus a chat first. Rollback restores the files of the chat in focus.", wm.Info)
	}
	session := shown.Session()
	// A chat nothing was sent in has no session, so nothing of it was saved
	if session == "" {
		return wm.Notify("This chat has no checkpoints yet. Send a message first.", wm.Info)
	}
	r := &rollback{clients: c, kit: kit, session: session, titles: map[string]string{}}
	words := strings.Fields(argument)
	first := ""
	if len(words) > 0 {
		first = strings.ToLower(words[0])
	}
	switch first {
	case "", "list", "ls":
		return r.open(nil)
	case "diff":
		reference := ""
		if len(words) > 1 {
			reference = words[1]
		}
		return r.on(func() tea.Cmd { return r.diff(nil, reference) })
	}
	var files []string
	all := false
	for _, word := range words[1:] {
		if lower := strings.ToLower(word); lower == "--all" || lower == "--force" {
			all = true
			continue
		}
		files = append(files, word)
	}
	// One file goes back as its key on the list puts it back; the whole project after the question the list asks
	if len(files) > 0 {
		return r.on(func() tea.Cmd { return r.restore(nil, words[0], strings.Join(files, " "), all) })
	}
	return r.open(func() tea.Cmd { return r.ask(words[0], all) })
}

// Checkpoints of one session in a list
type rollback struct {
	*clients
	kit     *tui.Kit
	session string
	hub     *cards.Hub
	list    *cards.List
	state   *checkpointsv1.ListResponse
	// Titles of the tools by name, for the reasons that name the tool a checkpoint was saved before
	titles map[string]string
	loaded bool
	err    error
}

// Opens the settings of checkpoints of the session's profile, the one in use when unnamed, saying what to turn on there
func (r *rollback) off(name string) tea.Cmd {
	return tea.Batch(r.kit.Open(configapp.Settings, profile.Join(name, checkpointsconfig.Key)), wm.Notify(turnOn, wm.Info))
}

// Runs then once the session's checkpoints are known to be on; opens their settings when they are off
func (r *rollback) on(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.checkpoints.List(r.kit.Env.Ctx, &checkpointsv1.ListRequest{SessionId: r.session})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return wm.Notify("Could not read the checkpoints. "+strs.Full(tui.Reason(err)), wm.Error)
			case !response.GetEnabled():
				return r.off(response.GetProfile())
			}
			return then()
		}
	})
}

// Opens the list over the screen, then runs then once the checkpoints were read and are on
func (r *rollback) open(then func() tea.Cmd) tea.Cmd {
	t := r.kit.Env.Theme
	r.list = cards.NewList(r.kit.Env, cards.ListSpec{
		Title:   "Rollback",
		Glyph:   t.Glyphs.Jobs,
		Key:     "checkpoints:rollback",
		Context: rollbackContext,
		Primary: "checkpoints.restore",
		Free:    map[string]bool{"checkpoints.session": true, "hub.refresh": true},
		Intro: func() []string {
			var lines []string
			if refusal := r.state.GetRefusal(); refusal != "" {
				lines = append(lines, render.Warm(strs.Full(refusal), t))
			}
			if dir := r.state.GetDirectory(); dir != "" && len(r.state.GetCheckpoints()) > 0 {
				lines = append(lines, "Checkpoints of "+dir+", newest first. Restoring one puts your files back as they were then.")
			}
			return lines
		},
		Columns: []render.Column{{Right: true}, {}, {Flex: 1}, {}},
		Yield:   []int{3},
		Rows: func() [][]string {
			rows := make([][]string, len(r.state.GetCheckpoints()))
			for i, entry := range r.state.GetCheckpoints() {
				rows[i] = []string{strconv.Itoa(i + 1), ago(entry.GetWhen().AsTime()), r.reason(entry.GetReason()), render.Dim(changed(entry), t)}
			}
			return rows
		},
		Detail: func(i int) []string { return r.detail(i) },
		Empty: func() string {
			switch {
			case r.err != nil:
				return "Could not read the checkpoints. " + strs.Full(tui.Reason(r.err)) + " " + r.kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !r.loaded || !r.state.GetEnabled():
				return "Reading the checkpoints…"
			}
			return "No checkpoints of " + r.state.GetDirectory() + " yet. The agent saves one before it changes your files."
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "checkpoints.restore"}, {Action: "checkpoints.diff"}, {Action: "checkpoints.file"}, {Action: "checkpoints.restore.all"},
				{Action: "checkpoints.session"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"checkpoints.restore":     func(_ *cards.List, i int) tea.Cmd { return r.ask(strconv.Itoa(i+1), false) },
			"checkpoints.restore.all": func(_ *cards.List, i int) tea.Cmd { return r.ask(strconv.Itoa(i+1), true) },
			"checkpoints.diff":        func(_ *cards.List, i int) tea.Cmd { return r.diff(r.hub, strconv.Itoa(i+1)) },
			"checkpoints.session":     func(*cards.List, int) tea.Cmd { return r.diff(r.hub, "") },
			"checkpoints.file": func(_ *cards.List, i int) tea.Cmd {
				number := strconv.Itoa(i + 1)
				return r.kit.Ask(r.hub, "Restore one file from checkpoint "+number, cards.Field{Label: "File",
					Help: "The path of the file inside " + r.state.GetDirectory() + ", such as src/main.go."}, "Restore",
					func(file string) tea.Cmd { return r.restore(r.hub, number, file, false) })
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return r.load(nil) },
		},
	})
	r.list.ToggleDetail()
	r.hub = r.kit.Hub("checkpoints:rollback", r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load(then))
}

// Puts the checkpoint the reference names in hand and asks before restoring the project to it, the person's own edits too when all is
// set: a restore takes back the last message and its reply
func (r *rollback) ask(reference string, all bool) tea.Cmd {
	i := r.index(reference)
	if i < 0 {
		return r.hub.Fail("There is no checkpoint " + reference + " of " + r.state.GetDirectory() + ".")
	}
	r.list.Select(i)
	number := strconv.Itoa(i + 1)
	q := cards.Question{Text: "Restore checkpoint " + number + "? The agent's changes since then and your last message are taken back, and your " +
		"own edits stay.", Verb: "Restore"}
	if all {
		q = everything(number)
	}
	r.list.Confirm(q, func() tea.Cmd { return r.restore(r.hub, number, "", all) })
	return nil
}

// Row of the checkpoint a reference names, by its number in the list or the start of its hash; -1 for none
func (r *rollback) index(reference string) int {
	entries := r.state.GetCheckpoints()
	if n, err := strconv.Atoi(reference); err == nil {
		if n >= 1 && n <= len(entries) {
			return n - 1
		}
		return -1
	}
	for i, entry := range entries {
		if reference != "" && strings.HasPrefix(entry.GetHash(), strings.ToLower(reference)) {
			return i
		}
	}
	return -1
}

// Question asked before a restore that takes back every change since the checkpoint, the person's own edits too
func everything(number string) cards.Question {
	return cards.Question{Text: "Restore checkpoint " + number + " with your own edits too? Every change since then and your last message are taken back.",
		Verb: "Restore all"}
}

// Files and lines a checkpoint changed against the one before it, empty for none
func changed(entry *checkpointsv1.Checkpoint) string {
	if entry.GetFiles() == 0 {
		return ""
	}
	return render.Count(int(entry.GetFiles()), "file")
}

// Why a checkpoint was saved, in words: before a command, before a tool of the agent, before a rollback
func (r *rollback) reason(text string) string {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "No reason saved"
	case strings.HasPrefix(text, "before terminal: "):
		return "Before the command " + strings.TrimPrefix(text, "before terminal: ")
	case strings.HasPrefix(text, "pre-rollback"):
		if _, to, ok := strings.Cut(text, "restoring to "); ok {
			return "Before the rollback to " + strings.TrimSuffix(to, ")")
		}
		return "Before a rollback"
	case strings.HasPrefix(text, "before "):
		name := strings.TrimPrefix(text, "before ")
		return "Before the agent used " + cmp.Or(r.titles[name], name)
	}
	return strs.Capital(text)
}

// Lines under the list about a checkpoint beyond what its row says: its whole reason where the row cuts it, then when it was saved, what
// it changed and the command that restores it, then its folder and hash
func (r *rollback) detail(i int) []string {
	t := r.kit.Env.Theme
	entries := r.state.GetCheckpoints()
	if i < 0 || i >= len(entries) {
		return nil
	}
	entry := entries[i]
	var lines []string
	if r.list.Cut(i, 2) {
		lines = append(lines, r.reason(entry.GetReason())+".")
	}
	facts := []string{"Saved " + entry.GetWhen().AsTime().Local().Format("Mon 2 Jan 2006 at 15:04")}
	if files := entry.GetFiles(); files > 0 {
		facts = append(facts, fmt.Sprintf("%s changed since the checkpoint before it: %s added, %s removed", render.Count(int(files), "file"),
			render.Count(int(entry.GetInsertions()), "line"), render.Count(int(entry.GetDeletions()), "line")))
	}
	facts = append(facts, "Also /rollback "+strconv.Itoa(i+1))
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	return append(lines, render.Dim("In "+r.state.GetDirectory()+" · Hash "+entry.GetHash(), t))
}

// Reads the checkpoints, and the titles of the tools their reasons name, then runs then; opens the settings of checkpoints in place of the
// list when they are off
func (r *rollback) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.checkpoints.List(ctx, &checkpointsv1.ListRequest{SessionId: r.session})
		titles := map[string]string{}
		if listed, err := r.tools.List(ctx, &toolsv1.ListRequest{}); err == nil {
			for _, tool := range listed.GetTools() {
				titles[tool.GetName()] = tool.GetTitle()
			}
		}
		return func() tea.Cmd {
			if err == nil {
				r.state = response
			}
			r.err, r.loaded, r.titles = err, true, titles
			r.list.Refresh()
			switch {
			case err == nil && !response.GetEnabled():
				return r.off(response.GetProfile())
			case err == nil && then != nil:
				return then()
			}
			return nil
		}
	})
}

// Pages what changed in the project since the checkpoint, the oldest kept when none is named, over the list when one is open
func (r *rollback) diff(hub *cards.Hub, checkpoint string) tea.Cmd {
	since := "checkpoint " + checkpoint
	if checkpoint == "" {
		since = "the oldest checkpoint"
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := r.checkpoints.Diff(r.kit.Env.Ctx, &checkpointsv1.DiffRequest{SessionId: r.session, Checkpoint: checkpoint})
		return func() tea.Cmd {
			if err != nil {
				return r.fail(hub, "Could not show the changes since "+since+". "+strs.Full(tui.Reason(err)))
			}
			if response.GetHash() == "" {
				return r.say(hub, "There are no checkpoints of this folder yet.", wm.Info)
			}
			if strings.TrimSpace(response.GetStat()) == "" && strings.TrimSpace(response.GetDiff()) == "" {
				return r.say(hub, "Nothing changed since "+since+".", wm.Info)
			}
			t := r.kit.Env.Theme
			page := cards.NewPage(r.kit.Env, cards.PageSpec{
				Title: "Changes since " + since,
				Key:   "checkpoints:diff:" + response.GetHash(),
				Lines: func(w int) []string {
					var out []string
					for _, line := range strings.Split(response.GetStat(), "\n") {
						if line != "" {
							out = append(out, render.Dim(render.Truncate(line, w, t), t))
						}
					}
					if len(out) > 0 {
						out = append(out, "")
					}
					for _, line := range strings.Split(response.GetDiff(), "\n") {
						out = append(out, paint(render.Truncate(line, w, t), line, t))
					}
					return out
				},
			})
			if hub != nil {
				return hub.Push(page)
			}
			return r.kit.WM.ShowOverlay(r.kit.Hub(page.Key(), page))
		}
	})
}

// A diff line in the color of what it is
func paint(shown, line string, t theme.Theme) string {
	switch {
	case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index "):
		return render.Heading(shown, t)
	case strings.HasPrefix(line, "@@"):
		return render.Cool(shown, t)
	case strings.HasPrefix(line, "+"):
		return render.Good(shown, t)
	case strings.HasPrefix(line, "-"):
		return render.Bad(shown, t)
	}
	return shown
}

func (r *rollback) fail(hub *cards.Hub, text string) tea.Cmd {
	if hub != nil {
		return hub.Fail(text)
	}
	return wm.Notify(text, wm.Error)
}

func (r *rollback) say(hub *cards.Hub, text string, level wm.Level) tea.Cmd {
	if hub != nil {
		return hub.Say(text, cards.NoteInfo)
	}
	return wm.Notify(text, level)
}

// Restores the project, or one file of it, to the checkpoint; a restore of the whole project also takes back the session's last message,
// so the agent doesn't remember changes rolled back. What happened is paged, over the list when one is open, with the restore of the edits
// it kept on a key of its own
func (r *rollback) restore(hub *cards.Hub, checkpoint, file string, all bool) tea.Cmd {
	if hub != nil {
		hub.Busy("Restoring checkpoint " + checkpoint + "…")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.checkpoints.Restore(ctx, &checkpointsv1.RestoreRequest{SessionId: r.session, Checkpoint: checkpoint, File: file, All: all})
		took := ""
		if err == nil && file == "" && (len(response.GetRestored()) > 0 || !response.GetSafe()) {
			took = r.takeBack()
		}
		return func() tea.Cmd {
			if err != nil {
				return r.fail(hub, "Could not restore checkpoint "+checkpoint+". "+strs.Full(tui.Reason(err)))
			}
			// Worded here, where the titles of the tools the list read are in hand
			lines := r.report(response)
			if took != "" {
				lines = append(lines, "", took)
			}
			kept := file == "" && response.GetSafe() && len(response.GetKeptEdits()) > 0
			owner := hub
			page := cards.NewPage(r.kit.Env, cards.PageSpec{
				Title:   "Restored checkpoint " + checkpoint,
				Key:     "checkpoints:restored:" + r.session,
				Context: rollbackContext,
				Lines: func(w int) []string {
					var out []string
					for _, line := range lines {
						out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
					}
					return out
				},
				Hints: func(*cards.Page) []wm.Hint {
					if kept {
						return []wm.Hint{{Action: "checkpoints.restore.all"}}
					}
					return nil
				},
				Actions: map[string]func(*cards.Page) tea.Cmd{
					"checkpoints.restore.all": func(p *cards.Page) tea.Cmd {
						if kept {
							p.Confirm(everything(checkpoint), func() tea.Cmd { return tea.Batch(owner.Close(p), r.restore(owner, checkpoint, "", true)) })
						}
						return nil
					},
				},
			})
			if hub != nil {
				hub.Quiet()
				// A restore asked for by the command has no list to read again
				if r.list != nil {
					return tea.Batch(hub.Push(page), r.load(nil))
				}
				return hub.Push(page)
			}
			owner = r.kit.Hub(page.Key(), page)
			return r.kit.WM.ShowOverlay(owner)
		}
	})
}

// What a restore did, as lines of the page titled with it: the file restored alone, why the checkpoint was saved, what was put back, what
// was kept and why, and what could not be removed
func (r *rollback) report(restored *checkpointsv1.RestoreResponse) []string {
	t := r.kit.Env.Theme
	files := func(heading string, paths []string) []string {
		if len(paths) == 0 {
			return nil
		}
		out := []string{"", render.Heading(heading, t)}
		for _, path := range paths {
			out = append(out, "  "+path)
		}
		return out
	}
	if restored.GetSafe() && len(restored.GetRestored()) == 0 && len(restored.GetKeptEdits()) > 0 {
		lines := []string{"Nothing was restored: you edited every file that changed since the checkpoint. Restore all puts those back too."}
		return append(lines, files("Your edits, kept", restored.GetKeptEdits())...)
	}
	var lines []string
	if file := restored.GetFile(); file != "" {
		lines = append(lines, "Only "+file+" was restored.")
	}
	if reason := strings.TrimSpace(restored.GetReason()); reason != "" {
		lines = append(lines, render.Dim("The checkpoint was saved "+strs.Lower(r.reason(reason))+".", t))
	}
	lines = append(lines, "A checkpoint of how your files were just before was saved, so you can undo this rollback.")
	lines = append(lines, files("Put back", restored.GetRestored())...)
	if edits := restored.GetKeptEdits(); len(edits) > 0 {
		lines = append(lines, files("Your edits, kept", edits)...)
		lines = append(lines, render.Dim("Restore all puts these back too.", t))
	}
	lines = append(lines, files("Kept as they are, too large for a checkpoint to hold", restored.GetKeptOversize())...)
	lines = append(lines, files("Could not be removed, so they are still there", restored.GetFailedDeletes())...)
	return lines
}

// Takes back the session's last message after a restore of its whole project: what became of it, as a line
func (r *rollback) takeBack() string {
	ctx := r.kit.Env.Ctx
	got, err := r.sessions.Get(ctx, &sessionsv1.GetRequest{Id: r.session})
	if err != nil {
		return "Your last message was kept. " + strs.Full(tui.Reason(err))
	}
	var last *sessionsv1.Message
	for _, message := range slices.Backward(got.GetMessages()) {
		if message.GetRole() == sessionsv1.Role_ROLE_USER && message.GetDisplay() != "hidden" && message.GetDisplay() != "auto_continue" {
			last = message
			break
		}
	}
	if last == nil {
		return ""
	}
	edited, err := r.agents.Edit(ctx, &agentv1.EditRequest{SessionId: r.session, MessageId: last.GetId()})
	if err != nil {
		return "Your last message was kept. " + strs.Full(tui.Reason(err))
	}
	return "Took back your last message and the reply to it, " + render.Count(int(edited.GetRemoved()), "message") +
		" in all, so the agent does not remember the changes you rolled back."
}

// Opens the checkpoint storage of the profile named, else of the one in use: what it takes by project, and the cleanups
func (c *clients) storage(kit *tui.Kit, argument string) tea.Cmd {
	name, _ := profile.Split(argument)
	return (&storage{clients: c, kit: kit, profile: name}).open()
}

// Checkpoint storage of a profile in a list of its projects
type storage struct {
	*clients
	kit *tui.Kit
	// Profile the storage was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	state   *checkpointsv1.StatusResponse
	// Checkpoints options of the profile that a prune starts from, how long checkpoints are kept and how much disk they may take, and why
	// they could not be read
	keep, limit *configv1.Setting
	missing     error
	loaded      bool
	err         error
}

// Projects with checkpoints, the one saved to last first
func (s *storage) projects() []*checkpointsv1.Project {
	projects := slices.Clone(s.state.GetProjects())
	slices.SortStableFunc(projects, func(a, b *checkpointsv1.Project) int { return b.GetTouched().AsTime().Compare(a.GetTouched().AsTime()) })
	return projects
}

// Projects whose folder is gone
func (s *storage) orphans() []*checkpointsv1.Project {
	return slices.DeleteFunc(slices.Clone(s.state.GetProjects()), func(project *checkpointsv1.Project) bool { return project.GetExists() })
}

// Folder of a project, or what stands for one that is not known
func folder(project *checkpointsv1.Project) string {
	return cmp.Or(project.GetWorkdir(), "Unknown folder")
}

func (s *storage) open() tea.Cmd {
	t := s.kit.Env.Theme
	title := "Checkpoint storage"
	if s.profile != "" {
		title += " · " + s.profile
	}
	var shown []*checkpointsv1.Project
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Jobs,
		Key:     "checkpoints:storage",
		Context: storageContext,
		Shared:  []string{cards.HubContext},
		Intro:   s.intro,
		Columns: []render.Column{{Flex: 1}, {}, {}, {}},
		Yield:   []int{2},
		Rows: func() [][]string {
			shown = s.projects()
			rows := make([][]string, len(shown))
			for i, project := range shown {
				state := ""
				if !project.GetExists() {
					state = render.Warm("Folder is gone", t)
				}
				rows[i] = []string{folder(project), render.Count(int(project.GetCommits()), "checkpoint"), render.Dim(s.last(project), t), state}
			}
			return rows
		},
		Detail: func(i int) []string { return s.detail(shown[i], s.list.Cut(i, 0), !s.list.Shows(2)) },
		Empty: func() string {
			switch {
			case connect.CodeOf(s.err) == connect.CodeNotFound:
				// A profile that is not there is not there on a second read either
				return "Could not read the checkpoint storage. " + strs.Full(tui.Reason(s.err))
			case s.err != nil:
				return "Could not read the checkpoint storage. " + strs.Full(tui.Reason(s.err)) + " " + s.kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !s.loaded:
				return "Reading the checkpoint storage…"
			}
			return "No checkpoints yet. While Save checkpoints is on, the agent saves one before it changes your files."
		},
		Free: map[string]bool{"checkpoints.prune": true, "checkpoints.clear.legacy": true, "checkpoints.clear": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if i >= 0 && i < len(shown) && !shown[i].GetExists() {
				out = append(out, wm.Hint{Action: "hub.remove", Label: "delete"})
			}
			if len(s.state.GetProjects()) > 0 {
				out = append(out, wm.Hint{Action: "checkpoints.prune"})
			}
			if len(s.state.GetArchives()) > 0 {
				out = append(out, wm.Hint{Action: "checkpoints.clear.legacy"})
			}
			if len(s.state.GetProjects()) > 0 || len(s.state.GetArchives()) > 0 {
				out = append(out, wm.Hint{Action: "checkpoints.clear"})
			}
			return out
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				project := shown[i]
				if project.GetExists() {
					return nil
				}
				l.Confirm(cards.Question{Text: "Delete the " + render.Count(int(project.GetCommits()), "checkpoint") + " of " + filepath.Base(folder(project)) +
					", whose folder is gone? You can't roll back to them after this.", Verb: "Delete", Deletes: true}, func() tea.Cmd { return s.drop(project) })
				return nil
			},
			"checkpoints.prune": func(*cards.List, int) tea.Cmd {
				if len(s.state.GetProjects()) == 0 {
					return nil
				}
				return s.prune()
			},
			"checkpoints.clear.legacy": func(l *cards.List, _ int) tea.Cmd {
				archives := len(s.state.GetArchives())
				if archives == 0 {
					return nil
				}
				l.Confirm(cards.Question{Text: "Delete the " + render.Count(archives, "old checkpoint archive") + "? That frees " +
					size(s.state.GetLegacyBytes()) + ", and what they hold can't be brought back.", Verb: "Delete", Deletes: true}, s.clearLegacy)
				return nil
			},
			"checkpoints.clear": func(l *cards.List, _ int) tea.Cmd {
				if len(s.state.GetProjects()) == 0 && len(s.state.GetArchives()) == 0 {
					return nil
				}
				l.Confirm(cards.Question{Text: "Delete every checkpoint of every project? You can't roll back to any of them after this.", Verb: "Delete",
					Deletes: true}, s.clear)
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load() },
		},
	})
	s.list.ToggleDetail()
	s.hub = s.kit.Hub("checkpoints:storage", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Lines above the projects: how much the checkpoints take, in the warm color when that is more than the disk limit with what to do, and
// where they are kept
func (s *storage) intro() []string {
	if !s.loaded || s.err != nil || (len(s.state.GetProjects()) == 0 && len(s.state.GetArchives()) == 0) {
		return nil
	}
	t := s.kit.Env.Theme
	said := "Checkpoints take " + size(s.state.GetTotalBytes()) + " in all"
	if archives := len(s.state.GetArchives()); archives > 0 {
		said += ": " + size(s.state.GetStoreBytes()) + " for your projects and " + size(s.state.GetLegacyBytes()) + " in " +
			render.Count(archives, "old archive")
	}
	lines := []string{said + "."}
	if limit, ok := whole(s.limit.GetValue(), 0); ok && limit > 0 && s.state.GetStoreBytes() >= int64(limit)*1024*1024 {
		lines = append(lines, render.Warm("That is more than the disk limit of "+size(int64(limit)*1024*1024)+". Prune deletes the oldest checkpoints "+
			"of each project until they fit.", t))
	}
	return append(lines, render.Dim("They are kept in "+s.state.GetBase()+".", t))
}

// When a checkpoint of the project was saved last, in words
func (s *storage) last(project *checkpointsv1.Project) string {
	if project.GetTouched() == nil {
		return ""
	}
	return ago(project.GetTouched().AsTime())
}

// Lines under the list about a project beyond what its row says: its whole folder where the row cuts it, when its checkpoints were saved
// last where the row has no room for it and first, and why a gone folder may only look gone
func (s *storage) detail(project *checkpointsv1.Project, cut, hidden bool) []string {
	t := s.kit.Env.Theme
	var lines []string
	if cut {
		lines = append(lines, folder(project))
	}
	var facts []string
	if hidden && project.GetTouched() != nil {
		facts = append(facts, "Last saved "+s.last(project))
	}
	if project.GetCreated() != nil {
		facts = append(facts, "First saved "+project.GetCreated().AsTime().Local().Format("2 Jan 2006"))
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	if !project.GetExists() {
		lines = append(lines, render.Warm("Its folder may have been deleted, or its drive, share or VPN is not connected.", t))
	}
	return lines
}

// Reads the storage, then the Checkpoints options of the profile it is of
func (s *storage) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		response, err := s.checkpoints.Status(ctx, &checkpointsv1.StatusRequest{Profile: s.profile})
		var keep, limit *configv1.Setting
		var missing error
		if err == nil {
			keep, missing = s.option(ctx, response.GetProfile(), checkpointsconfig.Retention)
			if missing == nil {
				limit, missing = s.option(ctx, response.GetProfile(), checkpointsconfig.MaxTotalSize)
			}
		}
		return func() tea.Cmd {
			if err == nil {
				s.state, s.keep, s.limit, s.missing = response, keep, limit, missing
			}
			s.err, s.loaded = err, true
			s.list.Refresh()
			return nil
		}
	})
}

// Option of the profile as the settings service has it: its title, its value and the words for its zero value
func (s *storage) option(ctx context.Context, name, key string) (*configv1.Setting, error) {
	got, err := s.settings.Get(ctx, &configv1.GetRequest{Key: key, Profile: name})
	if err != nil {
		return nil, err
	}
	return got.GetSetting(), nil
}

// Sentence of what a cleanup could not delete, empty when it deleted everything
func failures(errors int32) string {
	switch errors {
	case 0:
		return ""
	case 1:
		return " 1 could not be deleted. The log says why."
	}
	return " " + strconv.Itoa(int(errors)) + " could not be deleted. The log says why."
}

// Deletes the archives of what the checkpoint folder held before checkpoints had a store of their own
func (s *storage) clearLegacy() tea.Cmd {
	s.hub.Busy("Deleting the old archives…")
	return tui.Call(s.hub, func() (*checkpointsv1.ClearLegacyResponse, error) {
		return s.checkpoints.ClearLegacy(s.kit.Env.Ctx, &checkpointsv1.ClearLegacyRequest{Profile: s.state.GetProfile()})
	}, func(cleared *checkpointsv1.ClearLegacyResponse) tea.Cmd {
		text := "Deleted " + render.Count(int(cleared.GetDeleted()), "old archive") + " and freed " + size(cleared.GetFreed()) + "."
		if cleared.GetErrors() > 0 {
			return tea.Batch(s.hub.Fail(text+failures(cleared.GetErrors())), s.load())
		}
		return tea.Batch(s.hub.Done(text), s.load())
	})
}

// Deletes every checkpoint of the profile
func (s *storage) clear() tea.Cmd {
	s.hub.Busy("Deleting every checkpoint…")
	return tui.Call(s.hub, func() (*checkpointsv1.ClearResponse, error) {
		return s.checkpoints.Clear(s.kit.Env.Ctx, &checkpointsv1.ClearRequest{Profile: s.state.GetProfile()})
	}, func(cleared *checkpointsv1.ClearResponse) tea.Cmd {
		return tea.Batch(s.hub.Done("Deleted every checkpoint and freed "+size(cleared.GetFreed())+"."), s.load())
	})
}

// Deletes the checkpoints of one project whose folder is gone, and no other
func (s *storage) drop(project *checkpointsv1.Project) tea.Cmd {
	s.hub.Busy("Deleting the checkpoints of " + folder(project) + "…")
	return tui.Call(s.hub, func() (*checkpointsv1.PruneResponse, error) {
		return s.checkpoints.Prune(s.kit.Env.Ctx, &checkpointsv1.PruneRequest{Orphans: true, AllowListed: true, Allow: []string{project.GetHash()},
			Profile: s.state.GetProfile()})
	}, func(pruned *checkpointsv1.PruneResponse) tea.Cmd {
		if pruned.GetErrors() > 0 || pruned.GetOrphans() == 0 {
			return tea.Batch(s.hub.Fail("Could not delete the checkpoints of "+folder(project)+". The log says why."), s.load())
		}
		return tea.Batch(s.hub.Done("Deleted the checkpoints of "+folder(project)+" and freed "+size(pruned.GetFreed())+"."), s.load())
	})
}

// Whole number a setting holds, its YAML read as a length of time in unit when unit is set
func whole(value string, unit time.Duration) (int, bool) {
	var decoded any
	if yaml.Unmarshal([]byte(value), &decoded) != nil || decoded == nil {
		return 0, false
	}
	if unit > 0 {
		length, ok := config.Length(decoded, unit)
		return int(length / unit), ok
	}
	switch n := decoded.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

// Said when the options were not read and the service gave no reason
var errUnread = errors.New("The settings service gave no reason.")

// Prune in steps: which checkpoints go when some projects' folders are gone, then how old checkpoints may get and how much disk they may
// take, starting from the Checkpoints options; a question before anything is deleted
func (s *storage) prune() tea.Cmd {
	if s.missing != nil || s.keep == nil || s.limit == nil {
		return s.hub.Fail("Could not read the Checkpoints options that a prune starts from. " + strs.Full(tui.Reason(cmp.Or(s.missing, errUnread))))
	}
	flow := s.kit.Flow(s.hub, "Prune checkpoints")
	orphans := s.orphans()
	if len(orphans) == 0 {
		return s.ages(flow, nil)
	}
	gone := render.Count(len(orphans), "project")
	choices := []tui.Choice{
		{ID: "old", Title: "Old checkpoints", Detail: "Those of projects untouched for a while, then the oldest until all fit"},
		{ID: "both", Title: "Old and gone folders", Detail: "Also every checkpoint of the " + gone + " whose folder is gone"},
		{ID: "gone", Title: "Gone folders only", Detail: "Every checkpoint of the " + gone + " whose folder is gone, nothing else"},
	}
	return flow.Pick("Which checkpoints should go?", choices, "", func(choice tui.Choice) tea.Cmd {
		switch choice.ID {
		case "gone":
			return s.confirm(flow, s.request(0, 0, orphans))
		case "both":
			return s.ages(flow, orphans)
		}
		return s.ages(flow, nil)
	})
}

// Asks how old checkpoints may get and how much disk they may take, each field starting from its option; the projects whose folder is
// gone named go too
func (s *storage) ages(flow *tui.Flow, orphans []*checkpointsv1.Project) tea.Cmd {
	days, _ := whole(s.keep.GetValue(), 24*time.Hour)
	limit, _ := whole(s.limit.GetValue(), 0)
	fields := []cards.Field{
		number("days", s.keep, days, "Days after a project's last checkpoint, such as 7."),
		number("size", s.limit, limit, "The most MB that all checkpoints may take, such as 500."),
	}
	return flow.Ask("How old may checkpoints get, and how much disk may they take?", fields, "Prune", func(values map[string]string) tea.Cmd {
		days, _ := strconv.Atoi(strings.TrimSpace(values["days"]))
		limit, _ := strconv.Atoi(strings.TrimSpace(values["size"]))
		return s.confirm(flow, s.request(days, limit, orphans))
	})
}

// Field of a whole number a prune starts from an option with: the option's title, its value, and under it what to type, that it starts at
// the setting, and what 0 means in the option's own words
func number(id string, option *configv1.Setting, value int, what string) cards.Field {
	help := what + " It starts at your setting."
	if zero := option.GetZero(); zero != "" {
		help += "\n" + cmp.Or(strings.TrimSpace(option.GetZeroValue()), "0") + " means: " + zero + "."
	}
	return cards.Field{ID: id, Label: option.GetTitle(), Kind: cards.FieldNumber, Value: strconv.Itoa(value), Help: help, Check: func(text string) string {
		if n, err := strconv.Atoi(strings.TrimSpace(text)); err != nil || n < 0 {
			return "Type a whole number, 0 or more."
		}
		return ""
	}}
}

// Prune of the profile's checkpoints older than the days, then of the oldest beyond the megabytes, and of the projects whose folder is gone
// named
func (s *storage) request(days, megabytes int, orphans []*checkpointsv1.Project) *checkpointsv1.PruneRequest {
	request := &checkpointsv1.PruneRequest{RetentionDays: int32(days), MaxSizeMb: int32(megabytes), Orphans: len(orphans) > 0, AllowListed: true,
		Profile: s.state.GetProfile()}
	for _, project := range orphans {
		request.Allow = append(request.Allow, project.GetHash())
	}
	return request
}

// Ends the prune's steps and asks before it deletes, saying what goes; says so instead when the numbers keep every checkpoint
func (s *storage) confirm(flow *tui.Flow, request *checkpointsv1.PruneRequest) tea.Cmd {
	end := flow.End()
	var parts []string
	if days := int(request.GetRetentionDays()); days > 0 {
		parts = append(parts, "the checkpoints of projects untouched for "+render.Count(days, "day"))
	}
	if gone := len(request.GetAllow()); request.GetOrphans() && gone > 0 {
		parts = append(parts, "every checkpoint of "+render.Count(gone, "gone folder"))
	}
	what := strs.And(parts)
	if mb := int64(request.GetMaxSizeMb()); mb > 0 {
		fit := size(mb * 1024 * 1024)
		if what == "" {
			what = "the oldest checkpoints until all fit in " + fit
		} else {
			what += ", then the oldest until all fit in " + fit
		}
	}
	if what == "" {
		return tea.Batch(end, s.hub.Say("Nothing to prune: "+s.keep.GetTitle()+" is "+s.keep.GetZero()+" and "+s.limit.GetTitle()+" is "+
			s.limit.GetZero()+".", cards.NoteInfo))
	}
	s.list.Confirm(cards.Question{Text: "Delete " + what + "? You can't roll back to them after this.", Verb: "Prune", Deletes: true},
		func() tea.Cmd { return s.run(request) })
	return end
}

// Runs a cleanup and says what it deleted and freed
func (s *storage) run(request *checkpointsv1.PruneRequest) tea.Cmd {
	s.hub.Busy("Pruning checkpoints…")
	return tui.Call(s.hub, func() (*checkpointsv1.PruneResponse, error) {
		return s.checkpoints.Prune(s.kit.Env.Ctx, request)
	}, func(pruned *checkpointsv1.PruneResponse) tea.Cmd {
		var parts []string
		if n := pruned.GetOrphans(); n > 0 {
			parts = append(parts, render.Count(int(n), "gone folder"))
		}
		if n := pruned.GetStale(); n > 0 {
			parts = append(parts, render.Count(int(n), "untouched project"))
		}
		text := "Freed " + size(pruned.GetFreed()) + "."
		if len(parts) > 0 {
			text = "Deleted the checkpoints of " + strs.And(parts) + " and freed " + size(pruned.GetFreed()) + "."
		}
		if pruned.GetErrors() > 0 {
			return tea.Batch(s.hub.Fail(text+failures(pruned.GetErrors())), s.load())
		}
		return tea.Batch(s.hub.Done(text), s.load())
	})
}
