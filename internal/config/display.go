package config

import "gopkg.in/yaml.v3"

// Display section, with the per-platform display keys a gateway also reads globally kept unset unless written
type DisplayConfig struct {
	Compact                        bool                 `yaml:"compact" coerce:"bool"`
	Personality                    string               `yaml:"personality"`
	ResumeDisplay                  ResumeDisplay        `yaml:"resume_display"`
	ResumeExchanges                int                  `yaml:"resume_exchanges"`
	ResumeMaxUserChars             int                  `yaml:"resume_max_user_chars"`
	ResumeMaxAssistantChars        int                  `yaml:"resume_max_assistant_chars"`
	ResumeMaxAssistantLines        int                  `yaml:"resume_max_assistant_lines"`
	ResumeSkipToolOnly             bool                 `yaml:"resume_skip_tool_only" coerce:"bool"`
	BusyInputMode                  BusyInputMode        `yaml:"busy_input_mode"`
	BusySteerAckEnabled            bool                 `yaml:"busy_steer_ack_enabled"`
	CLIMultilineShortcuts          bool                 `yaml:"cli_multiline_shortcuts" coerce:"notoff"`
	Interface                      InterfaceKind        `yaml:"interface"`
	CLILayout                      CLILayout            `yaml:"cli_layout"`
	TUINative                      bool                 `yaml:"tui_native"`
	TUIAutoResumeRecent            bool                 `yaml:"tui_auto_resume_recent"`
	ResumeLastSession              bool                 `yaml:"resume_last_session"`
	TUIAgentsNudge                 bool                 `yaml:"tui_agents_nudge"`
	BellOnComplete                 bool                 `yaml:"bell_on_complete" coerce:"bool"`
	BellOnPrompt                   bool                 `yaml:"bell_on_prompt" coerce:"bool"`
	ShowReasoning                  bool                 `yaml:"show_reasoning" coerce:"bool"`
	ReasoningFull                  bool                 `yaml:"reasoning_full" coerce:"bool"`
	MemoryNotifications            Scalar               `yaml:"memory_notifications"`
	BackgroundProcessNotifications Scalar               `yaml:"background_process_notifications"`
	Streaming                      bool                 `yaml:"streaming" coerce:"bool"`
	Timestamps                     bool                 `yaml:"timestamps" coerce:"bool"`
	TimestampFormat                string               `yaml:"timestamp_format"`
	FinalResponseMarkdown          FinalMarkdown        `yaml:"final_response_markdown"`
	PersistentOutput               bool                 `yaml:"persistent_output" coerce:"bool"`
	PersistentOutputMaxLines       int                  `yaml:"persistent_output_max_lines"`
	CLIRebuildScrollbackOnRedraw   bool                 `yaml:"cli_rebuild_scrollback_on_redraw"`
	PersistPrompts                 bool                 `yaml:"persist_prompts" coerce:"bool"`
	InlineDiffs                    bool                 `yaml:"inline_diffs" coerce:"bool"`
	FileMutationVerifier           bool                 `yaml:"file_mutation_verifier" coerce:"bool"`
	CreditsNotices                 bool                 `yaml:"credits_notices" coerce:"bool"`
	TurnCompletionExplainer        bool                 `yaml:"turn_completion_explainer" coerce:"bool"`
	ShowCost                       bool                 `yaml:"show_cost"`
	Battery                        bool                 `yaml:"battery" coerce:"bool"`
	FocusView                      bool                 `yaml:"focus_view" coerce:"bool"`
	FocusSavedToolProgress         Scalar               `yaml:"focus_saved_tool_progress"`
	Skin                           string               `yaml:"skin"`
	Language                       string               `yaml:"language"`
	TUIStatusIndicator             StatusIndicator      `yaml:"tui_status_indicator"`
	CLIRefreshInterval             float64              `yaml:"cli_refresh_interval"`
	VimMode                        bool                 `yaml:"vim_mode" coerce:"bool"`
	UserMessagePreview             MessagePreview       `yaml:"user_message_preview"`
	InterimAssistantMessages       bool                 `yaml:"interim_assistant_messages"`
	SuppressWarningNotifications   bool                 `yaml:"suppress_warning_notifications" coerce:"truthwords"`
	ShowCommentary                 bool                 `yaml:"show_commentary" coerce:"bool"`
	ToolProgressCommand            bool                 `yaml:"tool_progress_command"`
	ToolPreviewLength              int                  `yaml:"tool_preview_length"`
	FriendlyToolLabels             bool                 `yaml:"friendly_tool_labels"`
	TurnSummary                    bool                 `yaml:"turn_summary" coerce:"bool"`
	SpinnerTokenFlow               bool                 `yaml:"spinner_token_flow" coerce:"bool"`
	ToolProgressGrouping           ProgressGrouping     `yaml:"tool_progress_grouping"`
	StatusPhrases                  StatusPhrases        `yaml:"status_phrases"`
	ReasoningStyle                 ReasoningStyle       `yaml:"reasoning_style"`
	EphemeralSystemTTL             int                  `yaml:"ephemeral_system_ttl"`
	Platforms                      Map[DisplayPlatform] `yaml:"platforms"`
	RuntimeFooter                  RuntimeFooter        `yaml:"runtime_footer"`
	StatusBar                      StatusBarConfig      `yaml:"status_bar"`
	CopyShortcut                   CopyShortcut         `yaml:"copy_shortcut"`
	Pet                            PetConfig            `yaml:"pet"`
	ToolProgress                   Scalar               `yaml:"tool_progress,omitempty"`
	LongRunningNotifications       Scalar               `yaml:"long_running_notifications,omitempty"`
	BusyAckDetail                  Scalar               `yaml:"busy_ack_detail,omitempty"`
	CleanupProgress                Scalar               `yaml:"cleanup_progress,omitempty"`
	LiveStatus                     Scalar               `yaml:"live_status,omitempty"`
	ThinkingProgress               Scalar               `yaml:"thinking_progress,omitempty"`
	ToolProgressOverrides          Map[Scalar]          `yaml:"tool_progress_overrides,omitempty"`
	GenericStatusPhrases           *StatusPhrases       `yaml:"generic_status_phrases,omitempty"`
	Statusbar                      Scalar               `yaml:"statusbar,omitempty"`
	TUIStatusbar                   Scalar               `yaml:"tui_statusbar,omitempty"`
	MouseTracking                  Scalar               `yaml:"mouse_tracking,omitempty"`
	TUIMouse                       Scalar               `yaml:"tui_mouse,omitempty"`
	MessageReactions               *bool                `yaml:"message_reactions,omitempty" coerce:"bool"`
	InAppTips                      *bool                `yaml:"in_app_tips,omitempty" coerce:"bool"`
	InAppTours                     *bool                `yaml:"in_app_tours,omitempty" coerce:"bool"`
	Sections                       Map[Raw]             `yaml:"sections,omitempty"`
	rest                           rest
}

