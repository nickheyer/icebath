package speech

import "context"

// A service that turns speech into text, what a speech plugin registers as a recognizer
type Recognizer struct {
	// As stt.provider names it
	Name  string
	Title string
	About string
	// Short name the voice requirements show it by, such as Groq or command: name; the title when empty
	Label string
	// What the log names it by when no local engine is installed and it transcribes in their place, such as Groq Whisper API
	Detected string
	// Warning logged when stt.provider names it and neither it nor its fallback can transcribe; empty logs its reason after the name
	Warning string
	// Info logged when its fallback transcribes in its place, nothing logged when empty
	Instead string
	// Runs on this machine: no upload cap applies and Apple's CAF is read as it is
	Local bool
	// A cloud service, whose uploads have their silence trimmed
	Cloud bool
	// Place among the cloud services stt.provider falls back to when no local engine is installed, lower first; 0 for none
	Rank int
	// Why it can't transcribe with the profile's settings now, empty when it can, never installing anything
	Missing func(ctx context.Context, job *Job) string
	// Its reason for not transcribing is the answer a transcription gets when it is chosen, in place of the setup hint naming every service
	Explains bool
	// Service tried in its place when it is chosen but missing, such as the other local engine
	Fallback string
	// Model it transcribes with, the call's own first, then its setting
	Model func(job *Job, model string) string
	// Language its own setting gives, empty to leave it to stt.language
	Language func(job *Job) string
	// Byte and duration caps of one upload with the model, none for a local engine
	Limit func(model string) Limit
	// Of the whisper family, which reads only the last 224 tokens of a prompt, so a longer one is cut to them
	Whisper    bool
	Transcribe func(ctx context.Context, job *Job, path string, hints Hints) Transcript
	// Live session of the speech as it is spoken, nil for a recognizer that only transcribes whole files
	Live func(ctx context.Context, job *Job, hints Hints, partial func(string)) (Live, error)
	// What a person sets up to use it
	Setup Setup
	// Settings of it a person picks from a list in the voice setup, such as its model
	Choices []Choices
}

// What one transcription is told besides the audio
type Hints struct {
	Model    string
	Language string
	Prompt   string
}

// Most one upload request may carry
type Limit struct {
	Bytes int64
	// Seconds of audio, 0 for no cap
	Seconds float64
}

// What a transcription came back with
type Transcript struct {
	Success bool   `json:"success"`
	Text    string `json:"transcript"`
	Error   string `json:"error,omitempty"`
	// Recognizer that answered
	Provider string `json:"provider,omitempty"`
	// Nothing was said
	NoSpeech bool `json:"no_speech,omitempty"`
	// What was heard was a known hallucination on silence, so it reads as nothing
	Filtered bool `json:"filtered,omitempty"`
	// Pieces an upload too large for one request was cut into
	Segments int `json:"segments,omitempty"`
	// Why nothing claimed the configured service
	Kind string `json:"error_type,omitempty"`
}

// Transcript of a failure
func Failure(message string) Transcript {
	return Transcript{Error: message}
}

// Transcript of a failure of a recognizer, named
func (r Recognizer) Failure(message string) Transcript {
	return Transcript{Error: message, Provider: r.Name}
}

// Transcript of a success of a recognizer
func (r Recognizer) Heard(text string) Transcript {
	return Transcript{Success: true, Text: text, Provider: r.Name}
}

// One live transcription session: 16 kHz mono PCM in, partial text out through the callback it was opened with, the final transcript once
// the audio ends
type Live interface {
	Push(pcm []byte)
	// Ends the audio and waits for the final transcript, giving up after the seconds
	Finish(timeout float64) Transcript
	Cancel()
}

// Why the recognizer can't transcribe with the profile's settings now, empty when it can
func (j *Job) Missing(ctx context.Context, r Recognizer) string {
	return r.missing(ctx, j)
}

// Whether the recognizer can transcribe now
func (r Recognizer) missing(ctx context.Context, job *Job) string {
	if r.Missing == nil {
		return ""
	}
	return r.Missing(ctx, job)
}
