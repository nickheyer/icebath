// Small agent tools for clarifying questions, the todo list, session search and cross-platform messages
package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	Skipped   = "\x00skipped"
	Cancelled = "\x00cancelled"
	SkipWord  = "skip"

	Undelivered          = "[clarify prompt could not be delivered]"
	UndeliveredDeclined  = "[clarify prompt could not be delivered: destination refused]"
	UndeliveredNoSurface = "[clarify prompt could not be delivered: no chat surface]"
)

var (
	ErrClarifyDeclined  = errors.New("clarify prompt declined by the destination")
	ErrClarifyNoSurface = errors.New("no chat surface can render the clarify prompt")
)

type TextOutcome string

const (
	TextResolved          TextOutcome = "resolved"
	TextRejectedProse     TextOutcome = "rejected_prose"
	TextRejectedSelection TextOutcome = "rejected_selection"
	TextNoPending         TextOutcome = "no_pending"
)

type Pending struct {
	ID          string
	SessionKey  string
	Question    string
	Choices     []string
	MultiSelect bool

	done         chan struct{}
	set          bool
	response     *string
	awaitingText bool
	notice       string
}

func (k *Clarifier) AwaitingText(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	return ok && p.awaitingText
}

func (p *Pending) finish() {
	if !p.set {
		p.set = true
		close(p.done)
	}
}

type Clarifier struct {
	mu       sync.Mutex
	entries  map[string]*Pending
	sessions map[string][]string
}

func NewClarifier() *Clarifier {
	return &Clarifier{entries: map[string]*Pending{}, sessions: map[string][]string{}}
}

func (k *Clarifier) Register(id, sessionKey, question string, choices []string, multiSelect bool) *Pending {
	var kept []string
	if len(choices) > 0 {
		kept = append([]string(nil), choices...)
	}
	p := &Pending{ID: id, SessionKey: sessionKey, Question: question, Choices: kept,
		MultiSelect: multiSelect && len(choices) > 0, done: make(chan struct{}), awaitingText: len(choices) == 0}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries[id] = p
	k.sessions[sessionKey] = append(k.sessions[sessionKey], id)
	return p
}

func (k *Clarifier) Wait(ctx context.Context, c *tool.Call, id string, timeout int) (string, bool) {
	response, ok, _ := k.await(ctx, c, id, timeout)
	return response, ok
}

