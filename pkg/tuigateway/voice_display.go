package tuigateway

import (
	"encoding/json"
	"errors"
)

type VoiceToggleAction string

func (VoiceToggleAction) Values() []string { return []string{"status", "on", "off", "tts"} }

type VoiceToggleParams struct {
	Action  VoiceToggleAction `json:"action,omitzero"`
	Profile *string           `json:"profile,omitzero"`
}

// Voice mode state, the requirements probe on status and the spoken stop hint on on
type VoiceToggleResult struct {
	Enabled        bool   `json:"enabled"`
	RecordKey      string `json:"record_key"`
	TTS            bool   `json:"tts"`
	StopHint       string `json:"stop_hint,omitzero"`
	Available      *bool  `json:"available,omitzero"`
	AudioAvailable *bool  `json:"audio_available,omitzero"`
	STTAvailable   *bool  `json:"stt_available,omitzero"`
	Details        string `json:"details,omitzero"`
}

type VoiceRecordAction string

func (VoiceRecordAction) Values() []string { return []string{"start", "stop"} }

type VoiceRecordParams struct {
	Action    VoiceRecordAction `json:"action,omitzero"`
	SessionID string            `json:"session_id,omitzero"`
	Profile   *string           `json:"profile,omitzero"`
}

type VoiceRecordStatus string

func (VoiceRecordStatus) Values() []string { return []string{"recording", "stopped", "busy"} }

type VoiceRecordResult struct {
	Status VoiceRecordStatus `json:"status"`
	Reason string            `json:"reason,omitzero"`
}

// Text of a voice capture, or a stop phrase, a typed stop or the silence limit that ended it
type VoiceTranscriptPayload struct {
	Text          string `json:"text,omitzero"`
	StopPhrase    bool   `json:"stop_phrase,omitzero"`
	Typed         bool   `json:"typed,omitzero"`
	NoSpeechLimit bool   `json:"no_speech_limit,omitzero"`
}

type VoicePartialPayload struct {
	Text string `json:"text"`
}

// Recorder state, idle, listening, transcribing and the others the recorder names
type VoiceStatusPayload struct {
	State string `json:"state"`
}

