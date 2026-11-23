package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Prompts the history keeps
const historyMax = 1000

// Text of pasted chips the whole history keeps at most, the newest first, older chips going back to plain labels
const historyChipBudget = 4 << 20

// File of the prompt history of a profile, one entry per line
const historyFile = "input_history.jsonl"

// Prompt history of the profile every composer recalls, read once and rewritten on each send
type inputHistory struct {
	mu      sync.Mutex
	dir     string
	entries []cards.Entry
	loaded  bool
}

// History of the active profile home
func newInputHistory(ctx context.Context) *inputHistory {
	return &inputHistory{dir: home.Dir(ctx)}
}

// Entries oldest first, the file read on first use; a line that is no entry is skipped
func (h *inputHistory) load() []cards.Entry {
	if h.loaded {
		return h.entries
	}
	h.loaded = true
	data, err := os.ReadFile(filepath.Join(h.dir, historyFile))
	if err != nil {
		return nil
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scan.Scan() {
		var e cards.Entry
		if json.Unmarshal(scan.Bytes(), &e) == nil && strings.TrimSpace(e.Text) != "" {
			h.entries = append(h.entries, e)
		}
	}
	if len(h.entries) > historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
	}
	return h.entries
}

// Entries with one more at the end, an older entry of the same text giving way to it
func withEntry(entries []cards.Entry, e cards.Entry) []cards.Entry {
	entries = slices.DeleteFunc(entries, func(old cards.Entry) bool { return old.Text == e.Text })
	return append(entries, e)
}

// Writes the history file whole, chips past the budget dropped from the oldest entries, through a temporary file so a crash never halves it
func writeHistory(dir string, entries []cards.Entry) error {
	budget := historyChipBudget
	kept := make([]cards.Entry, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var chips []cards.Chip
		for _, c := range e.Chips {
			if budget -= len(c.Text); budget >= 0 {
				chips = append(chips, c)
			}
		}
		e.Chips = chips
		kept[i] = e
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range kept {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, historyFile)
	if err := os.WriteFile(path+".tmp", buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Prompt history every composer of the window shares, read live at each recall
type composerHistory struct {
	file *inputHistory
}

// Shared history over the file of the profile
func newComposerHistory(file *inputHistory) *composerHistory { return &composerHistory{file: file} }

var _ cards.History = (*composerHistory)(nil)

// Entries oldest first
func (c *composerHistory) Entries() []cards.Entry {
	c.file.mu.Lock()
	defer c.file.mu.Unlock()
	return slices.Clone(c.file.load())
}

// Records a sent prompt, an older entry with the same text giving way to it, and rewrites the file; a prompt that is blank is left out
func (c *composerHistory) Add(e cards.Entry) {
	e.Text = strings.TrimFunc(e.Text, isJSSpace)
	if e.Text == "" {
		return
	}
	c.file.mu.Lock()
	defer c.file.mu.Unlock()
	entries := withEntry(slices.Clone(c.file.load()), e)
	if len(entries) > historyMax {
		entries = entries[len(entries)-historyMax:]
	}
	c.file.entries = entries
	_ = writeHistory(c.file.dir, entries)
}

// Wires a composer of this session to the shared history, the drafts kept per session and the chips of pasted text and attached images
func (a *app) bindComposer(c *cards.Composer) {
	c.History = a.inputs
	c.SetDraftDir(home.Dir(a.ctx))
	c.Chips = a.chipsOf
	c.OnRecall = a.recallChips
}

// Chips of the pastes and images whose labels a prompt text shows
func (a *app) chipsOf(text string) []cards.Chip {
	var out []cards.Chip
	for _, t := range a.tokens {
		if t.Label != "" && strings.Contains(text, t.Label) {
			out = append(out, cards.Chip{Kind: t.Kind, Label: t.Label, Text: t.Text, Path: t.Path, Mime: t.Mime})
		}
	}
	return out
}

// Takes back the chips of a prompt put into the composer from the history or a kept draft, an image whose staged file is gone left as its label
func (a *app) recallChips(e cards.Entry) {
	for _, chip := range e.Chips {
		if chip.Kind == "image" {
			if _, err := os.Stat(chip.Path); err != nil {
				continue
			}
		}
		known := slices.ContainsFunc(a.tokens, func(t composerToken) bool {
			return t.Label == chip.Label && t.Text == chip.Text && t.Path == chip.Path
		})
		if known {
			continue
		}
		token := composerToken{Kind: chip.Kind, Label: chip.Label, Text: chip.Text, Path: chip.Path, Mime: chip.Mime}
		if chip.Kind == "image" {
			token.Index = nextImageIndex(a.tokens)
		}
		a.tokens = trimTokens(append(a.tokens, token))
	}
}
