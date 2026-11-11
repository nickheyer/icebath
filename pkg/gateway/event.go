package gateway

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/observability"
)

var attachmentRefRE = regexp.MustCompile(`(?i)^(?:@(?:image|file|url):[^\n]+\n?)+`)

const maxAttachmentFilenameChars = 255

// Reduces an untrusted attachment name to a display safe basename, empty when unusable
func CleanAttachmentFilename(name any) string {
	s, ok := name.(string)
	if !ok {
		return ""
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		if c := pyunicode.Category(r); c == "Cc" || c == "Cf" {
			continue
		}
		b.WriteRune(r)
	}
	base := []rune(pystr.Strip(b.String()))
	if len(base) == 0 || string(base) == "." || string(base) == ".." {
		return ""
	}
	if len(base) > maxAttachmentFilenameChars {
		keep := []rune{}
		if dot := lastRuneIndex(base, '.'); dot > 0 && len(base)-dot-1 <= 16 {
			keep = base[dot:]
		}
		base = append(append([]rune{}, base[:maxAttachmentFilenameChars-len(keep)]...), keep...)
	}
	return string(base)
}

func lastRuneIndex(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

type MessageType string

const (
	MessageTypeText     MessageType = "text"
	MessageTypeLocation MessageType = "location"
	MessageTypePhoto    MessageType = "photo"
	MessageTypeVideo    MessageType = "video"
	MessageTypeAudio    MessageType = "audio"
	MessageTypeVoice    MessageType = "voice"
	MessageTypeDocument MessageType = "document"
	MessageTypeSticker  MessageType = "sticker"
	MessageTypeCommand  MessageType = "command"
)

type ProcessingOutcome string

const (
	ProcessingSuccess   ProcessingOutcome = "success"
	ProcessingFailure   ProcessingOutcome = "failure"
	ProcessingCancelled ProcessingOutcome = "cancelled"
)

type MessageEvent struct {
	Text                string
	MessageType         MessageType
	UserID              *string
	UserName            *string
	Source              *SessionSource
	RawMessage          any
	MessageID           *string
	LedgerMessageID     *string
	ReplyAnchorOverride *string
	PlatformUpdateID    *int64
	MediaURLs           []string
	MediaTypes          []string
	MediaTextInlined    []*bool
	ReplyToMessageID    *string
	ReplyToText         *string
	ReplyToAuthorID     *string
	ReplyToAuthorName   *string
	ReplyToIsOwnMessage bool
	PromptResponse      map[string]any
	AutoSkill           any
	ChannelPrompt       *string
	ChannelContext      *string
	Internal            bool
	Metadata            map[string]any
	Timestamp           time.Time
	AllowGatewayControl bool
	ReplyExpected       *bool
	InheritChannelPin   bool
	EditRevision        *string
	MediaFilenames      []string

	gatewayAccepted        bool
	producerDelivery       bool
	ingressSHA256          string
	consumerReply          *string
	webhookDuplicate       bool
	OnCommit               func()
	admittedBeforeDrain    bool
	notificationReplyMuted *bool
	pendingSTTText         *string
	pendingSTTTranscripts  []string
	pendingSTTClips        map[string]STTClip
	pendingSTTEchoed       int
	turnMarkerHandoff      bool
	activeTurnToken        any
	activeTurnSessionKey   *string
	turnRefusal            *string
	markerReleased         *InterruptEvent
	heartbeatSessionID     *string
	heartbeatResolvedID    *string
	heartbeatStarted       bool
	busyArrival            *observability.Arrival
	claimedOwner           *claimedTurnOwner
	botLoopAdmitted        bool
	fifoOnly               bool
	stopEpoch              int

	lastChunkLen int
}

// Event with the dataclass defaults for every field the caller leaves unset
func NewMessageEvent(text string) *MessageEvent {
	return &MessageEvent{Text: text, MessageType: MessageTypeText, Metadata: map[string]any{}, Timestamp: time.Now(), AllowGatewayControl: true}
}

// Takes the other event's reply expectation unless this one is addressed or the other is not
func (e *MessageEvent) AbsorbReplyExpected(other *MessageEvent) {
	if (e.ReplyExpected == nil || !*e.ReplyExpected) && (other.ReplyExpected == nil || *other.ReplyExpected) {
		e.ReplyExpected = other.ReplyExpected
	}
}

// Appends the other event's attachments keeping each original name aligned with its attachment
func (e *MessageEvent) ExtendMedia(other *MessageEvent) {
	if len(e.MediaFilenames) > 0 || len(other.MediaFilenames) > 0 {
		pad := func(m *MessageEvent) []string {
			out := append([]string{}, m.MediaFilenames...)
			for range m.MediaURLs {
				out = append(out, "")
			}
			return out[:len(m.MediaURLs)]
		}
		e.MediaFilenames = append(pad(e), pad(other)...)
	}
	e.MediaURLs = append(e.MediaURLs, other.MediaURLs...)
	e.MediaTypes = append(e.MediaTypes, other.MediaTypes...)
}

// Message text with the leading desktop attachment reference tags stripped
func (e *MessageEvent) commandText() string {
	return pystr.Lstrip(attachmentRefRE.ReplaceAllString(pystr.Lstrip(e.Text), ""))
}

// Reports a slash command the event is allowed to run as gateway control
func (e *MessageEvent) IsCommand() bool {
	return e.AllowGatewayControl && strings.HasPrefix(e.commandText(), "/")
}

// Lowercase command name without the bot suffix, false when not a command or the name holds a slash
func (e *MessageEvent) GetCommand() (string, bool) {
	if !e.IsCommand() {
		return "", false
	}
	raw := pystr.Lower(pystr.SplitWhitespace(e.commandText(), 1)[0][1:])
	raw, _, _ = strings.Cut(raw, "@")
	if strings.Contains(raw, "/") {
		return "", false
	}
	return raw, true
}

// Arguments after the command with em and en dashes turned back into ASCII flags
func (e *MessageEvent) GetCommandArgs() string {
	if !e.IsCommand() {
		return e.Text
	}
	parts := pystr.SplitWhitespace(pystr.Lstrip(e.commandText()), 1)
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}
	return strings.NewReplacer("——", "--", "—", "--", "–", "-").Replace(args)
}

