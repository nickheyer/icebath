package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const historyMax = 1000

// Prompt history shared with the classic CLI file, loaded once and appended per submit
type inputHistory struct {
	mu    sync.Mutex
	file  string
	cache []string
	now   func() time.Time
}

// History of the active home, the .icebath_history file the classic CLI also keeps
func newInputHistory(ctx context.Context) *inputHistory {
	return &inputHistory{file: filepath.Join(home.Dir(ctx), ".icebath_history"), now: time.Now}
}

// Entries of the history file, the newest thousand, read once and empty when the file is missing or unreadable
func (h *inputHistory) load() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked()
}

func (h *inputHistory) loadLocked() []string {
	if h.cache != nil {
		return h.cache
	}
	h.cache = []string{}
	raw, err := os.ReadFile(h.file)
	if err != nil {
		return h.cache
	}
	var entries, current []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "+") {
			current = append(current, line[1:])
		} else if len(current) > 0 {
			entries = append(entries, strings.Join(current, "\n"))
			current = nil
		}
	}
	if len(current) > 0 {
		entries = append(entries, strings.Join(current, "\n"))
	}
	if len(entries) > historyMax {
		entries = entries[len(entries)-historyMax:]
	}
	h.cache = append(h.cache, entries...)
	return h.cache
}

// Records a submitted prompt unless blank or equal to the newest entry, appending it to the file
func (h *inputHistory) append(line string) {
	trimmed := strings.TrimFunc(line, isJSSpace)
	if trimmed == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	items := h.loadLocked()
	if len(items) > 0 && items[len(items)-1] == trimmed {
		return
	}
	items = append(items, trimmed)
	if len(items) > historyMax {
		items = items[len(items)-historyMax:]
	}
	h.cache = items
	if err := os.MkdirAll(filepath.Dir(h.file), 0o755); err != nil {
		return
	}
	ts := strings.Replace(strings.TrimSuffix(h.now().UTC().Format("2006-01-02T15:04:05.000Z"), "Z"), "T", " ", 1)
	encoded := "+" + strings.ReplaceAll(trimmed, "\n", "\n+")
	f, err := os.OpenFile(h.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# " + ts + "\n" + encoded + "\n")
}
