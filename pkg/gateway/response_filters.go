package gateway

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	InternalNotificationDisplayKind = "internal_notification"
	markerLengthCap                 = 64
)

var (
	LiveGatewaySilentMarkers = map[string]bool{
		"[SILENT]": true, "SILENT": true, "NO_REPLY": true, "NO REPLY": true,
		"[静默]": true, "静默": true, "[沉默]": true, "沉默": true,
	}
	bracketedSilenceMarkers    = bracketedMarkers()
	ProducerNoticeDisplayKinds = map[string]bool{"process_complete": true, "async_delegation_complete": true}
	MachineryDisplayKinds      = map[string]bool{InternalNotificationDisplayKind: true, "process_complete": true, "async_delegation_complete": true}
	ProducerCardKinds          = map[string]bool{InternalNotificationDisplayKind: true, "process_complete": true, "async_delegation_complete": true}
)

// Sorted silence markers that open with a bracket
func bracketedMarkers() []string {
	var out []string
	for m := range LiveGatewaySilentMarkers {
		if strings.HasPrefix(m, "[") {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// Upper cased text with whitespace runs collapsed to single spaces
func canonicalSilenceCandidate(text string) string {
	return strings.Join(pystr.SplitWhitespace(pystr.Upper(pystr.Strip(text)), -1), " ")
}

// Punctuation other than square brackets
func isEdgePunctuation(ch rune) bool { return ch != '[' && ch != ']' && unicode.IsPunct(ch) }

// Text with stray edge punctuation removed while bracket structure survives
func stripEdgeSilencePunctuation(text string) string {
	r := []rune(text)
	start, end := 0, len(r)
	for start < end && isEdgePunctuation(r[start]) {
		start++
	}
	for end > start && isEdgePunctuation(r[end-1]) {
		end--
	}
	return pystr.Strip(string(r[start:end]))
}

// Canonical forms of a marker sized response, none when it is blank or too long
func canonicalSilenceCandidates(text any) []string {
	s, _ := text.(string)
	stripped := pystr.Strip(s)
	if n := len([]rune(stripped)); n == 0 || n > markerLengthCap {
		return nil
	}
	depunctuated := stripEdgeSilencePunctuation(stripped)
	if depunctuated == stripped {
		return []string{canonicalSilenceCandidate(stripped)}
	}
	return []string{canonicalSilenceCandidate(stripped), canonicalSilenceCandidate(depunctuated)}
}

// True only when the response is exactly a silence marker, never for prose or a blank reply
func IsIntentionalSilenceResponse(response any) bool {
	for _, c := range canonicalSilenceCandidates(response) {
		if LiveGatewaySilentMarkers[c] {
			return true
		}
	}
	return false
}

// Head and last line of the text with trailing blank space ignored, head empty for one line
func splitTrailingLine(text string) (string, string) {
	t := pystr.Rstrip(text)
	i := strings.LastIndex(t, "\n")
	if i < 0 {
		return "", t
	}
	return pystr.Rstrip(t[:i]), t[i+1:]
}

// Reply without a silence marker that closes it on its own line, a bare marker unchanged
func StripTrailingSilenceMarker(response any) any {
	s, ok := response.(string)
	if !ok {
		return response
	}
	head, tail := splitTrailingLine(s)
	if pystr.Strip(head) != "" && IsIntentionalSilenceResponse(tail) {
		return head
	}
	return s
}

// True while a streamed reply's last line could still become a closing silence marker
func EndsWithPartialSilenceMarker(text any) bool {
	s, ok := text.(string)
	if !ok {
		return false
	}
	head, tail := splitTrailingLine(s)
	return pystr.Strip(head) != "" && IsPartialSilenceMarker(tail)
}

// SilenceMarkerStreamHold gates live reply deltas that may be or end in a silence marker
type SilenceMarkerStreamHold struct {
	buf  []rune
	sent int
}

// Part of the reply safe to show now, holding a possible bare or closing marker
func (h *SilenceMarkerStreamHold) Feed(delta string) string {
	h.buf = append(h.buf, []rune(delta)...)
	buf := string(h.buf)
	if IsPartialSilenceMarker(buf) {
		return ""
	}
	if EndsWithPartialSilenceMarker(buf) {
		head, _ := splitTrailingLine(buf)
		return h.release(len([]rune(head)))
	}
	return h.release(len(h.buf))
}

// Remainder the completed reply delivers, nothing for a bare marker
func (h *SilenceMarkerStreamHold) Finish() string {
	buf := string(h.buf)
	if IsIntentionalSilenceResponse(buf) {
		return ""
	}
	return h.release(len([]rune(StripTrailingSilenceMarker(buf).(string))))
}

// Unsent characters up to the bound
func (h *SilenceMarkerStreamHold) release(upto int) string {
	if upto <= h.sent {
		return ""
	}
	out := string(h.buf[h.sent:upto])
	h.sent = upto
	return out
}

// Loose silence match for cron and webhook lanes, a marker line at either end or a bracketed opener
func IsAutonomousSilenceResponse(response any) bool {
	s, _ := response.(string)
	stripped := pystr.Strip(s)
	if stripped == "" {
		return false
	}
	var lines []string
	for _, ln := range pystr.Splitlines(stripped, false) {
		if pystr.Strip(ln) != "" {
			lines = append(lines, ln)
		}
	}
	upper := pystr.Upper(stripped)
	for _, m := range bracketedSilenceMarkers {
		if strings.HasPrefix(upper, m) {
			return true
		}
	}
	for _, c := range []string{stripped, lines[0], lines[len(lines)-1]} {
		if IsIntentionalSilenceResponse(c) {
			return true
		}
	}
	return false
}

// Silence markers suppress delivery only for a successful agent turn
func IsIntentionalSilenceAgentResult(agentResult map[string]any, response any) bool {
	return agentResult != nil && !pyrepr.Truthy(agentResult["failed"]) && IsIntentionalSilenceResponse(response)
}

// DisplayEvent is the inbound gateway event view the display kind helpers read
type DisplayEvent interface {
	IsInternal() bool
	EventMetadata() map[string]any
	HeartbeatSessionID() any
	EventMessageID() any
}

// Persisted user row kind of a turn, machinery only for self injected events
func DisplayKindForEvent(ctx context.Context, event DisplayEvent) any {
	if kind := admissionDisplay(ctx, event)["kind"]; pyrepr.Truthy(kind) {
		return kind
	}
	if event == nil {
		return nil
	}
	if event.IsInternal() && len(producerCard(event)) > 0 {
		return event.EventMetadata()["display_kind"]
	}
	if event.IsInternal() || pyrepr.Truthy(event.HeartbeatSessionID()) {
		return InternalNotificationDisplayKind
	}
	return nil
}

// Display text of an internal producer notice card, empty otherwise
func producerCard(event DisplayEvent) map[string]any {
	if event == nil {
		return map[string]any{}
	}
	metadata := event.EventMetadata()
	text, isStr := metadata["display_text"].(string)
	kind, _ := metadata["display_kind"].(string)
	if event.IsInternal() && ProducerNoticeDisplayKinds[kind] && isStr && pystr.Strip(text) != "" {
		return map[string]any{"display_text": text}
	}
	return map[string]any{}
}

// Producer card text plus the title preview the canonical admission committed for the event
func DisplayMetadataForEvent(ctx context.Context, event DisplayEvent) map[string]any {
	out := producerCard(event)
	if preview := admissionDisplay(ctx, event)["title_preview"]; pyrepr.Truthy(preview) {
		out["title_preview"] = preview
	}
	return out
}

// Display metadata plus the canonical admission submission id
func PersistedUserMetadataForEvent(ctx context.Context, event DisplayEvent) map[string]any {
	out := DisplayMetadataForEvent(ctx, event)
	for k, v := range admissionSubmission(ctx, event) {
		out[k] = v
	}
	return out
}

// Only a machinery turn may vanish on a bare silence marker
func IsMachineryDisplayKind(displayKind any) bool {
	s, ok := displayKind.(string)
	return ok && MachineryDisplayKinds[s]
}

// Existing exemptions, or a room opt in backed by a healthy completed result
func SilenceAllowed(displayKind any, replyExpected, allowSilence *bool, agentResult map[string]any) bool {
	if IsMachineryDisplayKind(displayKind) || (replyExpected != nil && !*replyExpected) {
		return true
	}
	if allowSilence == nil || !*allowSilence || agentResult == nil {
		return false
	}
	for _, k := range []string{"failed", "partial", "interrupted", "error"} {
		if pyrepr.Truthy(agentResult[k]) {
			return false
		}
	}
	completed, present := agentResult["completed"]
	return !present || completed != false
}

// Persisted reply_expected key, only when the adapter knew
func ReplyExpectedMetadata(replyExpected *bool) map[string]any {
	if replyExpected == nil {
		return map[string]any{}
	}
	return map[string]any{"reply_expected": *replyExpected}
}

// True while streamed text could still resolve to a silence marker
func IsPartialSilenceMarker(text any) bool {
	for _, c := range canonicalSilenceCandidates(text) {
		if c == "" {
			continue
		}
		for m := range LiveGatewaySilentMarkers {
			if strings.HasPrefix(m, c) {
				return true
			}
		}
	}
	return false
}
