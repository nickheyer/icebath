package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Directory under the sessions directory holding one file per sprint
const journalDir = "sprints"

// Delay a write waits for more changes, so a burst costs one write
const journalDelay = 400 * time.Millisecond

// Sprint as its file holds it
type record struct {
	ID       string                    `json:"id"`
	Project  string                    `json:"project"`
	Request  string                    `json:"request"`
	Started  time.Time                 `json:"started"`
	Finished time.Time                 `json:"finished,omitempty"`
	Stage    Stage                     `json:"stage"`
	Last     Stage                     `json:"last,omitempty"`
	Status   string                    `json:"status,omitempty"`
	Err      string                    `json:"err,omitempty"`
	Stopped  bool                      `json:"stopped,omitempty"`
	Brief    Brief                     `json:"brief"`
	Plan     Plan                      `json:"plan"`
	Tickets  []Ticket                  `json:"tickets,omitempty"`
	Members  []memberRecord            `json:"members,omitempty"`
	Messages []Message                 `json:"messages,omitempty"`
	Summary  string                    `json:"summary,omitempty"`
	Cause    string                    `json:"cause,omitempty"`
	Spent    map[string]int            `json:"spent,omitempty"`
	Usage    map[string]protocol.Usage `json:"usage,omitempty"`
}

// Member as its file holds it, the session by id
type memberRecord struct {
	Role    Role      `json:"role"`
	Name    string    `json:"name"`
	Index   int       `json:"index"`
	Session string    `json:"session,omitempty"`
	Joined  time.Time `json:"joined"`
	Ticket  string    `json:"ticket,omitempty"`
	Task    string    `json:"task,omitempty"`
}

// File of one sprint, written after changes go quiet and at once when the sprint ends
type journal struct {
	path     string
	project  string
	announce func(any)
	mu       sync.Mutex
	timer    *time.Timer
	// Last write failure told to the person, so the same one is told once
	told string
}

// Directory the sprint files of the app live in
func journalPath(app *harness.App) (string, error) {
	sessions, err := app.Config.SessionsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(sessions, journalDir), nil
}

// Journal of a sprint under the directory, announcing write failures through the app
func newJournal(dir, id, project string, announce func(any)) *journal {
	if announce == nil {
		announce = func(any) {}
	}
	return &journal{path: filepath.Join(dir, id+".json"), project: project, announce: announce}
}

// Schedules a write once changes go quiet
func (j *journal) touch(s *Sprint) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.timer != nil {
		return
	}
	j.timer = time.AfterFunc(journalDelay, func() {
		j.mu.Lock()
		j.timer = nil
		j.mu.Unlock()
		j.write(s)
	})
}

// Writes the sprint now, cancelling a pending write
func (j *journal) flush(s *Sprint) {
	j.mu.Lock()
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.mu.Unlock()
	j.write(s)
}

// Writes the snapshot through a temporary file, telling the person once when it fails
func (j *journal) write(s *Sprint) {
	err := writeRecord(j.path, toRecord(s.Snapshot(), j.project))
	j.mu.Lock()
	defer j.mu.Unlock()
	if err == nil {
		j.told = ""
		return
	}
	if text := err.Error(); text != j.told {
		j.told = text
		j.announce(harness.NoticeMsg{Level: harness.Error, Text: s.ID + " could not be saved: " + text})
	}
}

