package tui

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the screen of commands that run after exit
const afterExitContext = "tui.afterexit"

// One icebath command held until the terminal UI exits, with why it cannot run inside and when it was queued
type afterExitCommand struct {
	Argv   []string  `json:"argv"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Commands of one window, kept in a file of the home so a crash leaves them to be found and two windows never run each other's
type afterExitQueue struct {
	PID      int                `json:"pid"`
	Commands []afterExitCommand `json:"commands"`
}

// Serialises the reads and writes of this window's queue file
var afterExitMu sync.Mutex

// Folder of the queue files of every window of the home
func afterExitDir() string { return filepath.Join(home.Root(), "after-exit") }

// Queue file of the window running as the process
func afterExitPath(pid int) string {
	return filepath.Join(afterExitDir(), "window-"+strconv.Itoa(pid)+".json")
}

// Queue in a file, an empty queue for a file that does not exist
func readAfterExit(path string) (afterExitQueue, error) {
	var q afterExitQueue
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return q, err
	}
	return q, json.Unmarshal(data, &q)
}

// Writes the queue to its file, removing the file once the queue is empty
func writeAfterExit(path string, q afterExitQueue) error {
	if len(q.Commands) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Commands this window holds to run after it exits
func afterExitQueued() ([]afterExitCommand, error) {
	afterExitMu.Lock()
	defer afterExitMu.Unlock()
	q, err := readAfterExit(afterExitPath(os.Getpid()))
	return q.Commands, err
}

// Adds a command to this window's queue
func queueAfterExit(argv []string, reason string) error {
	afterExitMu.Lock()
	defer afterExitMu.Unlock()
	path := afterExitPath(os.Getpid())
	q, err := readAfterExit(path)
	if err != nil {
		return err
	}
	q.PID = os.Getpid()
	q.Commands = append(q.Commands, afterExitCommand{Argv: argv, Reason: reason, At: time.Now()})
	return writeAfterExit(path, q)
}

// Takes the command at i out of this window's queue
func dropAfterExit(i int) error {
	afterExitMu.Lock()
	defer afterExitMu.Unlock()
	path := afterExitPath(os.Getpid())
	q, err := readAfterExit(path)
	if err != nil {
		return err
	}
	if i < 0 || i >= len(q.Commands) {
		return nil
	}
	q.Commands = slices.Delete(q.Commands, i, i+1)
	return writeAfterExit(path, q)
}

// TakeAfterExit returns the commands this window queued to run once the terminal UI has exited and empties its queue
func TakeAfterExit() ([][]string, error) {
	afterExitMu.Lock()
	defer afterExitMu.Unlock()
	path := afterExitPath(os.Getpid())
	q, err := readAfterExit(path)
	if err != nil {
		return nil, err
	}
	out := make([][]string, len(q.Commands))
	for i, c := range q.Commands {
		out[i] = c.Argv
	}
	return out, writeAfterExit(path, afterExitQueue{})
}

// Whether a process of the pid still runs
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return runtime.GOOS == "windows" || p.Signal(syscall.Signal(0)) == nil
}

// Queues of windows of the home that closed without running them, oldest first, with the files that could not be read
func orphanedAfterExit() ([]afterExitQueue, []string, error) {
	entries, err := os.ReadDir(afterExitDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []afterExitQueue
	var unread []string
	for _, e := range entries {
		name := e.Name()
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "window-"), ".json"))
		if err != nil || !strings.HasSuffix(name, ".json") || pid == os.Getpid() || processAlive(pid) {
			continue
		}
		q, err := readAfterExit(filepath.Join(afterExitDir(), name))
		if err != nil {
			unread = append(unread, name)
			continue
		}
		q.PID = pid
		if len(q.Commands) > 0 {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Commands[0].At.Before(out[j].Commands[0].At) })
	return out, unread, nil
}

// Moves one command of a closed window's queue into this window's queue, or drops it when keep is false
func settleOrphan(pid, i int, keep bool) error {
	afterExitMu.Lock()
	path := afterExitPath(pid)
	q, err := readAfterExit(path)
	if err != nil || i < 0 || i >= len(q.Commands) {
		afterExitMu.Unlock()
		return err
	}
	c := q.Commands[i]
	q.Commands = slices.Delete(q.Commands, i, i+1)
	err = writeAfterExit(path, q)
	afterExitMu.Unlock()
	if err != nil || !keep {
		return err
	}
	return queueAfterExit(c.Argv, c.Reason)
}

// Queues a command for after exit and says so with where to see the queue
func (a *app) queueAfterExitCmd(argv []string, reason string) tea.Cmd {
	if err := queueAfterExit(argv, reason); err != nil {
		return wm.Notify(tr(txt.AfterExitQueueFailed, err.Error()), wm.Error)
	}
	return wm.Notify(tr(txt.AfterExitQueued, "icebath "+joinCommandLine(argv)), wm.Info)
}

// Keys of the screen of commands that run after exit
func registerAfterExitKeys(km *keys.Map) {
	if keys.ContextDocs[afterExitContext] == "" {
		if err := keys.AddContext(afterExitContext, "commands that run after exit"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.afterexit.quit", Context: afterExitContext, Label: "quit and run", Doc: "quit icebath now so the queued commands run", Default: []string{"Q"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "afterexit", order: 95, title: txt.HubsTitleAfterExit, short: txt.HubsShortAfterExit, slash: "after-exit",
		open: func(a *app, _ string) tea.Cmd { return a.openAfterExit() }, keys: registerAfterExitKeys})
}

// One row of the after exit screen, a command of this window or of a window that closed without running it
type afterExitRow struct {
	cmd afterExitCommand
	// Process of the closed window the command was left by, zero for this window, and its place in that window's queue
	orphan int
	at     int
}

// Screen of the commands this window runs after it exits, and of those windows that closed unexpectedly left behind
func (a *app) openAfterExit() tea.Cmd {
	var rows []afterExitRow
	var h *cards.Hub
	// Queue files of closed windows that could not be read, named once above the rows and otherwise left alone
	var unread []string
	load := func() error {
		rows = nil
		own, err := afterExitQueued()
		if err != nil {
			return err
		}
		for i, c := range own {
			rows = append(rows, afterExitRow{cmd: c, at: i})
		}
		orphans, skipped, err := orphanedAfterExit()
		if err != nil {
			return err
		}
		unread = skipped
		for _, q := range orphans {
			for i, c := range q.Commands {
				rows = append(rows, afterExitRow{cmd: c, orphan: q.PID, at: i})
			}
		}
		return nil
	}
	settle := func(err error, done string) tea.Cmd {
		if err == nil {
			err = load()
		}
		if err != nil {
			return h.Fail(err.Error())
		}
		return h.Done(done)
	}
	at := func(i int) (afterExitRow, bool) {
		if i < 0 || i >= len(rows) {
			return afterExitRow{}, false
		}
		return rows[i], true
	}
	remove := func(l *cards.List, i int) tea.Cmd {
		r, ok := at(i)
		if !ok {
			return nil
		}
		line := "icebath " + joinCommandLine(r.cmd.Argv)
		l.Confirm(tr(txt.AfterExitRemoveConfirm, line), func() tea.Cmd {
			if r.orphan != 0 {
				return settle(settleOrphan(r.orphan, r.at, false), tr(txt.AfterExitRemoved, line))
			}
			return settle(dropAfterExit(r.at), tr(txt.AfterExitRemoved, line))
		})
		return nil
	}
	list := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleAfterExit), Glyph: a.toolkit.Glyphs.Jobs, Key: "tui:afterexit", Context: afterExitContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColCommand), Flex: 2, Max: 60}, {Title: tr(txt.AfterExitColWhy), Flex: 3}, {Title: tr(txt.HubsColWhen), Right: true}},
		Yield:   []int{2, 1},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				why := r.cmd.Reason
				if r.orphan != 0 {
					why = tr(txt.AfterExitFromClosedWindow)
				}
				out[i] = []string{"icebath " + joinCommandLine(r.cmd.Argv), why, whenText(float64(r.cmd.At.Unix()))}
			}
			return out
		},
		Detail: func(i int) []string {
			r, ok := at(i)
			if !ok {
				return nil
			}
			out := []string{"icebath " + joinCommandLine(r.cmd.Argv), r.cmd.Reason}
			if r.orphan != 0 {
				out = append(out, "", tr(txt.AfterExitOrphanDetail))
			}
			return out
		},
		Status: func() string {
			if len(rows) == 0 {
				return ""
			}
			return tr(txt.AfterExitStatus)
		},
		Empty: func() string { return tr(txt.AfterExitEmpty) },
		Banner: func() string {
			if len(unread) == 0 {
				return ""
			}
			return tr(txt.AfterExitUnread, strings.Join(unread, ", "))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if r, ok := at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, wm.Hint{Action: "hub.remove"})
				if r.orphan == 0 {
					out = append(out, wm.Hint{Action: "tui.afterexit.quit"})
				}
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "tui.afterexit.quit": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.remove": remove,
			"hub.add":    func(*cards.List, int) tea.Cmd { return a.runCommand() },
			"tui.afterexit.quit": func(*cards.List, int) tea.Cmd {
				if len(rows) == 0 {
					return nil
				}
				return a.die(0)
			},
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				r, ok := at(i)
				if !ok {
					return nil
				}
				line := "icebath " + joinCommandLine(r.cmd.Argv)
				detail := func() []string { return []string{line, r.cmd.Reason} }
				return a.hubActions(h, line, detail, func() []hubAction {
					var acts []hubAction
					if r.orphan != 0 {
						acts = append(acts, hubAction{label: tr(txt.AfterExitKeep), does: tr(txt.AfterExitKeepDoes), run: func() tea.Cmd {
							return tea.Batch(h.Pop(), settle(settleOrphan(r.orphan, r.at, true), tr(txt.AfterExitKept, line)))
						}})
					} else {
						acts = append(acts, hubAction{key: "tui.afterexit.quit", label: tr(txt.AfterExitQuitNow), does: tr(txt.AfterExitQuitNowDoes), run: func() tea.Cmd { return a.die(0) }})
					}
					return append(acts, hubAction{key: "hub.remove", label: tr(txt.AfterExitRemove), does: tr(txt.AfterExitRemoveDoes), confirm: tr(txt.AfterExitRemoveConfirm, line), run: func() tea.Cmd {
						if r.orphan != 0 {
							return tea.Batch(h.Pop(), settle(settleOrphan(r.orphan, r.at, false), tr(txt.AfterExitRemoved, line)))
						}
						return tea.Batch(h.Pop(), settle(dropAfterExit(r.at), tr(txt.AfterExitRemoved, line)))
					}})
				})
			},
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			if err := load(); err != nil {
				return h.Fail(err.Error())
			}
			l.Refresh()
			return nil
		},
	})
	h = a.newHub("tui:afterexit", list)
	if err := load(); err != nil {
		return tea.Batch(a.wm.ShowOverlay(h), h.Fail(err.Error()))
	}
	return a.wm.ShowOverlay(h)
}
