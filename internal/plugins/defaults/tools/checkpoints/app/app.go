package app

import (
	"cmp"
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
	"github.com/nickheyer/icebath/internal/checkpoints"
	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	"github.com/nickheyer/icebath/internal/config"
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

// Sentence of a reason a server gave: capital first letter, ending in a period
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

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
		return r.open()
	case "diff":
		reference := ""
		if len(words) > 1 {
			reference = words[1]
		}
		return r.diff(nil, reference)
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
	return r.restore(nil, words[0], strings.Join(files, " "), all)
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

func (r *rollback) open() tea.Cmd {
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
				lines = append(lines, sentence(refusal))
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
				return "Could not read the checkpoints. " + sentence(tui.Reason(r.err)) + " " + r.kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !r.loaded:
				return "Reading the checkpoints…"
			case !r.state.GetEnabled():
				return checkpoints.NotEnabled
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
			"checkpoints.restore": func(l *cards.List, i int) tea.Cmd {
				l.Confirm(fmt.Sprintf("Restore checkpoint %d and take back your last message? Your edits stay.", i+1),
					func() tea.Cmd { return r.restore(r.hub, strconv.Itoa(i+1), "", false) })
				return nil
			},
			"checkpoints.restore.all": func(l *cards.List, i int) tea.Cmd {
				l.Confirm(fmt.Sprintf("Restore checkpoint %d and take back your last message? Your edits go too.", i+1),
					func() tea.Cmd { return r.restore(r.hub, strconv.Itoa(i+1), "", true) })
				return nil
			},
			"checkpoints.diff":    func(_ *cards.List, i int) tea.Cmd { return r.diff(r.hub, strconv.Itoa(i+1)) },
			"checkpoints.session": func(*cards.List, int) tea.Cmd { return r.diff(r.hub, "") },
			"checkpoints.file": func(_ *cards.List, i int) tea.Cmd {
				number := strconv.Itoa(i + 1)
				return r.kit.Ask(r.hub, "Restore one file from checkpoint "+number, cards.Field{Label: "File",
					Help: "The path of the file inside " + r.state.GetDirectory() + ", such as src/main.go."}, "Restore",
					func(file string) tea.Cmd { return r.restore(r.hub, number, file, false) })
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return r.load() },
		},
	})
	r.list.ToggleDetail()
	r.hub = r.kit.Hub("checkpoints:rollback", r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load())
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
	return strings.ToUpper(text[:1]) + text[1:]
}

// Lines under the list about a checkpoint: why it was saved, then when, what it changed and the command that restores it, then its folder
// and hash
func (r *rollback) detail(i int) []string {
	t := r.kit.Env.Theme
	entries := r.state.GetCheckpoints()
	if i < 0 || i >= len(entries) {
		return nil
	}
	entry := entries[i]
	lines := []string{r.reason(entry.GetReason()) + "."}
	facts := []string{"Saved " + entry.GetWhen().AsTime().Local().Format("Mon 2 Jan 2006 at 15:04")}
	if files := entry.GetFiles(); files > 0 {
		facts = append(facts, fmt.Sprintf("%s changed since the checkpoint before it: %s added, %s removed", render.Count(int(files), "file"),
			render.Count(int(entry.GetInsertions()), "line"), render.Count(int(entry.GetDeletions()), "line")))
	}
	facts = append(facts, "Also /rollback "+strconv.Itoa(i+1))
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	return append(lines, render.Dim("In "+r.state.GetDirectory()+" · Hash "+entry.GetHash(), t))
}

