package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const historyMediaLookupMaxWorkers = 2

var (
	historyMediaLookupTimeout   = 5 * time.Second
	historyMediaLookupAdmission = make(chan struct{}, historyMediaLookupMaxWorkers)
	jsonMediaToolPathFields     = []string{"host_image", "image", "agent_visible_image"}
	toolMediaRE                 = newPyRE(`(?i)MEDIA:(?P<path>(?:[A-Za-z]:[/\\]|/|~\/)\S+\.(?:png|jpe?g|gif|webp|` +
		`mp4|mov|avi|mkv|webm|ogg|opus|mp3|wav|m4a|` +
		`flac|epub|pdf|zip|rar|7z|docx?|xlsx?|pptx?|` +
		`txt|csv|apk|ipa))`)
	threePlusNewlinesRE = regexp.MustCompile(`\n{3,}`)
	localFileExpanduser = pypath.Expanduser
	localFileIsFile     = func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.Mode().IsRegular()
	}
)

// Session store surface the history media lookup reads transcripts through
type TranscriptLoader interface {
	LoadTranscript(ctx context.Context, sessionID string) ([]any, error)
}

// Optional routing index mapping a session key to its transcript session id
type SessionIDPeeker interface {
	PeekSessionID(sessionKey string) (string, bool)
}

// Dedup set of media paths prior turns already delivered, both JSON payload and assistant message shapes
func collectHistoryMediaPaths(ctx context.Context, history []any) (map[string]struct{}, error) {
	paths := map[string]struct{}{}
	toolNames, err := ToolNameByCallID(history)
	if err != nil {
		return nil, err
	}
	addTextMediaPaths := func(content string) error {
		for _, m := range toolMediaRE.finditer([]rune(content)) {
			if p := pystr.RstripChars(pystr.Strip(toolMediaRE.group(m, "path")), `",}`); p != "" {
				paths[p] = struct{}{}
			}
		}
		media, _, err := ExtractMedia(ctx, content)
		if err != nil {
			return err
		}
		for _, ref := range media {
			paths[ref.Path] = struct{}{}
		}
		return nil
	}
	for _, msg := range history {
		roleV, err := dictGet(msg, "role")
		if err != nil {
			return nil, err
		}
		role, _ := roleV.(string)
		if role != "assistant" && role != "tool" && role != "function" {
			continue
		}
		contentV, _ := dictGet(msg, "content")
		content := ""
		if pyrepr.Truthy(contentV) {
			content = pyrepr.Str(contentV)
		}
		if strings.Contains(content, "MEDIA:") {
			if err := addTextMediaPaths(content); err != nil {
				return nil, err
			}
			continue
		}
		if role == "assistant" {
			continue
		}
		cid := ""
		if v, err := firstTruthy(msg, "tool_call_id", "call_id"); err == nil && pyrepr.Truthy(v) {
			cid = pyrepr.Str(v)
		}
		if toolNames[cid] != "image_generate" {
			continue
		}
		payload, err := pyjson.Loads(content)
		if err != nil {
			continue
		}
		obj, ok := payload.(*pyjson.Object)
		if !ok {
			continue
		}
		if success, _ := obj.Get("success"); !pyrepr.Truthy(success) {
			continue
		}
		for _, field := range jsonMediaToolPathFields {
			if jp, _ := obj.Get(field); jp != nil {
				if s, ok := jp.(string); ok && s != "" {
					paths[s] = struct{}{}
					break
				}
			}
		}
	}
	return paths, nil
}

