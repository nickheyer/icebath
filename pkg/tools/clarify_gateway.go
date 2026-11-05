package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Outcomes for typed clarify replies
const (
	ClarifyTextResolved          = "resolved"
	ClarifyTextRejectedProse     = "rejected_prose"
	ClarifyTextRejectedSelection = "rejected_selection"
	ClarifyTextNoPending         = "no_pending"
	ClarifySkipWord              = "skip"
	ClarifySkipped               = "\x00skipped"
	ClarifyCancelled             = "\x00cancelled"
)

// ClarifyEntry is one pending clarify request inside a gateway session
type ClarifyEntry struct {
	ClarifyID    string
	SessionKey   string
	Question     string
	Choices      []string
	MultiSelect  bool
	AwaitingText bool
	done         chan struct{}
	response     *string
	resolved     bool
}

var (
	clarifyMu           sync.Mutex
	clarifyEntries      = map[string]*ClarifyEntry{}
	clarifySessionIndex = map[string][]string{}
)

// RegisterGatewayClarify registers a pending clarify, open ended entries start in text mode
func RegisterGatewayClarify(clarifyID, sessionKey, question string, choices []string, multiSelect bool) *ClarifyEntry {
	entry := &ClarifyEntry{ClarifyID: clarifyID, SessionKey: sessionKey, Question: question, MultiSelect: multiSelect && len(choices) > 0,
		AwaitingText: len(choices) == 0, done: make(chan struct{})}
	if len(choices) > 0 {
		entry.Choices = append([]string(nil), choices...)
	}
	clarifyMu.Lock()
	clarifyEntries[clarifyID] = entry
	clarifySessionIndex[sessionKey] = append(clarifySessionIndex[sessionKey], clarifyID)
	clarifyMu.Unlock()
	return entry
}

// Marks the entry resolved under the lock and wakes the waiter once
func (e *ClarifyEntry) settle(response *string) {
	if e.resolved {
		return
	}
	e.resolved = true
	e.response = response
	close(e.done)
}