// Reads the checkpoints, and the titles of the tools their reasons name
func (r *rollback) load() tea.Cmd {
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
				return r.fail(hub, "Could not show the changes since "+since+". "+sentence(tui.Reason(err)))
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
// so the agent doesn't remember changes rolled back. What happened is paged
func (r *rollback) restore(hub *cards.Hub, checkpoint, file string, all bool) tea.Cmd {
	if hub != nil {
		hub.Busy("Restoring checkpoint " + checkpoint + "…")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.checkpoints.Restore(ctx, &checkpointsv1.RestoreRequest{SessionId: r.session, Checkpoint: checkpoint, File: file, All: all})
		var lines []string
		if err == nil {
			lines = report(response, "/rollback "+checkpoint+" --all", r.kit.Env.Theme)
			if file == "" && (len(response.GetRestored()) > 0 || !response.GetSafe()) {
				if took := r.takeBack(); took != "" {
					lines = append(lines, "", took)
				}
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return r.fail(hub, "Could not restore checkpoint "+checkpoint+". "+sentence(tui.Reason(err)))
			}
			page := cards.NewPage(r.kit.Env, cards.PageSpec{
				Title: "Restored checkpoint " + checkpoint,
				Key:   "checkpoints:restored:" + r.session,
				Lines: func(w int) []string {
					var out []string
					for _, line := range lines {
						out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
					}
					return out
				},
			})
			if hub != nil {
				return tea.Batch(hub.Push(page), hub.Done("Restored checkpoint "+checkpoint+"."), r.load())
			}
			return r.kit.WM.ShowOverlay(r.kit.Hub(page.Key(), page))
		}
	})
}

// What a restore did, as lines of the page that reports it: what was put back, what was kept and why, and what could not be removed. All
// is the command that restores the edits a restore kept
func report(restored *checkpointsv1.RestoreResponse, all string, t theme.Theme) []string {
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
		lines := []string{"Nothing was restored: you edited every file that changed since the checkpoint. " + all + " puts those back too."}
		return append(lines, files("Your edits, kept", restored.GetKeptEdits())...)
	}
	what := "your files"
	if restored.GetFile() != "" {
		what = restored.GetFile()
	}
	lines := []string{"Restored " + what + " to checkpoint " + restored.GetRestoredTo() + "."}
	if reason := strings.TrimSpace(restored.GetReason()); reason != "" {
		lines = append(lines, render.Dim("It was saved for this reason: "+reason+".", t))
	}
	lines = append(lines, "A checkpoint of how your files were just before was saved, so you can undo this rollback.")
	lines = append(lines, files("Put back", restored.GetRestored())...)
	if edits := restored.GetKeptEdits(); len(edits) > 0 {
		lines = append(lines, files("Your edits, kept", edits)...)
		lines = append(lines, render.Dim(all+" puts these back too.", t))
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
		return "Your last message was kept. " + sentence(tui.Reason(err))
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
		return "Your last message was kept. " + sentence(tui.Reason(err))
	}
	return "Took back your last message and the reply to it, " + render.Count(int(edited.GetRemoved()), "message") +
		" in all, so the agent does not remember the changes you rolled back."
}

// Opens the checkpoint storage of the profile in use: what it takes by project, and the cleanups
func (c *clients) storage(kit *tui.Kit, _ string) tea.Cmd {
	return (&storage{clients: c, kit: kit}).open()
}