type displayFields DisplayConfig

// Decodes the mapping and keeps unknown keys
func (d *DisplayConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*displayFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DisplayConfig) MarshalYAML() (any, error) {
	return encodeMapping(displayFields(d), d.rest)
}

// Per-platform display override, every key unset unless written so resolution falls through to the global value
type DisplayPlatform struct {
	Streaming                    Scalar         `yaml:"streaming,omitempty"`
	ToolProgress                 Scalar         `yaml:"tool_progress,omitempty"`
	ToolProgressGrouping         Scalar         `yaml:"tool_progress_grouping,omitempty"`
	ShowReasoning                Scalar         `yaml:"show_reasoning,omitempty"`
	ReasoningStyle               Scalar         `yaml:"reasoning_style,omitempty"`
	ToolPreviewLength            Scalar         `yaml:"tool_preview_length,omitempty"`
	InterimAssistantMessages     Scalar         `yaml:"interim_assistant_messages,omitempty"`
	SuppressWarningNotifications Scalar         `yaml:"suppress_warning_notifications,omitempty"`
	LongRunningNotifications     Scalar         `yaml:"long_running_notifications,omitempty"`
	BusyAckDetail                Scalar         `yaml:"busy_ack_detail,omitempty"`
	BusySteerAckEnabled          Scalar         `yaml:"busy_steer_ack_enabled,omitempty"`
	ThinkingProgress             Scalar         `yaml:"thinking_progress,omitempty"`
	CleanupProgress              Scalar         `yaml:"cleanup_progress,omitempty"`
	LiveStatus                   Scalar         `yaml:"live_status,omitempty"`
	MemoryNotifications          Scalar         `yaml:"memory_notifications,omitempty"`
	FriendlyToolLabels           Scalar         `yaml:"friendly_tool_labels,omitempty"`
	StatusPhrases                *StatusPhrases `yaml:"status_phrases,omitempty"`
	GenericStatusPhrases         *StatusPhrases `yaml:"generic_status_phrases,omitempty"`
	RuntimeFooter                *RuntimeFooter `yaml:"runtime_footer,omitempty"`
	rest                         rest
}