// WaitForGatewayClarify blocks until the entry resolves or the timeout elapses, nil on timeout or unknown id
func WaitForGatewayClarify(ctx context.Context, env *Env, clarifyID string, timeout time.Duration) *string {
	clarifyMu.Lock()
	entry := clarifyEntries[clarifyID]
	clarifyMu.Unlock()
	if entry == nil {
		return nil
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	activity := newActivityState()
	for {
		slice := time.Second
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			if remaining < slice {
				slice = remaining
			}
		}
		fired := false
		select {
		case <-entry.done:
			fired = true
		case <-time.After(slice):
		}
		if fired {
			break
		}
		if IsInterrupted(ctx) {
			clarifyMu.Lock()
			if !entry.resolved {
				cancelled := ClarifyCancelled
				entry.response = &cancelled
			}
			clarifyMu.Unlock()
			break
		}
		touchActivityIfDue(ctx, env, activity, "waiting for user clarify response")
	}
	clarifyMu.Lock()
	delete(clarifyEntries, clarifyID)
	if !entry.resolved {
		entry.resolved = true
		close(entry.done)
	}
	ids := clarifySessionIndex[entry.SessionKey]
	for i, id := range ids {
		if id == clarifyID {
			ids = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(clarifySessionIndex, entry.SessionKey)
	} else {
		clarifySessionIndex[entry.SessionKey] = ids
	}
	response := entry.response
	clarifyMu.Unlock()
	return response
}

// ResolveGatewayClarify unblocks the waiter, false when already resolved, expired or unknown
func ResolveGatewayClarify(clarifyID, response string) bool {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	entry := clarifyEntries[clarifyID]
	if entry == nil || entry.resolved {
		return false
	}
	entry.settle(&response)
	return true
}

// GetPendingClarify returns the entry while it still awaits an answer
func GetPendingClarify(clarifyID string) *ClarifyEntry {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	entry := clarifyEntries[clarifyID]
	if entry == nil || entry.resolved {
		return nil
	}
	return entry
}

// GetPendingClarifyForSession returns the oldest entry awaiting free text, or any unresolved entry when asked
func GetPendingClarifyForSession(sessionKey string, includeChoicePrompts bool) *ClarifyEntry {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	for _, cid := range clarifySessionIndex[sessionKey] {
		entry := clarifyEntries[cid]
		if entry != nil && (includeChoicePrompts || entry.AwaitingText) {
			return entry
		}
	}
	return nil
}

// Stripped choice text matching case insensitively while ignoring the recommended suffix
func clarifyMatchLabel(text string, choices []string) (string, bool) {
	wanted := strings.ToLower(StripRecommended(text))
	for _, choice := range choices {
		if strings.ToLower(StripRecommended(choice)) == wanted {
			return strings.TrimSpace(choice), true
		}
	}
	return "", false
}

// Comma separated tokens, or space separated all numeric tokens, else nil
func clarifySplitTokens(text string) []string {
	if strings.Contains(text, ",") {
		var out []string
		for _, t := range strings.Split(text, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	parts := strings.Fields(text)
	if len(parts) > 1 {
		for _, p := range parts {
			if !isDigits(p) {
				return nil
			}
		}
		return parts
	}
	return nil
}

// Reports whether the text is one or more ASCII digits
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Reports whether Python int() accepts the text
func clarifyIsInt(text string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(text, "_", "")))
	return err == nil && !strings.Contains(strings.Trim(text, " "), " ")
}

// Tokens when the text looks like a typed selection, nil for free prose
func clarifySelectionAttemptTokens(text string, choices []string) []string {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	tokens := clarifySplitTokens(stripped)
	if tokens == nil {
		if isDigits(strings.TrimPrefix(stripped, "-")) || clarifyIsInt(stripped) {
			return []string{stripped}
		}
		return nil
	}
	if !strings.Contains(stripped, ",") || len(tokens) == 0 {
		if len(tokens) == 0 {
			return nil
		}
		return tokens
	}
	maxWords := 1
	for _, c := range choices {
		if n := len(strings.Fields(c)); n > maxWords {
			maxWords = n
		}
	}
	for _, t := range tokens {
		if !isDigits(t) && len(strings.Fields(t)) > maxWords {
			return nil
		}
	}
	return tokens
}

// Accepted value for a typed reply, or the rejection reason invalid_selection or prose
func clarifyCoerceTextDetailed(entry *ClarifyEntry, response string) (string, bool, string) {
	text := strings.TrimSpace(response)
	isSkip := strings.ToLower(text) == ClarifySkipWord
	if len(entry.Choices) == 0 {
		if isSkip {
			return ClarifySkipped, true, ""
		}
		return text, true, ""
	}
	var coerced string
	var ok, selectionShaped bool
	if entry.MultiSelect {
		coerced, ok = clarifyCoerceMultiSelectText(entry, text)
		selectionShaped = clarifySelectionAttemptTokens(text, entry.Choices) != nil
	} else {
		selectionShaped = clarifyIsInt(text)
		idx := -1
		if selectionShaped {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(text, "_", "")))
			idx = n - 1
		}
		if idx >= 0 && idx < len(entry.Choices) {
			coerced, ok = entry.Choices[idx], true
		} else {
			coerced, ok = clarifyMatchLabel(text, entry.Choices)
		}
	}
	if ok {
		return coerced, true, ""
	}
	if isSkip {
		return ClarifySkipped, true, ""
	}
	if entry.AwaitingText {
		return text, true, ""
	}
	if selectionShaped {
		return "", false, "invalid_selection"
	}
	return "", false, "prose"
}

