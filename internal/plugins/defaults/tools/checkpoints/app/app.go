package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/server"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	checkpointsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/checkpoints/v1/checkpointsv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the checkpoints of a session and of the store
const (
	rollbackContext = "checkpoints.rollback"
	storageContext  = "checkpoints.storage"
)

var rollbackKeys = []keys.Action{
	{ID: "checkpoints.restore", Context: rollbackContext, Label: "restore", Doc: "restore the files the agent changed since the checkpoint, keeping your edits", Default: []string{"enter"}},
	{ID: "checkpoints.restore.all", Context: rollbackContext, Label: "restore all", Doc: "restore every file changed since the checkpoint, your edits too", Default: []string{"a"}},
	{ID: "checkpoints.diff", Context: rollbackContext, Label: "diff", Doc: "show what changed since the checkpoint", Default: []string{"d"}},
	{ID: "checkpoints.file", Context: rollbackContext, Label: "one file", Doc: "restore one file from the checkpoint", Default: []string{"f"}},
	{ID: "checkpoints.session", Context: rollbackContext, Label: "since oldest", Doc: "show everything changed since the oldest checkpoint kept", Default: []string{"s"}},
}

var storageKeys = []keys.Action{
	{ID: "checkpoints.prune", Context: storageContext, Label: "prune", Doc: "delete orphan and stale snapshots and reclaim space", Default: []string{"p"}},
	{ID: "checkpoints.clear.legacy", Context: storageContext, Label: "clear archives", Doc: "delete the archives of what the snapshot folder held before", Default: []string{"l"}},
	{ID: "checkpoints.clear", Context: storageContext, Label: "clear all", Doc: "delete every file snapshot", Default: []string{"x"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	c := &clients{checkpoints: checkpointsv1connect.NewCheckpointServiceClient(conn), sessions: sessionsv1connect.NewSessionServiceClient(conn),
		agents: agentv1connect.NewAgentServiceClient(conn)}
	return []tui.Screen{
		{
			ID:      "rollback",
			Title:   "Rollback",
			Short:   "list, diff, or restore checkpoints",
			Slash:   []string{"rollback"},
			Usage:   "[list|diff [N]|<N> [file] [--all]]",
			Actions: rollbackKeys,
			Where:   map[string]string{rollbackContext: "checkpoints of the session"},
			Open:    c.rollback,
		},
		{
			ID:      "checkpoints",
			Title:   "File snapshots",
			Short:   "size of the snapshots /rollback uses, by project",
			Slash:   []string{"checkpoints"},
			Actions: storageKeys,
			Where:   map[string]string{storageContext: "file snapshots"},
			Open:    c.storage,
		},
	}
}

type clients struct {
	checkpoints checkpointsv1connect.CheckpointServiceClient
	sessions    sessionsv1connect.SessionServiceClient
	agents      agentv1connect.AgentServiceClient
}

// Lists, diffs or restores the checkpoints of the session the focused chat shows
func (c *clients) rollback(kit *tui.Kit, argument string) tea.Cmd {
	session := ""
	if shown, ok := kit.Focused().(tui.Sessioned); ok {
		session = shown.Session()
	}
	if session == "" {
		return wm.Notify("no active session — nothing to rollback", wm.Info)
	}
	r := &rollback{clients: c, kit: kit, session: session}
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
	loaded  bool
	err     error
}

func (r *rollback) open() tea.Cmd {
	t := r.kit.Env.Theme
	r.list = cards.NewList(r.kit.Env, cards.ListSpec{
		Title:   "Rollback checkpoints",
		Glyph:   t.Glyphs.Jobs,
		Key:     "checkpoints:rollback",
		Context: rollbackContext,
		Primary: "checkpoints.restore",
		Free:    map[string]bool{"checkpoints.session": true},
		Columns: []render.Column{{Title: "#"}, {Title: "Checkpoint"}, {Title: "Taken"}, {Title: "Reason", Flex: 3}, {Title: "Changed"}},
		Yield:   []int{4, 2},
		Rows: func() [][]string {
			rows := make([][]string, len(r.state.GetCheckpoints()))
			for i, entry := range r.state.GetCheckpoints() {
				rows[i] = []string{fmt.Sprint(i + 1), entry.GetShort(), entry.GetWhen().AsTime().Local().Format("2006-01-02 15:04"),
					cmp.Or(entry.GetReason(), "(no metadata)"), changed(entry, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			entries := r.state.GetCheckpoints()
			if i < 0 || i >= len(entries) {
				return nil
			}
			return []string{entries[i].GetHash() + " · " + r.state.GetDirectory()}
		},
		Banner: func() string {
			if refusal := r.state.GetRefusal(); refusal != "" {
				return render.Warm(refusal, t)
			}
			return ""
		},
		Empty: func() string {
			switch {
			case r.loaded && r.err == nil && !r.state.GetEnabled():
				return "checkpoints are not enabled — turn on File checkpoints in /settings under Tools › Checkpoints"
			case r.loaded && r.err == nil:
				return "no checkpoints found for " + r.state.GetDirectory()
			}
			return tui.Waiting(r.loaded, r.err, "")
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"checkpoints.restore": func(l *cards.List, i int) tea.Cmd {
				return r.confirm(l, i, "Restore the files the agent changed since checkpoint %d? Your own edits are kept.", false)
			},
			"checkpoints.restore.all": func(l *cards.List, i int) tea.Cmd {
				return r.confirm(l, i, "Restore every file changed since checkpoint %d, your own edits too?", true)
			},
			"checkpoints.diff":    func(_ *cards.List, i int) tea.Cmd { return r.diff(r.hub, fmt.Sprint(i+1)) },
			"checkpoints.session": func(*cards.List, int) tea.Cmd { return r.diff(r.hub, "") },
			"checkpoints.file": func(_ *cards.List, i int) tea.Cmd {
				number := fmt.Sprint(i + 1)
				return r.kit.Ask(r.hub, "Restore one file from checkpoint "+number, cards.Field{Label: "File", Placeholder: "path relative to " +
					r.state.GetDirectory()}, "Restore", func(file string) tea.Cmd { return r.restore(r.hub, number, file, false) })
			},
		},
	})
	r.hub = r.kit.Hub("checkpoints:rollback", r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load())
}

// What a checkpoint changed against the one before it
func changed(entry *checkpointsv1.Checkpoint, t theme.Theme) string {
	if entry.GetFiles() == 0 {
		return ""
	}
	return render.Count(int(entry.GetFiles()), "file") + " " + render.Good(fmt.Sprintf("+%d", entry.GetInsertions()), t) + "/" +
		render.Bad(fmt.Sprintf("-%d", entry.GetDeletions()), t)
}

func (r *rollback) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.checkpoints.List(r.kit.Env.Ctx, &checkpointsv1.ListRequest{SessionId: r.session})
		return func() tea.Cmd {
			if err == nil {
				r.state = response
			}
			r.err, r.loaded = err, true
			r.list.Refresh()
			return nil
		}
	})
}

func (r *rollback) confirm(l *cards.List, i int, question string, all bool) tea.Cmd {
	l.Confirm(fmt.Sprintf(question, i+1), func() tea.Cmd { return r.restore(r.hub, fmt.Sprint(i+1), "", all) })
	return nil
}

// Pages what changed in the project since the checkpoint, over the list when one is open
func (r *rollback) diff(hub *cards.Hub, checkpoint string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.checkpoints.Diff(r.kit.Env.Ctx, &checkpointsv1.DiffRequest{SessionId: r.session, Checkpoint: checkpoint})
		return func() tea.Cmd {
			if err != nil {
				return r.fail(hub, "rollback failed: "+tui.Reason(err))
			}
			if response.GetHash() == "" {
				return r.say(hub, "no checkpoints found", wm.Info)
			}
			if strings.TrimSpace(response.GetStat()) == "" && strings.TrimSpace(response.GetDiff()) == "" {
				return r.say(hub, "no changes since this checkpoint", wm.Info)
			}
			t := r.kit.Env.Theme
			page := cards.NewPage(r.kit.Env, cards.PageSpec{
				Title: "Rollback diff · " + response.GetHash()[:min(10, len(response.GetHash()))],
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

// A diff line in the colour of what it is
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
		return hub.Done(text)
	}
	return wm.Notify(text, level)
}

// Restores the project, or one file of it, to the checkpoint; a restore of the whole project also takes back the session's last turn,
// so the agent doesn't remember changes rolled back. What happened is paged
func (r *rollback) restore(hub *cards.Hub, checkpoint, file string, all bool) tea.Cmd {
	if hub != nil {
		hub.Busy("Restoring checkpoint " + checkpoint + "…")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.checkpoints.Restore(ctx, &checkpointsv1.RestoreRequest{SessionId: r.session, Checkpoint: checkpoint, File: file, All: all})
		lines := []string{}
		if err == nil {
			lines = server.Report(response)
			if file == "" && (len(response.GetRestored()) > 0 || !response.GetSafe()) {
				lines = append(lines, r.takeBack())
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return r.fail(hub, "rollback failed: "+tui.Reason(err))
			}
			page := cards.NewPage(r.kit.Env, cards.PageSpec{
				Title: "Rollback",
				Key:   "checkpoints:restored:" + r.session,
				Lines: func(w int) []string {
					var out []string
					for _, line := range lines {
						if line != "" {
							out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
						}
					}
					return out
				},
			})
			notice := wm.Notify("rollback restored "+cmp.Or(file, "all files")+": "+cmp.Or(response.GetReason(), response.GetRestoredTo()), wm.Success)
			if hub != nil {
				return tea.Batch(hub.Push(page), hub.Done("Restored to "+response.GetRestoredTo()), r.load(), notice)
			}
			return tea.Batch(r.kit.WM.ShowOverlay(r.kit.Hub(page.Key(), page)), notice)
		}
	})
}

// Takes back the session's last turn after a restore of its whole project: what became of it, as a line
func (r *rollback) takeBack() string {
	ctx := r.kit.Env.Ctx
	got, err := r.sessions.Get(ctx, &sessionsv1.GetRequest{Id: r.session})
	if err != nil {
		return "The conversation was kept: " + tui.Reason(err)
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
		return "The conversation was kept: " + tui.Reason(err)
	}
	return fmt.Sprintf("↶ Took back the last turn (%s), so the agent doesn't remember the changes rolled back.", render.Count(int(edited.GetRemoved()), "message"))
}

// Opens the snapshots of the profile in use: what they take by project, and the cleanups
func (c *clients) storage(kit *tui.Kit, _ string) tea.Cmd {
	return (&storage{clients: c, kit: kit}).open()
}

// Store of the profile in use in a list of its projects
type storage struct {
	*clients
	kit    *tui.Kit
	hub    *cards.Hub
	list   *cards.List
	state  *checkpointsv1.StatusResponse
	loaded bool
	err    error
}

func (s *storage) projects() []*checkpointsv1.Project {
	projects := slices.Clone(s.state.GetProjects())
	slices.SortStableFunc(projects, func(a, b *checkpointsv1.Project) int { return b.GetTouched().AsTime().Compare(a.GetTouched().AsTime()) })
	return projects
}

func (s *storage) open() tea.Cmd {
	t := s.kit.Env.Theme
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "File snapshots",
		Glyph:   t.Glyphs.Jobs,
		Key:     "checkpoints:storage",
		Context: storageContext,
		Columns: []render.Column{{Title: "Project", Flex: 3}, {Title: "Snapshots"}, {Title: "Last"}, {Title: "State"}},
		Yield:   []int{2},
		Rows: func() [][]string {
			projects := s.projects()
			rows := make([][]string, len(projects))
			for i, project := range projects {
				state := render.Good("live", t)
				if !project.GetExists() {
					state = render.Warm("orphan", t)
				}
				rows[i] = []string{cmp.Or(project.GetWorkdir(), "(unknown)"), fmt.Sprint(project.GetCommits()),
					render.Elapsed(timeSince(project)) + " ago", state}
			}
			return rows
		},
		Banner: func() string {
			if !s.loaded || s.err != nil {
				return ""
			}
			line := fmt.Sprintf("%s in all · store %s · archives %s (%d) · %s", render.Bytes(s.state.GetTotalBytes()), render.Bytes(s.state.GetStoreBytes()),
				render.Bytes(s.state.GetLegacyBytes()), len(s.state.GetArchives()), s.state.GetBase())
			if notice := s.state.GetFootprint(); notice != "" {
				return render.Warm(notice, t) + "\n" + render.Dim(line, t)
			}
			return render.Dim(line, t)
		},
		Empty: func() string { return tui.Waiting(s.loaded, s.err, "no file snapshots yet") },
		Free:  map[string]bool{"checkpoints.prune": true, "checkpoints.clear.legacy": true, "checkpoints.clear": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "checkpoints.prune", Label: "prune"}, {Action: "checkpoints.clear.legacy", Label: "clear archives"},
				{Action: "checkpoints.clear", Label: "clear all"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"checkpoints.prune": func(l *cards.List, _ int) tea.Cmd { return s.prune(l) },
			"checkpoints.clear.legacy": func(l *cards.List, _ int) tea.Cmd {
				l.Confirm("Delete the old snapshot archives?", func() tea.Cmd {
					s.hub.Busy("Clearing the archives…")
					return tui.Call(s.hub, func() (*checkpointsv1.ClearLegacyResponse, error) {
						return s.checkpoints.ClearLegacy(s.kit.Env.Ctx, &checkpointsv1.ClearLegacyRequest{})
					}, func(cleared *checkpointsv1.ClearLegacyResponse) tea.Cmd {
						text := fmt.Sprintf("Deleted %s, reclaimed %s.", render.Count(int(cleared.GetDeleted()), "archive"), render.Bytes(cleared.GetFreed()))
						if cleared.GetErrors() > 0 {
							return tea.Batch(s.hub.Fail(text+fmt.Sprintf(" Could not delete %d (see logs).", cleared.GetErrors())), s.load())
						}
						return tea.Batch(s.hub.Done(text), s.load())
					})
				})
				return nil
			},
			"checkpoints.clear": func(l *cards.List, _ int) tea.Cmd {
				l.Confirm("Delete every file snapshot? /rollback cannot undo anything after this.", func() tea.Cmd {
					s.hub.Busy("Clearing every snapshot…")
					return tui.Call(s.hub, func() (*checkpointsv1.ClearResponse, error) {
						return s.checkpoints.Clear(s.kit.Env.Ctx, &checkpointsv1.ClearRequest{})
					}, func(cleared *checkpointsv1.ClearResponse) tea.Cmd {
						return tea.Batch(s.hub.Done("Cleared. Reclaimed "+render.Bytes(cleared.GetFreed())+"."), s.load())
					})
				})
				return nil
			},
		},
	})
	s.hub = s.kit.Hub("checkpoints:storage", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

func timeSince(project *checkpointsv1.Project) time.Duration {
	if project.GetTouched() == nil {
		return 0
	}
	return time.Since(project.GetTouched().AsTime()).Round(time.Second)
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

// Form of a cleanup: how old a project may get, the size the store is brought within and whether projects whose folder is gone go; the
// orphans shown now are the only ones it deletes
func (s *storage) prune(l *cards.List) tea.Cmd {
	whole := func(value string) string {
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err != nil || n < 0 {
			return "needs a number"
		}
		return ""
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: "Prune file snapshots",
		Intro: []string{"Deletes the snapshots of projects not snapshotted for a while, and of projects whose folder is gone, then reclaims space."},
		Fields: []cards.Field{
			{ID: "days", Label: "Untouched for days", Kind: cards.FieldNumber, Value: "7", Required: true, Check: whole,
				Help: "Projects not snapshotted for this many days go"},
			{ID: "size", Label: "Store size in MB", Kind: cards.FieldNumber, Value: "500", Required: true, Check: whole,
				Help: "The oldest snapshots of each project go until the store fits"},
			{ID: "orphans", Label: "Projects that are gone", Kind: cards.FieldBool, Value: "true",
				Help: "A folder can be unreachable because it was deleted, or because its volume, share or VPN is down"},
		},
		Submit: "Prune",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			days, _ := strconv.Atoi(strings.TrimSpace(values["days"]))
			size, _ := strconv.Atoi(strings.TrimSpace(values["size"]))
			request := &checkpointsv1.PruneRequest{RetentionDays: int32(days), MaxSizeMb: int32(size), Orphans: values["orphans"] == "true", AllowListed: true}
			for _, project := range s.state.GetProjects() {
				if !project.GetExists() {
					request.Allow = append(request.Allow, project.GetHash())
				}
			}
			return tea.Batch(s.hub.Close(form), tui.Async(func() func() tea.Cmd {
				return func() tea.Cmd {
					question := fmt.Sprintf("Delete snapshots untouched for %d days", days)
					if request.Orphans && len(request.Allow) > 0 {
						question += fmt.Sprintf(" and %s whose folder is gone", render.Count(len(request.Allow), "project"))
					}
					l.Confirm(question+"?", func() tea.Cmd {
						s.hub.Busy("Pruning file snapshots…")
						return tui.Call(s.hub, func() (*checkpointsv1.PruneResponse, error) {
							return s.checkpoints.Prune(s.kit.Env.Ctx, request)
						}, func(pruned *checkpointsv1.PruneResponse) tea.Cmd {
							text := fmt.Sprintf("Deleted %d orphan and %d stale, reclaimed %s.", pruned.GetOrphans(), pruned.GetStale(), render.Bytes(pruned.GetFreed()))
							if pruned.GetErrors() > 0 {
								return tea.Batch(s.hub.Fail(text+fmt.Sprintf(" %d error(s), see logs.", pruned.GetErrors())), s.load())
							}
							return tea.Batch(s.hub.Done(text), s.load())
						})
					})
					return nil
				}
			}))
		},
	})
	return s.hub.Push(form)
}