type displayPlatformFields DisplayPlatform

// Decodes the mapping and keeps unknown keys
func (d *DisplayPlatform) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*displayPlatformFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DisplayPlatform) MarshalYAML() (any, error) {
	return encodeMapping(displayPlatformFields(d), d.rest)
}

// Display keys a platform may override, in the order the gateway resolves them
func DisplayOverrideKeys() []string {
	return []string{
		"tool_progress", "tool_progress_grouping", "show_reasoning", "reasoning_style", "tool_preview_length",
		"streaming", "interim_assistant_messages", "suppress_warning_notifications", "long_running_notifications",
		"busy_ack_detail", "busy_steer_ack_enabled", "cleanup_progress", "live_status",
	}
}

// Custom long-running status phrase catalog
type StatusPhrases struct {
	Path    StringList     `yaml:"path,omitempty"`
	Paths   StringList     `yaml:"paths,omitempty"`
	Mode    PhraseMode     `yaml:"mode,omitempty"`
	Phrases *PhraseSurface `yaml:"phrases,omitempty"`
	Status  []string       `yaml:"status,omitempty"`
	Generic []string       `yaml:"generic,omitempty"`
	rest    rest
}

type statusPhrasesFields StatusPhrases

// Decodes the mapping and keeps unknown keys
func (s *StatusPhrases) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*statusPhrasesFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s StatusPhrases) MarshalYAML() (any, error) {
	return encodeMapping(statusPhrasesFields(s), s.rest)
}

// Phrase lists by status surface
type PhraseSurface struct {
	Status  []string `yaml:"status,omitempty"`
	Generic []string `yaml:"generic,omitempty"`
	rest    rest
}

type phraseSurfaceFields PhraseSurface

// Decodes the mapping and keeps unknown keys
func (p *PhraseSurface) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*phraseSurfaceFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PhraseSurface) MarshalYAML() (any, error) {
	return encodeMapping(phraseSurfaceFields(p), p.rest)
}

// Whether custom phrases extend or replace the built-ins
type PhraseMode string

// Phrase modes
const (
	PhraseAppend  PhraseMode = "append"
	PhraseReplace PhraseMode = "replace"
)

// Accepted phrase modes, any other value reads as append
func PhraseModes() []PhraseMode {
	return []PhraseMode{PhraseAppend, PhraseReplace}
}

// Lines of submitted user messages echoed to scrollback
type MessagePreview struct {
	FirstLines int `yaml:"first_lines"`
	LastLines  int `yaml:"last_lines"`
	rest       rest
}

type messagePreviewFields MessagePreview

// Decodes the mapping and keeps unknown keys
func (m *MessagePreview) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*messagePreviewFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MessagePreview) MarshalYAML() (any, error) {
	return encodeMapping(messagePreviewFields(m), m.rest)
}

// Gateway runtime footer on the final message, a per-platform block overriding only the keys it writes
type RuntimeFooter struct {
	Enabled *bool    `yaml:"enabled,omitempty" coerce:"bool"`
	Fields  []string `yaml:"fields,omitempty"`
	rest    rest
}

type runtimeFooterFields RuntimeFooter

// Decodes the mapping and keeps unknown keys
func (r *RuntimeFooter) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*runtimeFooterFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r RuntimeFooter) MarshalYAML() (any, error) {
	return encodeMapping(runtimeFooterFields(r), r.rest)
}

// Runtime footer field names
func RuntimeFooterFields() []string {
	return []string{"model", "context_pct", "cwd", "latency", "served_model"}
}