// Checkpoint storage of the profile in use in a list of its projects
type storage struct {
	*clients
	kit    *tui.Kit
	hub    *cards.Hub
	list   *cards.List
	state  *checkpointsv1.StatusResponse
	loaded bool
	err    error
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
	var shown []*checkpointsv1.Project
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Checkpoint storage",
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
		Detail: func(i int) []string { return s.detail(shown[i]) },
		Empty: func() string {
			switch {
			case s.err != nil:
				return "Could not read the checkpoint storage. " + sentence(tui.Reason(s.err)) + " " + s.kit.Env.Keys.Display("hub.refresh") + " tries again."
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
				l.Confirm("Delete the checkpoints of "+filepath.Base(folder(project))+"? Its folder is gone.", func() tea.Cmd { return s.drop(project) })
				return nil
			},
			"checkpoints.prune": func(l *cards.List, _ int) tea.Cmd {
				if len(s.state.GetProjects()) == 0 {
					return nil
				}
				return s.prune(l)
			},
			"checkpoints.clear.legacy": func(l *cards.List, _ int) tea.Cmd {
				if len(s.state.GetArchives()) == 0 {
					return nil
				}
				l.Confirm("Delete the old checkpoint archives? They take "+size(s.state.GetLegacyBytes())+".", s.clearLegacy)
				return nil
			},
			"checkpoints.clear": func(l *cards.List, _ int) tea.Cmd {
				if len(s.state.GetProjects()) == 0 && len(s.state.GetArchives()) == 0 {
					return nil
				}
				l.Confirm("Delete every checkpoint? /rollback can't undo anything after this.", s.clear)
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load() },
		},
	})
	s.list.ToggleDetail()
	s.hub = s.kit.Hub("checkpoints:storage", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Lines above the projects: the warning of a store at its size, how much the checkpoints take and where they are kept
func (s *storage) intro() []string {
	var lines []string
	if footprint := s.state.GetFootprint(); footprint != "" {
		lines = append(lines, footprint)
	}
	if !s.loaded || s.err != nil || (len(s.state.GetProjects()) == 0 && len(s.state.GetArchives()) == 0) {
		return lines
	}
	said := "Checkpoints take " + size(s.state.GetTotalBytes()) + " in all"
	if archives := len(s.state.GetArchives()); archives > 0 {
		said += ": " + size(s.state.GetStoreBytes()) + " for your projects and " + size(s.state.GetLegacyBytes()) + " in " +
			render.Count(archives, "old archive")
	}
	return append(lines, said+".", "They are kept in "+s.state.GetBase()+".")
}

// When a checkpoint of the project was saved last, in words
func (s *storage) last(project *checkpointsv1.Project) string {
	if project.GetTouched() == nil {
		return ""
	}
	return ago(project.GetTouched().AsTime())
}

// Lines under the list about a project: its whole folder, its checkpoints and when they were saved, and why a gone folder may only look gone
func (s *storage) detail(project *checkpointsv1.Project) []string {
	t := s.kit.Env.Theme
	lines := []string{folder(project)}
	facts := []string{render.Count(int(project.GetCommits()), "checkpoint")}
	if project.GetTouched() != nil {
		facts = append(facts, "Last saved "+s.last(project))
	}
	if project.GetCreated() != nil {
		facts = append(facts, "First saved "+project.GetCreated().AsTime().Local().Format("2 Jan 2006"))
	}
	lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	if !project.GetExists() {
		lines = append(lines, render.Warm("Its folder is gone. It may have been deleted, or its drive, share or VPN is not connected.", t))
	}
	return lines
}

func (s *storage) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.checkpoints.Status(s.kit.Env.Ctx, &checkpointsv1.StatusRequest{})
		return func() tea.Cmd {
			if err == nil {
				s.state = response
			}
			s.err, s.loaded = err, true
			s.list.Refresh()
			return nil
		}
	})
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
		return s.checkpoints.ClearLegacy(s.kit.Env.Ctx, &checkpointsv1.ClearLegacyRequest{})
	}, func(cleared *checkpointsv1.ClearLegacyResponse) tea.Cmd {
		text := "Deleted " + render.Count(int(cleared.GetDeleted()), "old archive") + " and freed " + size(cleared.GetFreed()) + "."
		if cleared.GetErrors() > 0 {
			return tea.Batch(s.hub.Fail(text+failures(cleared.GetErrors())), s.load())
		}
		return tea.Batch(s.hub.Done(text), s.load())
	})
}

// Deletes every checkpoint
func (s *storage) clear() tea.Cmd {
	s.hub.Busy("Deleting every checkpoint…")
	return tui.Call(s.hub, func() (*checkpointsv1.ClearResponse, error) {
		return s.checkpoints.Clear(s.kit.Env.Ctx, &checkpointsv1.ClearRequest{})
	}, func(cleared *checkpointsv1.ClearResponse) tea.Cmd {
		return tea.Batch(s.hub.Done("Deleted every checkpoint and freed "+size(cleared.GetFreed())+"."), s.load())
	})
}