func (e *MessageEvent) WakeInternal() bool { return e.Internal }

func (e *MessageEvent) WakeMetadata() map[string]any { return e.Metadata }

func (e *MessageEvent) WakeReplyMuted() (bool, bool) {
	if e.notificationReplyMuted == nil {
		return false, false
	}
	return *e.notificationReplyMuted, true
}

func (e *MessageEvent) WakeSourcePlatform() string {
	if e.Source == nil {
		return ""
	}
	return string(e.Source.Platform)
}

// Transcribed text and transcripts cached on the event by the inbound transcription pass, ok false when absent
func (e *MessageEvent) PendingSTT() (text string, transcripts []string, ok bool) {
	if e.pendingSTTText == nil {
		return "", nil, false
	}
	return *e.pendingSTTText, append([]string(nil), e.pendingSTTTranscripts...), true
}

// Caches the transcribed text and transcripts the routed turn reads instead of transcribing again
func (e *MessageEvent) SetPendingSTT(text string, transcripts []string) {
	e.pendingSTTText = &text
	e.pendingSTTTranscripts = append([]string(nil), transcripts...)
}

// Drops the cached transcription so a changed event transcribes again
func (e *MessageEvent) clearPendingSTT() {
	e.pendingSTTText = nil
	e.pendingSTTTranscripts = nil
}

var eventTurnStateMu sync.Mutex

// Whether this delivery lifecycle may release the durable crash marker of the turn
func (e *MessageEvent) TurnMarkerHandoff() bool {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.turnMarkerHandoff
}

func (e *MessageEvent) setTurnMarkerHandoff(v bool) {
	eventTurnStateMu.Lock()
	e.turnMarkerHandoff = v
	eventTurnStateMu.Unlock()
}

// Durable active turn token the runner stamped on admission, nil when none
func (e *MessageEvent) GatewayActiveTurnToken() any {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.activeTurnToken
}

// Stamps the durable active turn token the delivery lifecycle clears
func (e *MessageEvent) SetGatewayActiveTurnToken(token any) {
	eventTurnStateMu.Lock()
	e.activeTurnToken = token
	eventTurnStateMu.Unlock()
}

// Event an adopter waits on until this turn released its crash marker, nil when none
func (e *MessageEvent) GatewayMarkerReleased() *InterruptEvent {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.markerReleased
}

// Installs the event set once this turn released its crash marker
func (e *MessageEvent) SetGatewayMarkerReleased(ev *InterruptEvent) {
	eventTurnStateMu.Lock()
	e.markerReleased = ev
	eventTurnStateMu.Unlock()
}

// Heartbeat session a scheduled wake carries, nil for every other inbound
func (e *MessageEvent) HeartbeatSessionID() *string {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.heartbeatSessionID
}

// Marks the event as a scheduled heartbeat wake of the session
func (e *MessageEvent) SetHeartbeatSessionID(id *string) {
	eventTurnStateMu.Lock()
	e.heartbeatSessionID = id
	eventTurnStateMu.Unlock()
}
