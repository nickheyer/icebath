package config

import (
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	PromptStashPreviewWidth = 60
	MaxStashItems           = 20

	ActionNoop       = "noop"
	ActionStashed    = "stashed"
	ActionRestored   = "restored"
	ActionOpenPanel  = "open_panel"
	ActionClosePanel = "close_panel"
)

var stashClockStart = time.Now()

// Collapses a multi line draft into one preview line ellipsized to width runes
func BuildPreview(text string, width int) string {
	if text == "" {
		return ""
	}
	flat := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	flat = strings.ReplaceAll(strings.ReplaceAll(flat, "\n", " ⏎ "), "\t", " ")
	flat = strings.Join(pystr.SplitWhitespace(flat, -1), " ")
	if runes := []rune(flat); width > 1 && len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return flat
}

// One parked draft with its exact text and attached images
type StashEntry struct {
	Text      string
	Images    []any
	StashedAt float64
	Preview   string
}

// Row the stash panel renderer consumes, images copied
func (e StashEntry) AsDict() map[string]any {
	return map[string]any{"text": e.Text, "images": append([]any{}, e.Images...), "stashed_at": e.StashedAt, "preview": e.Preview}
}

// Session scoped newest first stack of parked composer drafts
type PromptStash struct {
	items       []StashEntry
	maxItems    int
	clock       func() float64
	PanelOpen   bool
	PanelCursor int
}

// Stash capped at the default item count on the monotonic clock
func NewPromptStash() *PromptStash { return NewPromptStashWith(MaxStashItems, nil) }

// Stash capped at maxItems, at least one, stamping entries with clock
func NewPromptStashWith(maxItems int, clock func() float64) *PromptStash {
	if clock == nil {
		clock = func() float64 { return time.Since(stashClockStart).Seconds() }
	}
	return &PromptStash{maxItems: max(1, maxItems), clock: clock}
}

func (s *PromptStash) Len() int { return len(s.items) }

// Newest first copy of the entries
func (s *PromptStash) Items() []StashEntry { return append([]StashEntry{}, s.items...) }

func (s *PromptStash) PanelRows() []map[string]any {
	rows := make([]map[string]any, len(s.items))
	for i, e := range s.items {
		rows[i] = e.AsDict()
	}
	return rows
}

// Status bar pin with the count, an up arrow while browsing, empty when nothing is stashed
func (s *PromptStash) Indicator() string {
	switch n := len(s.items); {
	case n == 0:
		return ""
	case s.PanelOpen:
		return "📌 " + strconv.Itoa(n) + " ▲"
	default:
		return "📌 " + strconv.Itoa(n)
	}
}

// Composer hint advertising the stashed drafts
func (s *PromptStash) ComposerHint() string {
	switch n := len(s.items); n {
	case 0:
		return ""
	case 1:
		return "Ctrl+S to restore: " + s.items[0].Preview
	default:
		return "Ctrl+S to browse " + strconv.Itoa(n) + " stashed drafts"
	}
}

// Pushes a draft, false for a blank buffer with no images, the oldest dropped past the cap
func (s *PromptStash) Stash(text string, images []any) bool {
	if pystr.Strip(text) == "" && len(images) == 0 {
		return false
	}
	preview := BuildPreview(text, PromptStashPreviewWidth)
	if preview == "" {
		preview = "(images only)"
	}
	entry := StashEntry{Text: text, Images: append([]any{}, images...), StashedAt: s.clock(), Preview: preview}
	s.items = append([]StashEntry{entry}, s.items...)
	if len(s.items) > s.maxItems {
		s.items = s.items[:s.maxItems]
	}
	s.ClosePanel()
	return true
}

// Removes and returns the text and images at index, ok false when out of range
func (s *PromptStash) Pop(index int) (string, []any, bool) {
	if index < 0 || index >= len(s.items) {
		return "", nil, false
	}
	entry := s.items[index]
	s.items = append(s.items[:index:index], s.items[index+1:]...)
	if len(s.items) == 0 {
		s.PanelOpen = false
	}
	s.PanelCursor = s.clampCursor(s.PanelCursor)
	return entry.Text, append([]any{}, entry.Images...), true
}

func (s *PromptStash) Peek(index int) (StashEntry, bool) {
	if index < 0 || index >= len(s.items) {
		return StashEntry{}, false
	}
	return s.items[index], true
}

func (s *PromptStash) Clear() {
	s.items = nil
	s.ClosePanel()
}

func (s *PromptStash) clampCursor(value int) int {
	if len(s.items) == 0 {
		return 0
	}
	return max(0, min(value, len(s.items)-1))
}

// Opens the browse panel at the top, false when nothing is stashed
func (s *PromptStash) OpenPanel() bool {
	if len(s.items) == 0 {
		return false
	}
	s.PanelOpen = true
	s.PanelCursor = 0
	return true
}

func (s *PromptStash) ClosePanel() {
	s.PanelOpen = false
	s.PanelCursor = 0
}

// Moves the panel cursor by delta clamped to the list bounds
func (s *PromptStash) MoveCursor(delta int) int {
	s.PanelCursor = s.clampCursor(s.PanelCursor + delta)
	return s.PanelCursor
}

// Deletes the highlighted entry, closing the panel once empty, false when nothing was stashed
func (s *PromptStash) DeleteAtCursor() bool {
	if len(s.items) == 0 {
		return false
	}
	idx := s.clampCursor(s.PanelCursor)
	s.items = append(s.items[:idx:idx], s.items[idx+1:]...)
	if len(s.items) == 0 {
		s.ClosePanel()
	} else {
		s.PanelCursor = s.clampCursor(idx)
	}
	return true
}

// Pops the highlighted entry and closes the panel
func (s *PromptStash) RestoreAtCursor() (string, []any, bool) {
	if len(s.items) == 0 {
		return "", nil, false
	}
	text, images, ok := s.Pop(s.clampCursor(s.PanelCursor))
	s.ClosePanel()
	return text, images, ok
}

// Restored draft of a ctrl s press
type StashPayload struct {
	Text   string
	Images []any
}

// Action one ctrl s press takes, with the restored draft for a restore
func ResolveCtrlS(s *PromptStash, bufferText string, images []any) (string, *StashPayload) {
	if s.PanelOpen {
		s.ClosePanel()
		return ActionClosePanel, nil
	}
	if pystr.Strip(bufferText) != "" || len(images) > 0 {
		if s.Stash(bufferText, images) {
			return ActionStashed, nil
		}
		return ActionNoop, nil
	}
	switch len(s.items) {
	case 0:
		return ActionNoop, nil
	case 1:
		text, imgs, _ := s.Pop(0)
		return ActionRestored, &StashPayload{Text: text, Images: imgs}
	}
	s.OpenPanel()
	return ActionOpenPanel, nil
}