// CLI and TUI status bar fields
type StatusBarConfig struct {
	Fields []string `yaml:"fields"`
	rest   rest
}

type statusBarFields StatusBarConfig

// Decodes the mapping and keeps unknown keys
func (s *StatusBarConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*statusBarFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s StatusBarConfig) MarshalYAML() (any, error) {
	return encodeMapping(statusBarFields(s), s.rest)
}

// Status bar field names
func StatusBarFields() []string {
	return []string{
		"model", "context_detail", "context_pct", "cache_hit", "latency", "tps", "compressions", "bg_tasks",
		"bg_processes", "bg_subagents", "goal", "git_branch", "duration", "prompt_elapsed", "idle_since", "focus",
		"yolo", "stash", "battery", "title", "total_tokens",
	}
}

// Animated pet mascot
type PetConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Slug        string        `yaml:"slug"`
	RenderMode  PetRenderMode `yaml:"render_mode"`
	Scale       float64       `yaml:"scale"`
	UnicodeCols int           `yaml:"unicode_cols"`
	rest        rest
}

type petFields PetConfig

// Decodes the mapping and keeps unknown keys
func (p *PetConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*petFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PetConfig) MarshalYAML() (any, error) {
	return encodeMapping(petFields(p), p.rest)
}

// Resume recap style
type ResumeDisplay string

// Resume recap styles
const (
	ResumeFull    ResumeDisplay = "full"
	ResumeMinimal ResumeDisplay = "minimal"
	ResumeOff     ResumeDisplay = "off"
)

// Accepted resume recap styles
func ResumeDisplays() []ResumeDisplay {
	return []ResumeDisplay{ResumeFull, ResumeMinimal, ResumeOff}
}

// What a message typed during a running turn does
type BusyInputMode string

// Busy input modes
const (
	BusyInterrupt BusyInputMode = "interrupt"
	BusyQueue     BusyInputMode = "queue"
	BusySteer     BusyInputMode = "steer"
)

// Accepted busy input modes
func BusyInputModes() []BusyInputMode {
	return []BusyInputMode{BusyInterrupt, BusyQueue, BusySteer}
}

// Interactive front end that bare icebath launches
type InterfaceKind string

// Front ends
const (
	InterfaceCLI InterfaceKind = "cli"
	InterfaceTUI InterfaceKind = "tui"
)

// Accepted front ends
func InterfaceKinds() []InterfaceKind {
	return []InterfaceKind{InterfaceCLI, InterfaceTUI}
}

// Look of the classic REPL
type CLILayout string

// REPL looks
const (
	LayoutModern  CLILayout = "modern"
	LayoutClassic CLILayout = "classic"
)

// Accepted REPL looks
func CLILayouts() []CLILayout {
	return []CLILayout{LayoutModern, LayoutClassic}
}

// Memory notification levels
func MemoryNotificationLevels() []string {
	return []string{"off", "on", "verbose"}
}

// Background process notification levels
func BackgroundNotificationLevels() []string {
	return []string{"concise", "all", "result", "error", "off"}
}

// Tool progress modes, booleans reading as all and off
func ToolProgressModes() []string {
	return []string{"off", "new", "all", "verbose", "log"}
}

// Live status levels, booleans reading as full and off
func LiveStatusLevels() []string {
	return []string{"full", "verb", "off"}
}

// Final response rendering
type FinalMarkdown string

// Final response renderings
const (
	MarkdownRender FinalMarkdown = "render"
	MarkdownStrip  FinalMarkdown = "strip"
	MarkdownRaw    FinalMarkdown = "raw"
)

// Accepted final response renderings
func FinalMarkdowns() []FinalMarkdown {
	return []FinalMarkdown{MarkdownRender, MarkdownStrip, MarkdownRaw}
}

// TUI busy indicator style
type StatusIndicator string

// Busy indicator styles
const (
	IndicatorKaomoji StatusIndicator = "kaomoji"
	IndicatorEmoji   StatusIndicator = "emoji"
	IndicatorUnicode StatusIndicator = "unicode"
	IndicatorASCII   StatusIndicator = "ascii"
)

