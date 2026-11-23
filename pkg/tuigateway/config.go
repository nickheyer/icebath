package tuigateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Value of a config key, a word for most keys and a flag for the boolean switches
type ConfigValue struct {
	Text string
	Flag *bool
}

// Config value carrying a word
func TextValue(text string) *ConfigValue { return &ConfigValue{Text: text} }

// Config value carrying a flag
func FlagValue(on bool) *ConfigValue { return &ConfigValue{Flag: &on} }

func (v *ConfigValue) UnmarshalJSON(data []byte) error {
	var flag bool
	if err := json.Unmarshal(data, &flag); err == nil {
		*v = ConfigValue{Flag: &flag}
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("wants text or true or false")
	}
	*v = ConfigValue{Text: text}
	return nil
}

func (v ConfigValue) MarshalJSON() ([]byte, error) {
	if v.Flag != nil {
		return json.Marshal(*v.Flag)
	}
	return json.Marshal(v.Text)
}

// Scope a config write asks for, the session alone or the profile
type ConfigWriteScope string

func (ConfigWriteScope) Values() []string { return []string{"session", "global"} }

// Scope a config answer reports, once for a model pick that lasts one turn
type ConfigScope string

func (ConfigScope) Values() []string { return []string{"session", "global", "once"} }

type ConfigGetParams struct {
	Profile   *string `json:"profile,omitzero"`
	Key       string  `json:"key"`
	Cwd       string  `json:"cwd,omitzero"`
	SessionID string  `json:"session_id,omitzero"`
}

// One provider the config knows, the row of the provider read
type ConfigProviderRef struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Aliases       []string `json:"aliases"`
	Authenticated bool     `json:"authenticated"`
}

// Answer of one config key, value for the simple words and the fields of the composite reads
type ConfigGetResult struct {
	Key          string              `json:"key,omitzero"`
	Value        *ConfigValue        `json:"value,omitzero"`
	Scope        ConfigScope         `json:"scope,omitzero"`
	Display      string              `json:"display,omitzero"`
	ToolProgress string              `json:"tool_progress,omitzero"`
	Model        string              `json:"model,omitzero"`
	Provider     string              `json:"provider,omitzero"`
	Providers    []ConfigProviderRef `json:"providers,omitzero"`
	Home         string              `json:"home,omitzero"`
	Cwd          string              `json:"cwd,omitzero"`
	Branch       string              `json:"branch,omitzero"`
	Config       *ClientConfig       `json:"config,omitzero"`
	Prompt       *string             `json:"prompt,omitzero"`
	Mtime        *float64            `json:"mtime,omitzero"`
	McpRev       string              `json:"mcp_rev,omitzero"`
}

type ConfigSetParams struct {
	Profile               *string          `json:"profile,omitzero"`
	SessionID             string           `json:"session_id,omitzero"`
	Key                   string           `json:"key"`
	Value                 *ConfigValue     `json:"value,omitzero"`
	Scope                 ConfigWriteScope `json:"scope,omitzero"`
	ConfirmExpensiveModel bool             `json:"confirm_expensive_model,omitzero"`
}

// Answer of a config write, the key and its value with the extras the model, focus, cwd and personality writes add
type ConfigSetResult struct {
	Key             string       `json:"key"`
	Value           *ConfigValue `json:"value,omitzero"`
	Warning         string       `json:"warning,omitzero"`
	ConfirmRequired bool         `json:"confirm_required,omitzero"`
	ConfirmMessage  string       `json:"confirm_message,omitzero"`
	Scope           ConfigScope  `json:"scope,omitzero"`
	Deferred        bool         `json:"deferred,omitzero"`
	ToolProgress    string       `json:"tool_progress,omitzero"`
	Cwd             string       `json:"cwd,omitzero"`
	Branch          string       `json:"branch,omitzero"`
	HistoryReset    bool         `json:"history_reset,omitzero"`
	Info            *SessionInfo `json:"info,omitzero"`
}

type ThemeMode string

