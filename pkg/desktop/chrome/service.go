package chrome

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Renderer window a chrome call came from
type Sender interface {
	ID() string
	Send(event string, payload any)
	OnDestroyed(f func())
	IsDestroyed() bool
}

// Window whose focused element takes the context menu edit verbs
type Editor interface {
	// Runs Copy, Cut, Paste or SelectAll on the focused element
	EditingCommand(command string)
}

// Window that copies the image at a page point to the clipboard
type ImageCopier interface {
	Sender
	CopyImageAt(x, y int)
}

// Window whose selected misspelling a suggestion replaces
type SpellingEditor interface {
	ReplaceMisspelling(word string)
}

// Window whose webview runs find in page
type Finder interface {
	Sender
	// Starts or steps a find and answers its request id, results arrive through Service.FoundInPage
	FindInPage(query string, forward, findNext bool) (int, error)
	// Ends the find, removing its highlights and clearing the page selection
	StopFindInPage()
}

// Window whose DevTools the inspect shortcuts toggle
type Inspectable interface {
	Sender
	DevToolsOpened() bool
	OpenDevTools()
	CloseDevTools()
}

// Host facts, session dictionary and guest lookup the chrome group reads
type Options struct {
	UserData   string
	Platform   string
	Log        func(string)
	Dictionary Dictionary
	Guests     func(webContentsID float64) Guest
}

// One find result a webview reports for a request
type FoundResult struct {
	RequestID          int
	ActiveMatchOrdinal int
	Matches            int
}

// Match position and count the renderer find bar shows
type FoundInPagePayload struct {
	ActiveMatchOrdinal int `json:"activeMatchOrdinal"`
	Count              int `json:"count"`
}

// Reply of a find request, the count arriving later as found-in-page
type FindAck struct {
	Count int `json:"count"`
}

// Spelling facts of a context menu on an editable misspelling
type ContextMenuSpellcheckPayload struct {
	MisspelledWord string   `json:"misspelledWord"`
	Suggestions    []string `json:"suggestions"`
}

type point struct{ x, y int }

type findWaiter struct {
	requestID int
	known     bool
	done      chan struct{}
}

type windowState struct {
	point      *point
	forwarding bool
	waiters    []*findWaiter
	early      map[int]bool
}

// Context menu, find, spelling and DevTools behaviour of the renderer windows
type Service struct {
	opts Options

	mu          sync.Mutex
	f12Blocked  bool
	f12Active   map[string]bool
	windows     map[string]*windowState
	destroyHook map[string]bool
}

// Chrome group with the persisted F12 setting applied
func NewService(opts Options) *Service {
	s := &Service{opts: opts, f12Active: map[string]bool{}, windows: map[string]*windowState{}, destroyHook: map[string]bool{}}
	s.f12Blocked = s.readPersistedDisableF12()
	return s
}

func (s *Service) disableF12Path() string { return filepath.Join(s.opts.UserData, "disable-f12.json") }

// Persisted Disable F12 DevTools setting, false when unset or unreadable
func (s *Service) readPersistedDisableF12() bool {
	raw, err := os.ReadFile(s.disableF12Path())
	if err != nil {
		return false
	}
	var stored map[string]any
	if json.Unmarshal(raw, &stored) != nil {
		return false
	}
	on, _ := stored["on"].(bool)
	return on
}

// Window state of id, forgotten with the window, the caller holding the lock
func (s *Service) stateLocked(w Sender) *windowState {
	id := w.ID()
	st := s.windows[id]
	if st == nil {
		st = &windowState{early: map[int]bool{}}
		s.windows[id] = st
	}
	if !s.destroyHook[id] {
		s.destroyHook[id] = true
		w.OnDestroyed(func() { s.windowDestroyed(id) })
	}
	return st
}

func (s *Service) windowDestroyed(id string) {
	s.mu.Lock()
	st := s.windows[id]
	delete(s.windows, id)
	delete(s.destroyHook, id)
	delete(s.f12Active, id)
	s.mu.Unlock()
	if st != nil {
		for _, wt := range st.waiters {
			close(wt.done)
		}
	}
}

// Runs an edit verb of the custom context menu on the focused element of the calling window
func (s *Service) ContextMenuEdit(w Editor, command any) {
	switch command {
	case "copy":
		w.EditingCommand("Copy")
	case "cut":
		w.EditingCommand("Cut")
	case "paste":
		w.EditingCommand("Paste")
	case "selectAll":
		w.EditingCommand("SelectAll")
	}
}

// Records the point of a native context menu gesture and forwards its spelling facts on an editable misspelling
func (s *Service) ContextMenuShown(w Sender, x, y int, editable bool, misspelledWord string, suggestions []string) {
	s.mu.Lock()
	s.stateLocked(w).point = &point{x, y}
	s.mu.Unlock()
	if suggestions == nil {
		suggestions = []string{}
	}
	if editable && misspelledWord != "" {
		w.Send("context-menu-spellcheck", ContextMenuSpellcheckPayload{MisspelledWord: misspelledWord, Suggestions: suggestions})
	}
}