// Deletes the checkpoints of one project whose folder is gone, and no other
func (s *storage) drop(project *checkpointsv1.Project) tea.Cmd {
	s.hub.Busy("Deleting the checkpoints of " + folder(project) + "…")
	return tui.Call(s.hub, func() (*checkpointsv1.PruneResponse, error) {
		return s.checkpoints.Prune(s.kit.Env.Ctx, &checkpointsv1.PruneRequest{Orphans: true, AllowListed: true, Allow: []string{project.GetHash()}})
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

// Form of a cleanup, its numbers those of the settings Keep for and Disk limit: how old a project may get, the size the checkpoints are
// brought within and whether projects whose folder is gone go; the projects whose folder is gone now are the only ones it deletes
func (s *storage) prune(l *cards.List) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		days, limit := 7, 500
		if got, err := s.settings.Get(ctx, &configv1.GetRequest{Key: checkpointsconfig.Retention}); err == nil {
			if n, ok := whole(got.GetSetting().GetValue(), 24*time.Hour); ok {
				days = n
			}
		}
		if got, err := s.settings.Get(ctx, &configv1.GetRequest{Key: checkpointsconfig.MaxTotalSize}); err == nil {
			if n, ok := whole(got.GetSetting().GetValue(), 0); ok {
				limit = n
			}
		}
		return func() tea.Cmd { return s.hub.Push(s.pruneForm(l, days, limit)) }
	})
}

func (s *storage) pruneForm(l *cards.List, days, limit int) *cards.Form {
	count := func(unit string) func(string) string {
		return func(value string) string {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err != nil || n < 0 {
				return "Enter a whole number of " + unit + ", 0 or more."
			}
			return ""
		}
	}
	orphans := s.orphans()
	fields := []cards.Field{
		{ID: "days", Label: "Days untouched", Kind: cards.FieldNumber, Value: strconv.Itoa(days), Required: true, Check: count("days"),
			Help: "Delete the checkpoints of projects with no new checkpoint for this many days.\n0 keeps them however old they are."},
		{ID: "size", Label: "Disk limit in MB", Kind: cards.FieldNumber, Value: strconv.Itoa(limit), Required: true, Check: count("MB"),
			Help: "Then delete the oldest checkpoints of each project until all of them fit. The newest of each project is always kept.\n0 means no limit."},
	}
	if len(orphans) > 0 {
		fields = append(fields, cards.Field{ID: "orphans", Label: "Gone folders", Kind: cards.FieldBool, Value: "true",
			Help: "Also delete the checkpoints of " + render.Count(len(orphans), "project") + " whose folder is gone. A folder can look gone when its " +
				"drive, share or VPN is not connected."})
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Prune checkpoints",
		Intro:  []string{"Delete old checkpoints to free disk space."},
		Fields: fields,
		Submit: "Prune",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			days, _ := strconv.Atoi(strings.TrimSpace(values["days"]))
			limit, _ := strconv.Atoi(strings.TrimSpace(values["size"]))
			request := &checkpointsv1.PruneRequest{RetentionDays: int32(days), MaxSizeMb: int32(limit), Orphans: values["orphans"] == "true", AllowListed: true}
			if request.Orphans {
				for _, project := range orphans {
					request.Allow = append(request.Allow, project.GetHash())
				}
			}
			var parts []string
			if days > 0 {
				parts = append(parts, "checkpoints untouched for "+render.Count(days, "day"))
			}
			if request.Orphans {
				parts = append(parts, "those of "+render.Count(len(orphans), "gone folder"))
			}
			question := ""
			switch {
			case len(parts) > 0:
				question = "Delete " + strings.Join(parts, " and ") + "?"
			case limit > 0:
				question = "Delete the oldest checkpoints until all fit in " + strconv.Itoa(limit) + " MB?"
			default:
				return tea.Batch(s.hub.Close(form), s.hub.Say("Nothing to prune: every number is 0 and no gone folder is chosen.", cards.NoteInfo))
			}
			l.Confirm(question, func() tea.Cmd { return s.run(request) })
			return s.hub.Close(form)
		},
	})
	return form
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
			text = "Deleted the checkpoints of " + strings.Join(parts, " and ") + " and freed " + size(pruned.GetFreed()) + "."
		}
		if pruned.GetErrors() > 0 {
			return tea.Batch(s.hub.Fail(text+failures(pruned.GetErrors())), s.load())
		}
		return tea.Batch(s.hub.Done(text), s.load())
	})
}