func (ThemeMode) Values() []string { return []string{"auto", "light", "dark"} }

type BusyInputMode string

func (BusyInputMode) Values() []string { return []string{"queue", "steer", "interrupt"} }

type IndicatorStyle string

func (IndicatorStyle) Values() []string { return []string{"unicode", "emoji", "ascii"} }

type DetailsMode string

func (DetailsMode) Values() []string { return []string{"hidden", "collapsed", "expanded"} }

type StatusBarPlace string

func (StatusBarPlace) Values() []string { return []string{"top", "bottom", "off"} }

type MouseTracking string

func (MouseTracking) Values() []string { return []string{"all", "off", "wheel", "buttons"} }

type VoiceSubmitMode string

func (VoiceSubmitMode) Values() []string { return []string{"direct", "draft"} }

type PetRenderMode string

func (PetRenderMode) Values() []string {
	return []string{"auto", "kitty", "iterm", "sixel", "unicode", "off"}
}

// Status bar fields to show, every field when empty
type ClientStatusBar struct {
	Fields []string `json:"fields"`
}

type ClientPet struct {
	RenderMode PetRenderMode `json:"render_mode"`
}

// Display settings the terminal UI follows, keyed as in the display section and normalized with defaults filled
type ClientDisplay struct {
	BellOnComplete   bool                   `json:"bell_on_complete"`
	BellOnPrompt     bool                   `json:"bell_on_prompt"`
	Theme            ThemeMode              `json:"tui_theme"`
	Battery          bool                   `json:"battery"`
	BusyInputMode    BusyInputMode          `json:"busy_input_mode"`
	Compact          bool                   `json:"tui_compact"`
	DetailsMode      DetailsMode            `json:"details_mode"`
	Sections         map[string]DetailsMode `json:"sections"`
	FocusView        bool                   `json:"focus_view"`
	Indicator        IndicatorStyle         `json:"tui_status_indicator"`
	InlineDiffs      bool                   `json:"inline_diffs"`
	InlineImages     bool                   `json:"inline_images"`
	MouseTracking    MouseTracking          `json:"mouse_tracking"`
	ShowReasoning    bool                   `json:"show_reasoning"`
	StatusBarPlace   StatusBarPlace         `json:"tui_statusbar"`
	StatusBar        ClientStatusBar        `json:"status_bar"`
	Streaming        bool                   `json:"streaming"`
	Timestamps       bool                   `json:"timestamps"`
	Language         string                 `json:"language"`
	AgentsNudge      bool                   `json:"tui_agents_nudge"`
	AutoResumeRecent bool                   `json:"tui_auto_resume_recent"`
	TurnSummary      bool                   `json:"turn_summary"`
	SpinnerTokenFlow bool                   `json:"spinner_token_flow"`
	Pet              ClientPet              `json:"pet"`
	// The arena card's own settings, checked by the card that reads them
	Arena any `json:"arena,omitzero"`
}

type ClientApprovals struct {
	DestructiveSlashConfirm bool `json:"destructive_slash_confirm"`
}

type ClientVoice struct {
	RecordKey  string          `json:"record_key"`
	SubmitMode VoiceSubmitMode `json:"submit_mode"`
}

// Settings of a profile the terminal UI follows, the answer of config.get full
type ClientConfig struct {
	Display                    ClientDisplay   `json:"display"`
	Approvals                  ClientApprovals `json:"approvals"`
	Voice                      ClientVoice     `json:"voice"`
	PasteCollapseThreshold     int64           `json:"paste_collapse_threshold"`
	PasteCollapseCharThreshold int64           `json:"paste_collapse_char_threshold"`
}

// Reads one config key, or with key full the settings the terminal UI follows, on both routes
var ConfigGet = NewMethod[ConfigGetParams, ConfigGetResult]("config.get")

// Changes one config key, persisted or for the session, and answers the value it took, on both routes
var ConfigSet = NewMethod[ConfigSetParams, ConfigSetResult]("config.set")

