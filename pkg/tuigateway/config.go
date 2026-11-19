package tuigateway

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
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
	MouseTracking    MouseTracking          `json:"mouse_tracking"`
	ShowReasoning    bool                   `json:"show_reasoning"`
	StatusBarPlace   StatusBarPlace         `json:"tui_statusbar"`
	StatusBar        ClientStatusBar        `json:"status_bar"`
	Streaming        bool                   `json:"streaming"`
	Timestamps       bool                   `json:"timestamps"`
	Language         string                 `json:"language"`
	AgentsNudge      bool                   `json:"tui_agents_nudge"`
	AutoResumeRecent bool                   `json:"tui_auto_resume_recent"`
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

// Mouse tracking spellings mapped to off, wheel, buttons or all
var MouseTrackingAliases = map[string]string{
	"0": "off", "1": "all", "all": "all", "any": "all", "button": "buttons", "buttons": "buttons",
	"click": "buttons", "false": "off", "full": "all", "no": "off", "off": "off", "on": "all",
	"scroll": "wheel", "true": "all", "wheel": "wheel", "yes": "all",
}

var (
	falseWords      = map[string]bool{"0": true, "off": true, "false": true, "no": true}
	sectionNames    = []string{"thinking", "tools", "subagents", "activity"}
	thinkingDetails = map[string]DetailsMode{"collapsed": "collapsed", "full": "expanded", "truncated": "collapsed"}
	statusBarWords  = map[string]StatusBarPlace{"bottom": "bottom", "off": "off", "on": "top", "top": "top"}
)

// Lowered trimmed text of a config value, empty for anything but text
func wordOf(v any) string {
	text, _ := v.(string)
	return strings.ToLower(strings.TrimSpace(text))
}

// Word of a config value when the enum allows it, else the default
func choiceOf[T interface {
	~string
	Values() []string
}](v any, def T) T {
	if word := wordOf(v); slices.Contains(def.Values(), word) {
		return T(word)
	}
	return def
}

// Number of a config value, false for anything but a number
func numberOf(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

// Flag of a config value, off words and zero false, other text and numbers true, a missing value the default
func flagOf(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case bool:
		return x
	case string:
		word := strings.ToLower(strings.TrimSpace(x))
		return word != "" && !falseWords[word]
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if n, isNumber := numberOf(v); isNumber {
		return n != 0
	}
	return def
}

// Paste collapse threshold of a config value, a non negative number or leading integer text, else the default
func thresholdOf(v any, def int64) int64 {
	if text, isText := v.(string); isText {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		end := 0
		if end < len(text) && (text[end] == '+' || text[end] == '-') {
			end++
		}
		digits := end
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if n, err := strconv.ParseInt(text[:end], 10, 64); end > digits && err == nil && n >= 0 {
			return n
		}
		return def
	}
	if n, isNumber := numberOf(v); isNumber && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		return int64(math.Floor(n + 0.5))
	}
	return def
}

// Mouse tracking mode of a display section, the legacy tui_mouse only when mouse_tracking is absent
func MouseTrackingOf(display map[string]any) MouseTracking {
	raw, present := display["mouse_tracking"]
	if !present {
		if raw, present = display["tui_mouse"]; !present {
			return "all"
		}
	}
	if text, isText := raw.(string); isText {
		if mode, known := MouseTrackingAliases[strings.ToLower(strings.TrimSpace(text))]; known {
			return MouseTracking(mode)
		}
		return "all"
	}
	if n, isNumber := numberOf(raw); raw == false || (isNumber && n == 0) {
		return "off"
	}
	return "all"
}

// Global details mode from details_mode, then the legacy thinking_mode, then collapsed
func detailsModeOf(display map[string]any) DetailsMode {
	if mode := choiceOf(display["details_mode"], DetailsMode("")); mode != "" {
		return mode
	}
	if mode, known := thinkingDetails[wordOf(display["thinking_mode"])]; known {
		return mode
	}
	return "collapsed"
}

// Per section details overrides, unknown sections and modes dropped
func sectionsOf(raw any) map[string]DetailsMode {
	out := map[string]DetailsMode{}
	obj, _ := raw.(map[string]any)
	for name, v := range obj {
		if mode := choiceOf(v, DetailsMode("")); mode != "" && slices.Contains(sectionNames, name) {
			out[name] = mode
		}
	}
	return out
}

// Status bar fields named in a list, lowered, each once
func fieldsOf(raw any) []string {
	list, _ := raw.([]any)
	var out []string
	for _, item := range list {
		if name := wordOf(item); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Settings the terminal UI follows, read from a config whose display section is the local one
func ClientConfigOf(cfg map[string]any) ClientConfig {
	display, _ := cfg["display"].(map[string]any)
	approvals, _ := cfg["approvals"].(map[string]any)
	voice, _ := cfg["voice"].(map[string]any)
	statusBar, _ := display["status_bar"].(map[string]any)
	pet, _ := display["pet"].(map[string]any)
	recordKey, _ := voice["record_key"].(string)
	language, _ := display["language"].(string)
	place := StatusBarPlace("top")
	if raw := display["tui_statusbar"]; raw == false {
		place = "off"
	} else if word, known := statusBarWords[wordOf(raw)]; known {
		place = word
	}
	return ClientConfig{
		Display: ClientDisplay{
			BellOnComplete:   flagOf(display["bell_on_complete"], false),
			BellOnPrompt:     flagOf(display["bell_on_prompt"], false),
			Theme:            choiceOf(display["tui_theme"], ThemeMode("auto")),
			Battery:          flagOf(display["battery"], false),
			BusyInputMode:    choiceOf(display["busy_input_mode"], BusyInputMode("queue")),
			Compact:          flagOf(display["tui_compact"], false),
			DetailsMode:      detailsModeOf(display),
			Sections:         sectionsOf(display["sections"]),
			FocusView:        flagOf(display["focus_view"], false),
			Indicator:        choiceOf(display["tui_status_indicator"], IndicatorStyle("unicode")),
			InlineDiffs:      flagOf(display["inline_diffs"], true),
			MouseTracking:    MouseTrackingOf(display),
			ShowReasoning:    flagOf(display["show_reasoning"], true),
			StatusBarPlace:   place,
			StatusBar:        ClientStatusBar{Fields: fieldsOf(statusBar["fields"])},
			Streaming:        flagOf(display["streaming"], true),
			Timestamps:       flagOf(display["timestamps"], false),
			Language:         strings.TrimSpace(language),
			AgentsNudge:      flagOf(display["tui_agents_nudge"], true),
			AutoResumeRecent: flagOf(display["tui_auto_resume_recent"], false),
			Pet:              ClientPet{RenderMode: choiceOf(pet["render_mode"], PetRenderMode("auto"))},
			Arena:            display["arena"],
		},
		Approvals:                  ClientApprovals{DestructiveSlashConfirm: flagOf(approvals["destructive_slash_confirm"], true)},
		Voice:                      ClientVoice{RecordKey: strings.TrimSpace(recordKey), SubmitMode: choiceOf(voice["submit_mode"], VoiceSubmitMode("direct"))},
		PasteCollapseThreshold:     thresholdOf(cfg["paste_collapse_threshold"], 5),
		PasteCollapseCharThreshold: thresholdOf(cfg["paste_collapse_char_threshold"], 2000),
	}
}
