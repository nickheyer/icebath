package install

import (
	"encoding/json"
	"strings"
)

// Event channel name the installer UI listens on
const EventChannel = "bootstrap"

// Version of the stage protocol the manifest reports
const ProtocolVersion = 1

type StageInfo struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	Category       string `json:"category"`
	NeedsUserInput bool   `json:"needs_user_input"`
}

type Manifest struct {
	ProtocolVersion *int        `json:"protocol_version"`
	Stages          []StageInfo `json:"stages"`
}

// Terminal stage frame with the stage name, outcome, skip flag, reason and stage data
type StageResult struct {
	Stage   string          `json:"stage"`
	OK      bool            `json:"ok"`
	Skipped bool            `json:"skipped"`
	Reason  *string         `json:"reason"`
	Data    json.RawMessage `json:"data"`
}

type StageState string

const (
	StateRunning   StageState = "running"
	StateSucceeded StageState = "succeeded"
	StateSkipped   StageState = "skipped"
	StateFailed    StageState = "failed"
)

type LogStream string

const (
	Stdout LogStream = "stdout"
	Stderr LogStream = "stderr"
)

type EventType string

const (
	EventManifest EventType = "manifest"
	EventStage    EventType = "stage"
	EventLog      EventType = "log"
	EventComplete EventType = "complete"
	EventFailed   EventType = "failed"
)

// One bootstrap channel payload, the fields of its type set
type Event struct {
	Type            EventType
	Stages          []StageInfo
	ProtocolVersion *int
	Name            string
	State           StageState
	DurationMs      *int64
	Result          *StageResult
	Error           *string
	Stage           *string
	Line            string
	Stream          LogStream
	InstallRoot     string
	Marker          json.RawMessage
}

// Tagged JSON of the event with exactly the keys its type carries
func (e Event) MarshalJSON() ([]byte, error) {
	switch e.Type {
	case EventManifest:
		stages := e.Stages
		if stages == nil {
			stages = []StageInfo{}
		}
		return json.Marshal(struct {
			Type            EventType   `json:"type"`
			Stages          []StageInfo `json:"stages"`
			ProtocolVersion *int        `json:"protocolVersion"`
		}{e.Type, stages, e.ProtocolVersion})
	case EventStage:
		return json.Marshal(struct {
			Type       EventType    `json:"type"`
			Name       string       `json:"name"`
			State      StageState   `json:"state"`
			DurationMs *int64       `json:"durationMs,omitempty"`
			Result     *StageResult `json:"result,omitempty"`
			Error      *string      `json:"error,omitempty"`
		}{e.Type, e.Name, e.State, e.DurationMs, e.Result, e.Error})
	case EventLog:
		return json.Marshal(struct {
			Type   EventType `json:"type"`
			Stage  *string   `json:"stage,omitempty"`
			Line   string    `json:"line"`
			Stream LogStream `json:"stream"`
		}{e.Type, e.Stage, e.Line, e.Stream})
	case EventComplete:
		marker := e.Marker
		if len(marker) == 0 {
			marker = json.RawMessage("null")
		}
		return json.Marshal(struct {
			Type        EventType       `json:"type"`
			InstallRoot string          `json:"installRoot"`
			Marker      json.RawMessage `json:"marker"`
		}{e.Type, e.InstallRoot, marker})
	default:
		errText := ""
		if e.Error != nil {
			errText = *e.Error
		}
		return json.Marshal(struct {
			Type  EventType `json:"type"`
			Stage *string   `json:"stage,omitempty"`
			Error string    `json:"error"`
		}{EventFailed, e.Stage, errText})
	}
}

// Event with terminal escapes removed from log lines only
func (e Event) SanitizedForUI() Event {
	if e.Type == EventLog {
		e.Line = StripANSI(e.Line)
	}
	return e
}

// Text a terminal would be left showing for one raw log line, escapes dropped and carriage return redraws collapsed
func StripANSI(line string) string {
	const esc = '\x1b'
	var out strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c != esc {
			out.WriteRune(c)
			continue
		}
		i++
		if i >= len(runes) {
			break
		}
		switch runes[i] {
		case '[':
			for i+1 < len(runes) {
				i++
				if runes[i] >= 0x40 && runes[i] <= 0x7e {
					break
				}
			}
		case ']', 'P', 'X', '^', '_':
			for i+1 < len(runes) {
				i++
				if runes[i] == 0x07 {
					break
				}
				if runes[i] == esc {
					if i+1 < len(runes) && runes[i+1] == '\\' {
						i++
					}
					break
				}
			}
		case '(', ')':
			if i+1 < len(runes) {
				i++
			}
		}
	}
	segments := strings.Split(out.String(), "\r")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] != "" {
			return segments[i]
		}
	}
	return ""
}

func strPtr(s string) *string { return &s }

func int64Ptr(v int64) *int64 { return &v }

// Manifest event of stages at the protocol version, nil version for synthetic manifests
func ManifestEvent(stages []StageInfo, version *int) Event {
	return Event{Type: EventManifest, Stages: stages, ProtocolVersion: version}
}

// Stage transition event, duration result and error set only when given
func StageEvent(name string, state StageState, durationMs *int64, result *StageResult, errText *string) Event {
	return Event{Type: EventStage, Name: name, State: state, DurationMs: durationMs, Result: result, Error: errText}
}

// Log line event of an optional stage on one stream
func LogEvent(stage, line string, stream LogStream) Event {
	e := Event{Type: EventLog, Line: line, Stream: stream}
	if stage != "" {
		e.Stage = strPtr(stage)
	}
	return e
}

// Failed event of an optional stage
func FailedEvent(stage, errText string) Event {
	e := Event{Type: EventFailed, Error: strPtr(errText)}
	if stage != "" {
		e.Stage = strPtr(stage)
	}
	return e
}

// Complete event for the install root with the published marker or JSON null
func CompleteEvent(installRoot string, marker json.RawMessage) Event {
	return Event{Type: EventComplete, InstallRoot: installRoot, Marker: marker}
}