// Parses "1,3", "1 3" or labels into a JSON array of choice labels, false when any token is unrecognised
func clarifyCoerceMultiSelectText(entry *ClarifyEntry, text string) (string, bool) {
	if text == "" {
		return "", false
	}
	tokens := clarifySplitTokens(text)
	if tokens == nil {
		tokens = []string{text}
	}
	var selected []string
	for _, token := range tokens {
		var label string
		var ok bool
		if isDigits(token) {
			idx, _ := strconv.Atoi(token)
			idx--
			if idx >= 0 && idx < len(entry.Choices) {
				label, ok = strings.TrimSpace(entry.Choices[idx]), true
			}
		} else {
			label, ok = clarifyMatchLabel(token, entry.Choices)
		}
		if !ok {
			return "", false
		}
		if !containsString(selected, label) {
			selected = append(selected, label)
		}
	}
	if len(selected) == 0 {
		return "", false
	}
	raw, _ := json.Marshal(selected)
	return string(raw), true
}

// AttemptClarifyTextResponse tries to resolve the oldest pending clarify from typed text and reports the outcome
func AttemptClarifyTextResponse(sessionKey, response string) string {
	entry := GetPendingClarifyForSession(sessionKey, true)
	if entry == nil {
		return ClarifyTextNoPending
	}
	coerced, ok, reason := clarifyCoerceTextDetailed(entry, response)
	if !ok {
		if reason == "invalid_selection" {
			return ClarifyTextRejectedSelection
		}
		return ClarifyTextRejectedProse
	}
	if ResolveGatewayClarify(entry.ClarifyID, coerced) {
		return ClarifyTextResolved
	}
	return ClarifyTextNoPending
}

// ResolveClarifyTextResponse is true only when the typed reply was accepted and the waiter unblocked
func ResolveClarifyTextResponse(sessionKey, response string) bool {
	return AttemptClarifyTextResponse(sessionKey, response) == ClarifyTextResolved
}

// MarkClarifyAwaitingText flips an entry into text capture mode, false if unknown
func MarkClarifyAwaitingText(clarifyID string) bool {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	entry := clarifyEntries[clarifyID]
	if entry != nil {
		entry.AwaitingText = true
	}
	return entry != nil
}

// HasPendingClarify is true when the session has at least one pending clarify entry
func HasPendingClarify(sessionKey string) bool {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	for _, cid := range clarifySessionIndex[sessionKey] {
		if clarifyEntries[cid] != nil {
			return true
		}
	}
	return false
}

// ClearClarifySession drops every pending clarify for a session and returns how many were cancelled
func ClearClarifySession(sessionKey string) int {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	cancelled := 0
	ids := clarifySessionIndex[sessionKey]
	delete(clarifySessionIndex, sessionKey)
	for _, cid := range ids {
		entry := clarifyEntries[cid]
		delete(clarifyEntries, cid)
		if entry == nil || entry.resolved {
			continue
		}
		response := ClarifyCancelled
		entry.settle(&response)
		cancelled++
	}
	return cancelled
}

// ResolveClarifyTimeout reads clarify.timeout when set, else agent.clarify_timeout, else 3600, non numeric gives 3600
func ResolveClarifyTimeout(cfg config.Tree) int {
	var raw any
	if clarify, ok := cfg["clarify"].(map[string]any); ok {
		raw = clarify["timeout"]
	}
	if raw == nil {
		raw = 3600
		if agent, ok := cfg["agent"].(map[string]any); ok {
			if v, present := agent["clarify_timeout"]; present {
				raw = v
			}
		}
	}
	switch v := raw.(type) {
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 3600
		}
		return n
	}
	if n, ok := pyIntValue(raw); ok {
		return n
	}
	return 3600
}

// GetClarifyTimeout is the clarify timeout from config.yaml, zero or negative meaning unlimited
func GetClarifyTimeout(env *Env) int {
	doc, err := readRawConfig(env)
	if err != nil {
		return 3600
	}
	return ResolveClarifyTimeout(doc)
}