func (k *Clarifier) await(ctx context.Context, c *tool.Call, id string, timeout int) (string, bool, string) {
	k.mu.Lock()
	p, ok := k.entries[id]
	k.mu.Unlock()
	if !ok {
		return "", false, ""
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(time.Duration(timeout) * time.Second)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	start, lastTouch := time.Now(), time.Now()
wait:
	for {
		select {
		case <-p.done:
			break wait
		case <-deadline:
			break wait
		case <-ctx.Done():
			k.mu.Lock()
			if !p.set {
				cancelled := Cancelled
				p.response = &cancelled
			}
			k.mu.Unlock()
			break wait
		case now := <-ticker.C:
			if c != nil && c.Activity != nil && now.Sub(lastTouch) >= 10*time.Second {
				lastTouch = now
				c.Activity(fmt.Sprintf("waiting for user clarify response (%ds elapsed)", int(now.Sub(start).Seconds())))
			}
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, id)
	p.finish()
	ids := k.sessions[p.SessionKey]
	for i, v := range ids {
		if v == id {
			ids = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(k.sessions, p.SessionKey)
	} else {
		k.sessions[p.SessionKey] = ids
	}
	if p.response == nil {
		return "", false, p.notice
	}
	return *p.response, true, p.notice
}

func (k *Clarifier) Resolve(id, response string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	if !ok || p.set {
		return false
	}
	p.response = &response
	p.finish()
	return true
}

func (k *Clarifier) Get(id string) (*Pending, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	if !ok || p.set {
		return nil, false
	}
	return p, true
}

func (k *Clarifier) ForSession(sessionKey string, includeChoicePrompts bool) (*Pending, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.forSessionLocked(sessionKey, includeChoicePrompts)
}

func (k *Clarifier) forSessionLocked(sessionKey string, includeChoicePrompts bool) (*Pending, bool) {
	for _, id := range k.sessions[sessionKey] {
		if p, ok := k.entries[id]; ok && (includeChoicePrompts || p.awaitingText) {
			return p, true
		}
	}
	return nil, false
}

func (k *Clarifier) AttemptText(sessionKey, response string) TextOutcome {
	k.mu.Lock()
	p, ok := k.forSessionLocked(sessionKey, true)
	var entry Pending
	if ok {
		entry = Pending{Choices: p.Choices, MultiSelect: p.MultiSelect, awaitingText: p.awaitingText}
	}
	k.mu.Unlock()
	if !ok {
		return TextNoPending
	}
	coerced, reason, accepted := coerceText(&entry, response)
	if !accepted {
		if reason == "invalid_selection" {
			return TextRejectedSelection
		}
		return TextRejectedProse
	}
	if k.Resolve(p.ID, coerced) {
		return TextResolved
	}
	return TextNoPending
}

func (k *Clarifier) ResolveText(sessionKey, response string) bool {
	return k.AttemptText(sessionKey, response) == TextResolved
}

func (k *Clarifier) MarkAwaitingText(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	if ok {
		p.awaitingText = true
	}
	return ok
}

func (k *Clarifier) HasPending(sessionKey string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, id := range k.sessions[sessionKey] {
		if _, ok := k.entries[id]; ok {
			return true
		}
	}
	return false
}

func (k *Clarifier) ClearSession(sessionKey string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.clearLocked(sessionKey)
}

func (k *Clarifier) clearLocked(sessionKey string) int {
	ids := k.sessions[sessionKey]
	delete(k.sessions, sessionKey)
	cancelled := 0
	for _, id := range ids {
		p, ok := k.entries[id]
		delete(k.entries, id)
		if !ok || p.set {
			continue
		}
		value := Cancelled
		p.response = &value
		p.finish()
		cancelled++
	}
	return cancelled
}

func (k *Clarifier) Withdraw(sessionKey, id, notice string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.entries[id]
	if !ok || p.set {
		return false
	}
	if notice == "" {
		notice = Undelivered
	}
	p.notice = notice
	k.clearLocked(sessionKey)
	return true
}

func newClarifyID() string {
	var b [5]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func matchLabel(text string, choices []string) (string, bool) {
	wanted := StripRecommended(text)
	for _, choice := range choices {
		if strings.EqualFold(StripRecommended(choice), wanted) {
			return strip(choice), true
		}
	}
	return "", false
}

func splitTokens(text string) ([]string, bool) {
	if strings.Contains(text, ",") {
		var out []string
		for _, t := range strings.Split(text, ",") {
			if t = strip(t); t != "" {
				out = append(out, t)
			}
		}
		return out, true
	}
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return nil, false
	}
	for _, p := range parts {
		if !isDigits(p) {
			return nil, false
		}
	}
	return parts, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isInt(s string) bool {
	s = strip(s)
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return false
	}
	return isDigits(strings.ReplaceAll(s, "_", ""))
}

func intIndex(s string) int {
	s = strings.ReplaceAll(strip(s), "_", "")
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n - 1
}

func selectionTokens(text string, choices []string) ([]string, bool) {
	stripped := strip(text)
	if stripped == "" {
		return nil, false
	}
	tokens, split := splitTokens(stripped)
	if !split {
		if isDigits(strings.TrimPrefix(stripped, "-")) || isInt(stripped) {
			return []string{stripped}, true
		}
		return nil, false
	}
	if !strings.Contains(stripped, ",") || len(tokens) == 0 {
		return tokens, len(tokens) > 0
	}
	maxWords := 1
	for _, c := range choices {
		maxWords = max(maxWords, len(strings.Fields(c)))
	}
	for _, t := range tokens {
		if !isDigits(t) && len(strings.Fields(t)) > maxWords {
			return nil, false
		}
	}
	return tokens, true
}

func coerceText(p *Pending, response string) (string, string, bool) {
	text := strip(response)
	skip := strings.EqualFold(text, SkipWord)
	if len(p.Choices) == 0 {
		if skip {
			return Skipped, "", true
		}
		return text, "", true
	}
	var coerced string
	var found, shaped bool
	if p.MultiSelect {
		coerced, found = coerceMulti(p.Choices, text)
		_, shaped = selectionTokens(text, p.Choices)
	} else {
		shaped = isInt(text)
		idx := -1
		if shaped {
			idx = intIndex(text)
		}
		if idx >= 0 && idx < len(p.Choices) {
			coerced, found = p.Choices[idx], true
		} else {
			coerced, found = matchLabel(text, p.Choices)
		}
	}
	switch {
	case found:
		return coerced, "", true
	case skip:
		return Skipped, "", true
	case p.awaitingText:
		return text, "", true
	case shaped:
		return "", "invalid_selection", false
	}
	return "", "prose", false
}

func coerceMulti(choices []string, text string) (string, bool) {
	if text == "" {
		return "", false
	}
	tokens, split := splitTokens(text)
	if !split {
		tokens = []string{text}
	}
	var selected []string
	for _, token := range tokens {
		var label string
		var ok bool
		if isDigits(token) {
			if idx := intIndex(token); idx >= 0 && idx < len(choices) {
				label, ok = strip(choices[idx]), true
			}
		} else {
			label, ok = matchLabel(token, choices)
		}
		if !ok {
			return "", false
		}
		if !contains(selected, label) {
			selected = append(selected, label)
		}
	}
	if len(selected) == 0 {
		return "", false
	}
	raw, err := fileutil.EncodeJSON(selected, fileutil.JSONStyle{})
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func strip(s string) string {
	return strings.TrimFunc(s, isPySpace)
}

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

func runeLen(s string) int {
	return utf8.RuneCountInString(s)
}