// Typed word of a setting the terminal UI takes from an enum, the type's first value when the setting holds another
func choiceOf[T interface {
	~string
	Values() []string
}](word string) T {
	if slices.Contains(T("").Values(), word) {
		return T(word)
	}
	return T(T("").Values()[0])
}

// Fold states set for single transcript parts, parts without their own left out
func sectionsOf(ctx context.Context) map[string]DetailsMode {
	out := map[string]DetailsMode{}
	for part, raw := range settings.DetailSectionModes.Get(ctx) {
		mode, _ := raw.(string)
		if slices.Contains(settings.DetailSections, part) && slices.Contains(DetailsMode("").Values(), mode) {
			out[part] = DetailsMode(mode)
		}
	}
	return out
}

// Status bar fields to show, lowered and each once
func fieldsOf(ctx context.Context) []string {
	var out []string
	for _, name := range settings.StatusBarFields.Strings(ctx) {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Settings the terminal UI follows for ctx: its home, its session's own values, and the terminal UI's display overrides
func ClientConfigOf(ctx context.Context) ClientConfig {
	ctx = settings.WithPlatform(ctx, settings.LocalPlatform)
	return ClientConfig{
		Display: ClientDisplay{
			BellOnComplete:   settings.BellOnComplete.Get(ctx),
			BellOnPrompt:     settings.BellOnPrompt.Get(ctx),
			Theme:            choiceOf[ThemeMode](settings.Theme.Get(ctx)),
			Battery:          settings.Battery.Get(ctx),
			BusyInputMode:    choiceOf[BusyInputMode](settings.BusyMode.Get(ctx)),
			Compact:          settings.Compact.Get(ctx),
			DetailsMode:      choiceOf[DetailsMode](settings.DetailsMode.Get(ctx)),
			Sections:         sectionsOf(ctx),
			FocusView:        settings.DetailLevel.Get(ctx) == "minimal",
			Indicator:        choiceOf[IndicatorStyle](settings.BusyIndicator.Get(ctx)),
			InlineDiffs:      settings.DisplayInlineDiffs.Get(ctx),
			InlineImages:     settings.InlineImages.Get(ctx) != "off",
			MouseTracking:    choiceOf[MouseTracking](settings.Mouse.Get(ctx)),
			ShowReasoning:    settings.ShowThinking.Get(ctx),
			StatusBarPlace:   choiceOf[StatusBarPlace](settings.StatusBar.Get(ctx)),
			StatusBar:        ClientStatusBar{Fields: fieldsOf(ctx)},
			Streaming:        settings.DisplayStreaming.Get(ctx),
			Timestamps:       settings.Timestamps.Get(ctx),
			Language:         strings.TrimSpace(settings.Language.Get(ctx)),
			AgentsNudge:      settings.DisplayTuiAgentsNudge.Get(ctx),
			AutoResumeRecent: settings.ResumeLast.Get(ctx),
			TurnSummary:      settings.DisplayTurnSummary.Get(ctx),
			SpinnerTokenFlow: settings.DisplaySpinnerTokenFlow.Get(ctx),
			Pet:              ClientPet{RenderMode: choiceOf[PetRenderMode](settings.DisplayPetRenderMode.Get(ctx))},
			Arena:            config.RedactConfigValue(settings.DisplayArena.Get(ctx)),
		},
		Approvals:                  ClientApprovals{DestructiveSlashConfirm: settings.ConfirmDestructive.Get(ctx)},
		Voice:                      ClientVoice{RecordKey: strings.TrimSpace(settings.RecordKey.Get(ctx)), SubmitMode: choiceOf[VoiceSubmitMode](settings.VoiceSubmit.Get(ctx))},
		PasteCollapseThreshold:     int64(settings.PasteCollapseThreshold.Get(ctx)),
		PasteCollapseCharThreshold: int64(settings.PasteCollapseCharThreshold.Get(ctx)),
	}
}

// Settings the terminal UI follows when it could not read them, every one at its default
func DefaultClientConfig() ClientConfig {
	return ClientConfigOf(settings.DefaultsOnly(context.Background()))
}