// Accepted busy indicator styles
func StatusIndicators() []StatusIndicator {
	return []StatusIndicator{IndicatorKaomoji, IndicatorEmoji, IndicatorUnicode, IndicatorASCII}
}

// Gateway tool progress bubble grouping
type ProgressGrouping string

// Progress groupings
const (
	GroupAccumulate ProgressGrouping = "accumulate"
	GroupSeparate   ProgressGrouping = "separate"
)

// Accepted progress groupings, any other value reads as accumulate
func ProgressGroupings() []ProgressGrouping {
	return []ProgressGrouping{GroupAccumulate, GroupSeparate}
}

// Reasoning summary rendering
type ReasoningStyle string

// Reasoning renderings
const (
	ReasoningCode       ReasoningStyle = "code"
	ReasoningBlockquote ReasoningStyle = "blockquote"
	ReasoningSubtext    ReasoningStyle = "subtext"
)

// Accepted reasoning renderings, any other value reads as code
func ReasoningStyles() []ReasoningStyle {
	return []ReasoningStyle{ReasoningCode, ReasoningBlockquote, ReasoningSubtext}
}

// Copy key binding
type CopyShortcut string

// Copy key bindings
const (
	CopyAuto       CopyShortcut = "auto"
	CopyCtrlC      CopyShortcut = "ctrl_c"
	CopyCtrlShiftC CopyShortcut = "ctrl_shift_c"
	CopyDisabled   CopyShortcut = "disabled"
)

// Accepted copy key bindings
func CopyShortcuts() []CopyShortcut {
	return []CopyShortcut{CopyAuto, CopyCtrlC, CopyCtrlShiftC, CopyDisabled}
}

// Pet sprite render protocol
type PetRenderMode string

// Pet render protocols
const (
	PetAuto    PetRenderMode = "auto"
	PetKitty   PetRenderMode = "kitty"
	PetITerm   PetRenderMode = "iterm"
	PetSixel   PetRenderMode = "sixel"
	PetUnicode PetRenderMode = "unicode"
	PetOff     PetRenderMode = "off"
)

// Accepted pet render protocols
func PetRenderModes() []PetRenderMode {
	return []PetRenderMode{PetAuto, PetKitty, PetITerm, PetSixel, PetUnicode, PetOff}
}

// UI languages for static messages, unknown reading as en
func UILanguages() []string {
	return []string{"en", "zh", "ja", "de", "es", "fr", "tr", "uk"}
}

// Defaults of the display section
func defaultDisplay() DisplayConfig {
	return DisplayConfig{
		ResumeDisplay:                  ResumeFull,
		ResumeExchanges:                10,
		ResumeMaxUserChars:             300,
		ResumeMaxAssistantChars:        200,
		ResumeMaxAssistantLines:        3,
		ResumeSkipToolOnly:             true,
		BusyInputMode:                  BusyInterrupt,
		BusySteerAckEnabled:            true,
		CLIMultilineShortcuts:          true,
		Interface:                      InterfaceCLI,
		CLILayout:                      LayoutModern,
		ResumeLastSession:              true,
		TUIAgentsNudge:                 true,
		ShowReasoning:                  true,
		MemoryNotifications:            Text("on"),
		BackgroundProcessNotifications: Text("concise"),
		TimestampFormat:                "%H:%M",
		FinalResponseMarkdown:          MarkdownStrip,
		PersistentOutput:               true,
		PersistentOutputMaxLines:       200,
		PersistPrompts:                 true,
		InlineDiffs:                    true,
		FileMutationVerifier:           true,
		CreditsNotices:                 true,
		TurnCompletionExplainer:        true,
		FocusSavedToolProgress:         Text("all"),
		Skin:                           "default",
		Language:                       "en",
		TUIStatusIndicator:             IndicatorKaomoji,
		CLIRefreshInterval:             1.0,
		UserMessagePreview:             MessagePreview{FirstLines: 2, LastLines: 2},
		InterimAssistantMessages:       true,
		ShowCommentary:                 true,
		FriendlyToolLabels:             true,
		TurnSummary:                    true,
		SpinnerTokenFlow:               true,
		ToolProgressGrouping:           GroupAccumulate,
		ReasoningStyle:                 ReasoningCode,
		Platforms: MapOf(
			[]string{"telegram", "discord", "slack", "wecom"},
			[]DisplayPlatform{{Streaming: Bool(true)}, {Streaming: Bool(false)}, {Streaming: Bool(false)}, {Streaming: Bool(true)}},
		),
		RuntimeFooter: RuntimeFooter{Enabled: new(false), Fields: []string{"model", "context_pct", "cwd"}},
		StatusBar:     StatusBarConfig{Fields: []string{}},
		CopyShortcut:  CopyAuto,
		Pet:           PetConfig{RenderMode: PetAuto, Scale: 0.33},
	}
}