type WakeStartParams struct {
	Surface   string  `json:"surface,omitzero"`
	Persist   bool    `json:"persist,omitzero"`
	SessionID string  `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Listener arming, a refusal carrying its reason with a hint or the surface that owns the microphone
type WakeStartResult struct {
	Started          bool    `json:"started"`
	Reason           string  `json:"reason,omitzero"`
	Hint             string  `json:"hint,omitzero"`
	Phrase           string  `json:"phrase,omitzero"`
	Provider         string  `json:"provider,omitzero"`
	OwnerSurface     *string `json:"owner_surface,omitzero"`
	EnabledPersisted bool    `json:"enabled_persisted,omitzero"`
}

type WakeStopParams struct {
	Persist bool    `json:"persist,omitzero"`
	Profile *string `json:"profile,omitzero"`
}

// Listener stop, the reason null when this surface stopped it and not_owner otherwise
type WakeStopResult struct {
	Reason            *string `json:"reason"`
	Stopped           bool    `json:"stopped"`
	DisabledPersisted bool    `json:"disabled_persisted"`
	Phrase            string  `json:"phrase"`
	Provider          string  `json:"provider"`
}

type WakeControlParams struct {
	Profile *string `json:"profile,omitzero"`
}

type WakeResumeResult struct {
	Reason  *string `json:"reason"`
	Resumed bool    `json:"resumed"`
}

type WakeStatusParams struct {
	Surface string  `json:"surface,omitzero"`
	Profile *string `json:"profile,omitzero"`
}

// Configured microphone, an index or a name, null for the system default
type WakeDeviceSelector struct {
	Index *int64
	Name  string
}

func (s *WakeDeviceSelector) UnmarshalJSON(data []byte) error {
	var index int64
	if err := json.Unmarshal(data, &index); err == nil {
		*s = WakeDeviceSelector{Index: &index}
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return errors.New("wants a device index or name")
	}
	*s = WakeDeviceSelector{Name: name}
	return nil
}

func (s WakeDeviceSelector) MarshalJSON() ([]byte, error) {
	if s.Index != nil {
		return json.Marshal(*s.Index)
	}
	return json.Marshal(s.Name)
}

// Audio layer diagnostics of the configured microphone
type WakeInputDevice struct {
	Selector          *WakeDeviceSelector `json:"selector"`
	Name              string              `json:"name,omitzero"`
	Error             string              `json:"error,omitzero"`
	MaxInputChannels  int64               `json:"max_input_channels,omitzero"`
	DefaultSamplerate float64             `json:"default_samplerate,omitzero"`
	HostapiIndex      *int64              `json:"hostapi_index,omitzero"`
	Hostapi           string              `json:"hostapi,omitzero"`
}

// Wake word state, enabled the config truth, listening the caller's armed detector, audio_silent armed but deaf
type WakeStatusResult struct {
	Listening           bool            `json:"listening"`
	OwnedByCaller       bool            `json:"owned_by_caller"`
	OwnerSurface        *string         `json:"owner_surface"`
	Phrase              string          `json:"phrase"`
	Provider            string          `json:"provider"`
	ConfiguredSurface   string          `json:"configured_surface"`
	InputDevice         WakeInputDevice `json:"input_device"`
	Available           bool            `json:"available"`
	Hint                string          `json:"hint"`
	Enabled             bool            `json:"enabled"`
	AudioSilent         bool            `json:"audio_silent"`
	LocalInputAvailable bool            `json:"local_input_available"`
}

type WakeDetectedPayload struct {
	Phrase          string  `json:"phrase"`
	Profile         *string `json:"profile"`
	StartNewSession bool    `json:"start_new_session"`
}

// Turns voice mode or speech output on or off, or reports them with the requirements probe
var VoiceToggle = NewMethod[VoiceToggleParams, VoiceToggleResult]("voice.toggle")

// Starts or stops a push to talk capture, the text arriving as voice.transcript
var VoiceRecord = NewMethod[VoiceRecordParams, VoiceRecordResult]("voice.record")

// Arms the wake word listener for the calling surface
var WakeStart = NewMethod[WakeStartParams, WakeStartResult]("wake.start")

// Stops this surface's listener, persist also turning the wake word off in config
var WakeStop = NewMethod[WakeStopParams, WakeStopResult]("wake.stop")

// Reclaims the microphone after a pause, nothing when the listener is not armed
var WakeResume = NewMethod[WakeControlParams, WakeResumeResult]("wake.resume")

// Everything a client needs to draw the wake word state and decide whether to arm it
var WakeStatus = NewMethod[WakeStatusParams, WakeStatusResult]("wake.status")

// A spoken interjection interrupted the turn
var VoiceInterruptedEvent = NewEvent[NoPayload]("voice.interrupted")

// The recorder state changed
var VoiceStatusEvent = NewEvent[VoiceStatusPayload]("voice.status")

// Live text so far while the user still speaks
var VoicePartialEvent = NewEvent[VoicePartialPayload]("voice.partial")

// A voice capture produced text or ended on a stop phrase or the silence limit
var VoiceTranscriptEvent = NewEvent[VoiceTranscriptPayload]("voice.transcript")

// A wake phrase fired
var WakeDetectedEvent = NewEvent[WakeDetectedPayload]("wake.detected")

type DisplayLeaseHolder string

func (DisplayLeaseHolder) Values() []string { return []string{"agent", "human"} }

// Takeover lease of a screen as clients see it, the holder's viewer id never leaving the gateway
type DisplayLease struct {
	Holder     DisplayLeaseHolder `json:"holder"`
	ViewerID   *string            `json:"viewer_id"`
	ViewerHash *string            `json:"viewer_hash"`
	Since      float64            `json:"since"`
	Epoch      int64              `json:"epoch"`
	Reason     string             `json:"reason"`
}

// Docker sandbox kept on the previous default image, the switch left to the user
type DisplayImageSwitch struct {
	CurrentImage string `json:"current_image"`
	TargetImage  string `json:"target_image"`
	Containers   int64  `json:"containers"`
}

// Runtime of the profile's screen with its lease and the profile key it speaks for
type DisplayStatus struct {
	Profile           string              `json:"profile"`
	Supported         bool                `json:"supported"`
	Installed         bool                `json:"installed"`
	Missing           []string            `json:"missing"`
	Running           bool                `json:"running"`
	PID               *int64              `json:"pid"`
	Display           *string             `json:"display"`
	Socket            *string             `json:"socket"`
	Geometry          string              `json:"geometry"`
	InstallCommand    *string             `json:"install_command"`
	Browser           *string             `json:"browser"`
	Blocker           *string             `json:"blocker"`
	MemoryAvailableMB *int64              `json:"memory_available_mb"`
	MemoryLimitMB     *int64              `json:"memory_limit_mb"`
	Placement         string              `json:"placement"`
	ImageSwitch       *DisplayImageSwitch `json:"image_switch"`
	Lease             DisplayLease        `json:"lease"`
	ProfileKey        string              `json:"profile_key"`
}

type DisplayStopParams struct {
	Profile *string `json:"profile,omitzero"`
	Force   bool    `json:"force,omitzero"`
}

type DisplayStopResult struct {
	DisplayStatus
	Stopped bool `json:"stopped"`
}

type DisplaySwitchSandboxImageParams struct {
	Profile *string `json:"profile,omitzero"`
	Approve bool    `json:"approve"`
}

type DisplaySwitchSandboxImageResult struct {
	DisplayStatus
	DockerImage string `json:"docker_image"`
}

// Package install started on the gateway host, or the command to run by hand
type DisplayInstallResult struct {
	Started    bool   `json:"started"`
	Command    string `json:"command"`
	ProfileKey string `json:"profile_key"`
}

type DisplayInstallLogPayload struct {
	ProfileKey string `json:"profile_key"`
	Line       string `json:"line"`
}

// End of the install, 0 done, -1 cancelled, -2 no sudo with the command to run by hand streamed
type DisplayInstallDonePayload struct {
	ProfileKey string        `json:"profile_key"`
	Code       int64         `json:"code"`
	Status     DisplayStatus `json:"status"`
}

type DisplayLeasePayload struct {
	ProfileKey string       `json:"profile_key"`
	Lease      DisplayLease `json:"lease"`
}

// Runtime and lease of this profile's screen
var DisplayStatusMethod = NewMethod[ProfileParams, DisplayStatus]("display.status")

// Starts this profile's screen and waits until it is published
var DisplayStart = NewMethod[ProfileParams, DisplayStatus]("display.start")

// Stops the screen, refused with viewer_mismatch while a person holds it unless forced
var DisplayStop = NewMethod[DisplayStopParams, DisplayStopResult]("display.stop")

// Decides the pending default sandbox image switch of this profile
var DisplaySwitchSandboxImage = NewMethod[DisplaySwitchSandboxImageParams, DisplaySwitchSandboxImageResult]("display.switchSandboxImage")

// Runs the package install on the gateway host, progress streamed as display.install.log and done
var DisplayInstall = NewMethod[ProfileParams, DisplayInstallResult]("display.install")

// This profile's screen started or stopped
var DisplayStatusEvent = NewEvent[DisplayStatus]("display.status")

// The takeover lease changed hands
var DisplayLeaseEvent = NewEvent[DisplayLeasePayload]("display.lease")

// One line of package manager output
var DisplayInstallLogEvent = NewEvent[DisplayInstallLogPayload]("display.install.log")

// The install ended
var DisplayInstallDoneEvent = NewEvent[DisplayInstallDonePayload]("display.install.done")