// Media paths delivered in prior turns of the session, excluding the current turn
func (b *BasePlatformAdapter) historyMediaPathsForSession(ctx context.Context, sessionKey string) (map[string]struct{}, error) {
	b.mu.Lock()
	store := b.sessionStore
	b.mu.Unlock()
	loader, ok := store.(TranscriptLoader)
	if store == nil || !ok {
		return nil, nil
	}
	sessionID := ""
	if peeker, ok := store.(SessionIDPeeker); ok {
		sessionID, _ = peeker.PeekSessionID(sessionKey)
	}
	if sessionID == "" {
		sessionID = sessionKey
	}
	transcript, err := loader.LoadTranscript(ctx, sessionID)
	if err != nil {
		var readErr *TranscriptReadError
		if errors.As(err, &readErr) {
			slog.Warn(fmt.Sprintf("Transcript read failed for session %s; media dedup runs with no history this turn (#100788)", sessionKey))
		}
		return nil, nil
	}
	if len(transcript) == 0 {
		return nil, nil
	}
	history := append([]any(nil), transcript...)
	lastUser := -1
	for i := len(history) - 1; i >= 0; i-- {
		if role, _ := dictGet(history[i], "role"); role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser >= 0 {
		history = history[:lastUser]
	} else {
		for i := len(history) - 1; i >= 0; i-- {
			if role, _ := dictGet(history[i], "role"); role == "assistant" {
				history = append(history[:i:i], history[i+1:]...)
				break
			}
		}
	}
	if len(history) == 0 {
		return nil, nil
	}
	return collectHistoryMediaPaths(ctx, history)
}

// History lookup in a bounded detached worker that fails open on capacity, timeout or error
func (b *BasePlatformAdapter) boundedHistoryMediaPathsForSession(ctx context.Context, sessionKey string) (map[string]struct{}, error) {
	name := b.Self().Name()
	failOpen := func(reason string, err error) {
		msg := fmt.Sprintf("[%s] "+reason+" %s; delivering bare local file path(s) without history dedup", name, sessionKey)
		if err != nil {
			msg += fmt.Sprintf(": %v", err)
		}
		slog.Warn(msg)
	}
	select {
	case historyMediaLookupAdmission <- struct{}{}:
	default:
		failOpen("Media-delivery history lookup capacity exhausted for", nil)
		return nil, nil
	}
	type lookup struct {
		paths map[string]struct{}
		err   error
	}
	result := make(chan lookup, 1)
	workerCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() { <-historyMediaLookupAdmission }()
		var out lookup
		func() {
			defer func() {
				if r := recover(); r != nil {
					out = lookup{err: fmt.Errorf("%v", r)}
				}
			}()
			out.paths, out.err = b.historyMediaPathsForSession(workerCtx, sessionKey)
		}()
		result <- out
	}()
	timer := time.NewTimer(historyMediaLookupTimeout)
	defer timer.Stop()
	select {
	case r := <-result:
		if r.err != nil {
			failOpen("Media-delivery history lookup failed for", r.err)
			return nil, nil
		}
		return r.paths, nil
	case <-timer.C:
		failOpen("Timed out loading media-delivery history for", nil)
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Spans of fenced code blocks and inline code in the content
func codeSpans(content []rune) []runeSpan {
	var spans []runeSpan
	for _, rx := range []pyRE{fencedCodeRE, inlineCodeRE} {
		for _, m := range rx.finditer(content) {
			spans = append(spans, runeSpan{m.RuneIndex, m.RuneIndex + m.RuneLength})
		}
	}
	return spans
}

// Bare local file paths with deliverable extensions that exist on disk, outside code, and the cleaned text
func ExtractLocalFiles(ctx context.Context, content string) ([]string, string) {
	parts := make([]string, 0, len(MediaDeliveryExts))
	for _, e := range MediaDeliveryExts {
		parts = append(parts, strings.TrimLeft(e, "."))
	}
	pathRE := newPyRE(`(?i)(?<![/:\w.])(?:~/|/|[A-Za-z]:[/\\])(?:[\w.\-]+[/\\])*[\w.\-]+\.(?:` + strings.Join(parts, "|") + `)\b`)
	runes := []rune(content)
	spans := codeSpans(runes)
	var order []string
	unique := map[string]string{}
	for _, m := range pathRE.finditer(runes) {
		inCode := false
		for _, s := range spans {
			if s.start <= m.RuneIndex && m.RuneIndex < s.end {
				inCode = true
				break
			}
		}
		if inCode {
			continue
		}
		raw := m.String()
		expanded := localFileExpanduser(raw)
		if localFileIsFile(expanded) {
			if _, seen := unique[expanded]; !seen {
				unique[expanded] = raw
				order = append(order, expanded)
			}
		} else {
			slog.Info(fmt.Sprintf("Skipping bare file path in reply (no file on disk): %s", logSafePath(raw)))
		}
	}
	if len(unique) == 0 {
		return []string{}, content
	}
	cleaned := content
	for _, p := range order {
		cleaned = strings.ReplaceAll(cleaned, unique[p], "")
	}
	return order, pystr.Strip(threePlusNewlinesRE.ReplaceAllString(cleaned, "\n\n"))
}

// Removes MEDIA directives from a message body, the module alias of the tag stripper
func stripMediaDirectives(ctx context.Context, text string) (string, error) {
	return stripMediaTagDirectives(ctx, text)
}