// Documentation of the display section
func displayDocs() map[string]string {
	return map[string]string{
		"display.compact":                          "Compact CLI output.",
		"display.personality":                      "Active personality name.",
		"display.resume_display":                   "Recap shown on /resume and startup resume: full | minimal | off.",
		"display.resume_exchanges":                 "Max user+assistant pairs to show in the resume recap.",
		"display.resume_max_user_chars":            "Truncate user message text in the recap.",
		"display.resume_max_assistant_chars":       "Truncate non-last assistant text in the recap.",
		"display.resume_max_assistant_lines":       "Truncate non-last assistant lines in the recap.",
		"display.resume_skip_tool_only":            "Skip tool-call-only assistant entries in the recap so it isn't dominated by `[2 tool calls: ...]` lines; false shows them inline.",
		"display.busy_input_mode":                  "What a message sent during a running turn does: interrupt | queue | steer.",
		"display.busy_steer_ack_enabled":           "Steer mode: false hides only the \"Steered into current run\" bubble; steering itself still happens.",
		"display.cli_multiline_shortcuts":          "Classic CLI multiline beyond Alt+Enter: Ctrl+J newline, trailing backslash+Enter continues, Shift+Enter reported distinctly. false restores the c-j submit fallback for POSIX PTYs whose plain Enter arrives as LF.",
		"display.interface":                        "`icebath` and `icebath chat` launch \"cli\" (classic REPL) | \"tui\" (full-screen); --cli, --tui, ICEBATH_TUI=1 win.",
		"display.cli_layout":                       "The REPL's look: \"modern\" (glyph rows, status under the composer) | \"classic\" (boxed panels).",
		"display.tui_native":                       "Native TUI uses the terminal's primary buffer and scrollback instead of the custom alternate-screen viewport. Flags win: `--native` / `--tui-native` and `--cli`.",
		"display.tui_auto_resume_recent":           "`icebath --tui` auto-resumes the most recent human-facing session (like `icebath -c`). ICEBATH_TUI_RESUME=<id> always wins.",
		"display.resume_last_session":              "Desktop reopens the last chat or page on cold start (also in Settings -> Appearance).",
		"display.tui_agents_nudge":                 "One-time TUI hint (\"subagents working, /agents to watch live\") on first delegation.",
		"display.bell_on_complete":                 "Ring the terminal bell when a turn completes.",
		"display.bell_on_prompt":                   "Bell when a blocking prompt opens (clarify, approval, sudo).",
		"display.show_reasoning":                   "Stream reasoning live before the response; otherwise thinking models show only a spinner for tens of seconds.",
		"display.reasoning_full":                   "The post-response \"Reasoning\" recap collapses to 10 lines; true prints it all (live streaming is always full).",
		"display.memory_notifications":             "Background self-improvement notices in chat: \"off\" (review still runs) | \"on\" (generic \"Memory updated\") | \"verbose\" (content preview). Per-platform via display.platforms.<platform>.memory_notifications.",
		"display.background_process_notifications": "Gateway notices when a terminal(background=true) process finishes: \"concise\" (one line; failures append an output tail) | \"all\" (running updates + final raw output) | \"result\" (final raw only) | \"error\" (raw only on non-zero exit) | \"off\".",
		"display.streaming":                        "CLI token streaming; gateway streaming follows the top-level streaming section.",
		"display.timestamps":                       "Message timestamps (CLI labels, TUI rows, desktop transcript).",
		"display.timestamp_format":                 "strftime format, e.g. \"%b-%d %H:%M\".",
		"display.final_response_markdown":          "Final response markdown: render | strip | raw.",
		"display.persistent_output":                "Preserve recent classic-CLI output across Ctrl+L, /redraw and resize clears; disable if an emulator misbehaves with replayed scrollback.",
		"display.persistent_output_max_lines":      "Lines of classic-CLI output preserved across clears.",
		"display.cli_rebuild_scrollback_on_redraw": "Also clear terminal scrollback on classic-CLI full redraw and resize recovery; enable when a terminal or tmux stack stamps stale prompt chrome into scrollback.",
		"display.persist_prompts":                  "Print a one-line summary of resolved modal prompts (approval, clarify) to scrollback.",
		"display.inline_diffs":                     "Inline diff previews for write_file, patch and skill_manage.",
		"display.file_mutation_verifier":           "Append a one-line advisory to the final response when a write_file or patch failed this turn and was never superseded by a successful write to the same path (catches \"half the parallel patches failed, model claims success\").",
		"display.credits_notices":                  "Portal credits status-bar notices (usage bands, grant-spent, depleted and restored). false mutes them; balance data and /usage keep working.",
		"display.turn_completion_explainer":        "Append a one-line explanation when a turn ends with no usable reply (empty after retries, truncated stream, pending tool result, iteration or budget limit) instead of the bare \"(empty)\" sentinel.",
		"display.show_cost":                        "$ cost in the status bar.",
		"display.battery":                          "Battery read-out first in the status bar; no-op without a battery.",
		"display.focus_view":                       "Focus view (/focus): display-only. Pins tool_progress to \"off\", reports the per-turn hidden-line count, pins a \"focus\" status segment. Never affects what the model sees.",
		"display.focus_saved_tool_progress":        "The tool_progress mode /focus off restores.",
		"display.skin":                             "Active skin name.",
		"display.language":                         "UI language for static messages (approval prompts, some gateway slash replies); not agent responses, logs or tool outputs. en, zh, ja, de, es, fr, tr, uk; unknown -> en.",
		"display.tui_status_indicator":             "TUI busy indicator: kaomoji | emoji | unicode (braille) | ascii. `/indicator <style>`.",
		"display.cli_refresh_interval":             "Seconds between idle redraws in the classic CLI; keeps wall-clock status-bar read-outs ticking and the bottom chrome from going stale. 0 disables it if it fights terminal auto-scroll in non-fullscreen mode.",
		"display.vim_mode":                         "Vi/vim keybindings in the CLI input composer (config-only, no slash command). Off by default, preserving the standard emacs bindings.",
		"display.user_message_preview":             "CLI: submitted user-message lines echoed to scrollback.",
		"display.interim_assistant_messages":       "Gateway: natural mid-turn assistant status messages. Desktop: keep mid-turn narration between tool calls instead of collapsing to the final message.",
		"display.suppress_warning_notifications":   "Engine warning and failure notifications stay visible unless an operator opts in. Does not suppress task results, manual commands, or existing logs.",
		"display.show_commentary":                  "Codex Responses commentary channel: true delivers completed commentary as mid-turn interim updates; false routes it to reasoning (visible only with show_reasoning).",
		"display.tool_progress_command":            "Enable the /verbose command in the messaging gateway.",
		"display.tool_preview_length":              "Max chars for tool call previews (0 = no limit).",
		"display.friendly_tool_labels":             "Human-phrased status labels for built-in tools (\"Reading <file>\") in the CLI spinner and gateway and desktop tool progress; custom, plugin and MCP tools use the raw preview.",
		"display.turn_summary":                     "CLI-only post-turn line: \"12.4s, edited 2 files +18 -3, read 4 files, ran 3 commands\". Never in quiet or non-interactive or gateway surfaces (own footer).",
		"display.spinner_token_flow":               "CLI-only: cumulative turn output tokens on the live spinner.",
		"display.tool_progress_grouping":           "Gateway tool-progress grouping where edits are supported: \"accumulate\" edits one bubble | \"separate\" one message per tool (noisier). Needs tool_progress enabled. Per-platform: display.platforms.<platform>.tool_progress_grouping.",
		"display.status_phrases":                   "Custom long-running status phrases. `path` or `paths` = ICEBATH_HOME-relative YAML files or dirs (or the conventional status_phrases.yaml and status_phrases/*.yaml). Keys: status, generic. mode: \"append\" (default) | \"replace\". Per-platform: display.platforms.<platform>.status_phrases.",
		"display.reasoning_style":                  "Reasoning summary rendering: \"code\" (fenced block) | \"blockquote\" (\"> \") | \"subtext\" (\"-# \" Discord small grey text; Discord's default). Per-platform via display.platforms.<platform>.reasoning_style.",
		"display.ephemeral_system_ttl":             "Auto-delete ephemeral system notices (\"New session started!\") after N seconds where deletion is supported (Telegram; others ignore). Agent responses are never touched. 0 = disabled.",
		"display.platforms":                        "Per-platform display and streaming overrides; unset keys fall through to the global. Telegram has smooth native draft streaming (on); Discord and Slack only have edit-based streaming, which flickers (off). Gap-fillers only: explicit user values win, and the global streaming.enabled master switch still gates everything.",
		"display.platforms.wecom":                  "WeCom native streaming (msgtype \"stream\" via aibot_respond_msg).",
		"display.runtime_footer":                   "Gateway runtime footer on the FINAL message, e.g. `model, 68%, ~/projects/icebath`. Per-platform: display.platforms.<platform>.runtime_footer.",
		"display.runtime_footer.fields":            "Order shown; drop any to hide. Opt-in extras: latency, served_model (alias -> the deployment a routing proxy reported, or icebath's fallback route).",
		"display.status_bar":                       "CLI and TUI status bar fields. Non-empty = only listed fields show (built-in order kept, config controls visibility not ordering); empty = default set. Available: model, context_detail, context_pct, cache_hit, latency, tps, compressions, bg_tasks, bg_processes, bg_subagents, goal, git_branch (current branch, opt-in only), duration, prompt_elapsed, idle_since, focus, yolo, stash, battery, title, total_tokens (session total, opt-in only). Narrow terminals still drop context_detail, prompt_elapsed and idle_since.",
		"display.copy_shortcut":                    "\"auto\" (platform default) | ctrl_c | ctrl_shift_c | disabled.",
		"display.pet":                              "Petdex animated mascot (github.com/crafter-station/petdex): cosmetic sprite across CLI, TUI and desktop, managed with `icebath pets`. No effect on prompt caching.",
		"display.pet.slug":                         "Active pet slug in the icebath home pets/ directory; empty -> first installed.",
		"display.pet.render_mode":                  "auto (detect kitty, iTerm2, sixel, else unicode half-blocks) | kitty | iterm | sixel | unicode | off.",
		"display.pet.scale":                        "Size scalar relative to native 192x208 frames, shared by the desktop canvas and CLI and TUI column width. Half-block fallback clamps to a legibility floor.",
		"display.pet.unicode_cols":                 "Hard override for terminal column width; 0 = derive from scale.",
		"display.tool_progress_overrides":          "Deprecated; use display.platforms. A user-set value is still honored at runtime and folded into platforms by migration.",
		"display.generic_status_phrases":           "Legacy spelling of status_phrases, read before it globally and per platform.",
		"display.message_reactions":                "Desktop Appearance switch mirrored to the gateway: offers the message reaction tool (default off).",
		"display.in_app_tips":                      "Desktop Appearance switch mirrored to the gateway: offers the in-app tip tool (default on).",
		"display.in_app_tours":                     "Desktop Appearance switch mirrored to the gateway: offers the guided tour tool (default on).",
	}
}

// TUI status bar placements read case-insensitively, false reading as off, on as top and any other value top
func StatusbarModes() []string {
	return []string{"off", "top", "bottom"}
}

// Canonical TUI mouse tracking modes
func MouseTrackingModes() []string {
	return []string{"off", "all", "buttons", "wheel"}
}

// Words the TUI reads for mouse tracking once lower-cased, true meaning all, false off, unknown words all
func MouseTrackingAliases() map[string]string {
	return map[string]string{
		"0": "off", "1": "all", "all": "all", "any": "all", "button": "buttons", "buttons": "buttons",
		"click": "buttons", "false": "off", "full": "all", "no": "off", "off": "off", "on": "all",
		"scroll": "wheel", "true": "all", "wheel": "wheel", "yes": "all",
	}
}