// Removes the file of the sprint
func (j *journal) remove() error {
	j.mu.Lock()
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.mu.Unlock()
	if err := os.Remove(j.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Record of a snapshot for the project
func toRecord(snap Snapshot, project string) record {
	r := record{ID: snap.ID, Project: project, Request: snap.Request, Started: snap.Started, Finished: snap.Finished, Stage: snap.Stage, Last: snap.Last, Status: snap.Status, Brief: snap.Brief, Plan: snap.Plan, Tickets: snap.Tickets, Messages: snap.Messages, Summary: snap.Summary, Cause: snap.Cause, Spent: snap.Spent, Usage: snap.Usage}
	if snap.Err != nil {
		r.Err, r.Stopped = snap.Err.Error(), snap.Stopped()
	}
	for _, m := range snap.Members {
		r.Members = append(r.Members, memberRecord{Role: m.Role, Name: m.Name, Index: m.Index, Session: m.SessionID(), Joined: m.Joined, Ticket: m.Ticket, Task: m.Task})
	}
	return r
}

// Writes a record as JSON through a synced temporary file renamed over the target
func writeRecord(path string, r record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Reads one sprint file
func readRecord(path string) (record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return record{}, fmt.Errorf("%s: %w", path, err)
	}
	if r.ID == "" {
		return record{}, fmt.Errorf("%s: no sprint id", path)
	}
	return r, nil
}

// Sprint rebuilt from its record, one the process ended under marked interrupted at the time of its file
func fromRecord(r record, modified time.Time, dir string, announce func(any)) *Sprint {
	s := newSprint(r.ID, r.Request, r.Started, announce)
	s.mu.Lock()
	s.Finished, s.Stage, s.Last, s.Status = r.Finished, r.Stage, r.Last, r.Status
	s.Brief, s.Plan, s.Summary, s.Cause = r.Brief, r.Plan, r.Summary, r.Cause
	for i := range r.Tickets {
		t := r.Tickets[i]
		s.Tickets = append(s.Tickets, &t)
	}
	for _, m := range r.Members {
		s.Members = append(s.Members, &Member{Role: m.Role, Name: m.Name, Index: m.Index, Session: m.Session, Joined: m.Joined, Ticket: m.Ticket, Task: m.Task})
	}
	s.Messages = r.Messages
	for _, m := range r.Messages {
		s.seq = max(s.seq, m.Seq)
	}
	if r.Spent != nil {
		s.Spent = r.Spent
	}
	if r.Usage != nil {
		s.Usage = r.Usage
	}
	switch {
	case r.Err != "" && r.Stopped:
		s.Err = fmt.Errorf("%w: %s", errStopped, r.Err)
	case r.Err != "":
		s.Err = errors.New(r.Err)
	}
	if r.Finished.IsZero() {
		s.Finished, s.Err, s.Last = modified, errExited, r.Stage
		s.Stage, s.Status = StageFailed, errExited.Error()+", restart to go on from the "+string(r.Stage)+" stage"
		for _, m := range s.Members {
			m.Ticket, m.Task = "", ""
		}
	}
	s.journal = newJournal(dir, r.ID, r.Project, announce)
	s.mu.Unlock()
	return s
}

// Loads every sprint of the project from the directory into the registry, newest file last, reporting how many
func recoverSprints(app *harness.App) (int, error) {
	dir, err := journalPath(app)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	type found struct {
		r        record
		modified time.Time
	}
	var list []found
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		r, err := readRecord(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, err
		}
		if r.Project != app.Root || sprintByID(r.ID) != nil {
			continue
		}
		list = append(list, found{r: r, modified: info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].r.Started.Before(list[j].r.Started) })
	for _, f := range list {
		register(fromRecord(f.r, f.modified, dir, app.Announce))
	}
	return len(list), nil
}

// Id of the next sprint, one past the highest number any sprint file or registered sprint carries
func nextID(app *harness.App) (string, error) {
	dir, err := journalPath(app)
	if err != nil {
		return "", err
	}
	highest := 0
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		highest = max(highest, sprintNumber(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))))
	}
	regMu.Lock()
	for _, s := range sprints {
		highest = max(highest, sprintNumber(s.ID))
	}
	regMu.Unlock()
	return Name + "-" + strconv.Itoa(highest+1), nil
}

// Number of a sprint id such as sprint-12, zero for any other name
func sprintNumber(id string) int {
	rest, ok := strings.CutPrefix(id, Name+"-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Attaches the journal of a sprint started in this process under the directory of the app
func (s *Sprint) keep(ctx context.Context, app *harness.App) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := journalPath(app)
	if err != nil {
		return err
	}
	j := newJournal(dir, s.ID, app.Root, app.Announce)
	s.mu.Lock()
	s.journal = j
	s.mu.Unlock()
	j.flush(s)
	return nil
}

// Removes the file of the sprint, nothing when it has none
func (s *Sprint) unkeep() error {
	s.mu.Lock()
	j := s.journal
	s.mu.Unlock()
	if j == nil {
		return nil
	}
	return j.remove()
}
