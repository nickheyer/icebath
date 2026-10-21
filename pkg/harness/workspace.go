package harness

import (
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// What happened to one file during the session
type Touch struct {
	Path   string
	Reads  int
	Writes int
	Edits  int
	// Last time any tool touched the file
	Last time.Time
	// Agent that touched it last
	Agent string
	// Most recent change with the whole file before and after
	Change *files.Change
	// First content seen before any change, so a diff spans the whole session
	Original *string
	// Call id of the last change
	CallID string
}

// Files the tools touched and the directory the shell sits in
type Workspace struct {
	mu       sync.Mutex
	files    map[string]*Touch
	dir      string
	searches int
	Clock    func() time.Time
}

func NewWorkspace(dir string) *Workspace {
	return &Workspace{files: map[string]*Touch{}, dir: dir, Clock: time.Now}
}

// Updates the record from a tool end event by the structured data its result carries
func (w *Workspace) Apply(ev agent.Event) {
	if ev.Type != agent.EventToolEnd || ev.Data == nil {
		return
	}
	now := ev.Time
	if now.IsZero() {
		now = w.Clock()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch d := ev.Data.(type) {
	case files.ReadData:
		t := w.touch(d.Path, now, ev.Agent)
		t.Reads++
	case files.Change:
		t := w.touch(d.Path, now, ev.Agent)
		if t.Original == nil {
			old := d.Old
			t.Original = &old
		}
		change := d
		t.Change = &change
		t.CallID = ev.Call.ID
		if ev.Call.Name == files.EditName {
			t.Edits++
		} else {
			t.Writes++
		}
	case files.GlobData, files.GrepData:
		w.searches++
	case shell.Run:
		if d.Dir != "" {
			w.dir = d.Dir
		}
	}
}

func (w *Workspace) touch(path string, now time.Time, agent string) *Touch {
	t, ok := w.files[path]
	if !ok {
		t = &Touch{Path: path}
		w.files[path] = t
	}
	t.Last, t.Agent = now, agent
	return t
}

// Files touched, most recent first
func (w *Workspace) Files() []Touch {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Touch, 0, len(w.files))
	for _, t := range w.files {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Files that were written or edited, most recent first
func (w *Workspace) Changed() []Touch {
	var out []Touch
	for _, t := range w.Files() {
		if t.Change != nil {
			out = append(out, t)
		}
	}
	return out
}

func (w *Workspace) Get(path string) (Touch, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.files[path]
	if !ok {
		return Touch{}, false
	}
	return *t, true
}

// Directory the shell will run the next command in
func (w *Workspace) Dir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dir
}

func (w *Workspace) SetDir(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dir = dir
}

// Glob and Grep calls run so far
func (w *Workspace) Searches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.searches
}