// Copies the image under the last context menu gesture of the calling window
func (s *Service) ContextMenuCopyImage(w ImageCopier) {
	s.mu.Lock()
	var p *point
	if st := s.windows[w.ID()]; st != nil {
		p = st.point
	}
	s.mu.Unlock()
	if p != nil {
		w.CopyImageAt(p.x, p.y)
	}
}

// Stores the Disable F12 DevTools setting and applies it to every window at once
func (s *Service) SetDisableF12(on any) {
	blocked := jsTruthy(on)
	s.mu.Lock()
	s.f12Blocked = blocked
	s.mu.Unlock()
	body, _ := json.MarshalIndent(map[string]bool{"on": blocked}, "", "  ")
	err := os.MkdirAll(filepath.Dir(s.disableF12Path()), 0o777)
	if err == nil {
		err = os.WriteFile(s.disableF12Path(), body, 0o666)
	}
	if err != nil {
		s.opts.Log("[disable-f12] write failed: " + hardening.NodeErr(err).Error())
	}
}

// Marks whether a renderer binding of the calling window claims F12
func (s *Service) SetF12ShortcutActive(w Sender, active any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateLocked(w)
	if jsTruthy(active) {
		s.f12Active[w.ID()] = true
	} else {
		delete(s.f12Active, w.ID())
	}
}

// Handles a key before the page sees it, F12 and the inspect chord, and Ctrl+F on Linux, true when the page must not get it
func (s *Service) BeforeInputEvent(w Inspectable, input KeyInput) bool {
	isMac := s.opts.Platform == "darwin"
	prevent := false
	if input.Type == "keyDown" {
		handled := false
		if input.Key == "F12" {
			s.mu.Lock()
			decision := DecideF12Shortcut(input, s.f12Active[w.ID()], s.f12Blocked)
			s.mu.Unlock()
			switch decision {
			case F12Forward:
				w.Send("f12-shortcut", ToF12KeyboardEventPayload(input))
				prevent, handled = true, true
			case F12Block:
				prevent, handled = true, true
			}
		}
		if !handled && isInspectShortcut(input, isMac) {
			prevent = true
			if w.DevToolsOpened() {
				w.CloseDevTools()
			} else {
				w.OpenDevTools()
			}
		}
	}
	if s.opts.Platform == "linux" && !w.IsDestroyed() && IsFindChord(input, isMac) {
		w.Send("open-find-bar", nil)
		prevent = true
	}
	return prevent
}

// Searches the calling window and answers once the webview reports the first result of the request
func (s *Service) FindInPage(w Finder, query any, options any) (FindAck, error) {
	if w == nil || w.IsDestroyed() {
		return FindAck{}, nil
	}
	q := ""
	if query != nil {
		q = jsString(query)
	}
	forward, findNext := true, false
	if _, ok := options.(map[string]any); ok {
		forward = get(options, "forward") != false
		findNext = jsTruthy(get(options, "findNext"))
	}
	wt := &findWaiter{done: make(chan struct{})}
	s.mu.Lock()
	st := s.stateLocked(w)
	st.forwarding = true
	if q == "" {
		s.mu.Unlock()
		return FindAck{}, errors.New("Must provide a non-empty search content")
	}
	st.waiters = append(st.waiters, wt)
	s.mu.Unlock()
	id, err := w.FindInPage(q, forward, findNext)
	s.mu.Lock()
	if err != nil {
		for i, other := range st.waiters {
			if other == wt {
				st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return FindAck{}, err
	}
	if cur := s.windows[w.ID()]; cur == st {
		wt.requestID, wt.known = id, true
		if st.early[id] {
			s.resolveLocked(st, wt)
		}
	}
	s.mu.Unlock()
	<-wt.done
	return FindAck{}, nil
}

func (s *Service) resolveLocked(st *windowState, wt *findWaiter) {
	for i, other := range st.waiters {
		if other == wt {
			st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
			close(wt.done)
			break
		}
	}
	delete(st.early, wt.requestID)
}

// Takes a find result of a window, forwarding it as found-in-page and releasing the request that waited on it
func (s *Service) FoundInPage(w Sender, result FoundResult) {
	s.mu.Lock()
	st := s.windows[w.ID()]
	forward := st != nil && st.forwarding
	if st != nil {
		matched := false
		for _, wt := range append([]*findWaiter(nil), st.waiters...) {
			if wt.known && wt.requestID == result.RequestID {
				s.resolveLocked(st, wt)
				matched = true
			}
		}
		if !matched {
			for _, wt := range st.waiters {
				if !wt.known {
					st.early[result.RequestID] = true
					break
				}
			}
		}
	}
	s.mu.Unlock()
	if forward && !w.IsDestroyed() {
		w.Send("found-in-page", FoundInPagePayload{ActiveMatchOrdinal: result.ActiveMatchOrdinal, Count: result.Matches})
	}
}

// Ends the find of the calling window and clears its highlights
func (s *Service) StopFindInPage(w Finder) {
	if w == nil || w.IsDestroyed() {
		return
	}
	w.StopFindInPage()
}
