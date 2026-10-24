package theme

import "charm.land/lipgloss/v2"

// Every mark the interface draws, swapped as one set so the whole surface keeps one look
type Glyphs struct {
	Brand     string
	Session   string
	Trace     string
	Agent     string
	Approval  string
	Metrics   string
	Files     string
	Findings  string
	Jobs      string
	Mission   string
	Sessions  string
	Settings  string
	Config    string
	Palette   string
	Card      string
	Profiles  string
	Models    string
	Rules     string
	Hooks     string
	MCP       string
	Tools     string
	Log       string
	Keys      string
	Read      string
	Write     string
	Edit      string
	Glob      string
	Grep      string
	Bash      string
	Output    string
	Kill      string
	Fetch     string
	Spawn     string
	Record    string
	Plan      string
	Server    string
	Tool      string
	Speaker   string
	Steer     string
	Thinking  string
	Compact   string
	Hook      string
	Note      string
	Error     string
	Flag      string
	Child     string
	Attach    string
	Idle      string
	Cursor    string
	Marked    string
	Live      string
	Hollow    string
	Check     string
	Skip      string
	Todo      string
	Done      string
	Active    string
	Bullet    string
	Mask      string
	Fork      string
	Changed   string
	Sep       string
	Ellipsis  string
	Close     string
	Menu      string
	Prompt    string
	Send      string
	Grip      string
	Back      string
	CapLeft   string
	CapRight  string
	Info      string
	Success   string
	Warn      string
	Fail      string
	Search    string
	None      string
	Rule      string
	Dashed    string
	Bar       string
	Fill      string
	Slide     string
	Thumb     string
	Caret     string
	Up        string
	Down      string
	Track     string
	Heavy     string
	Tick      string
	HeavyTick string
	Hit       string
	Full      string
	Empty     string
	// Eight levels of a bar graph from lowest to highest
	Ramp []rune
	// Frames of the working spinner
	Spinner []string
	// Characters text boils through while it materializes
	Churn []rune
	// One cell symbols of bare navigation chords such as pgup, text labels stand in for chords missing here
	Chords map[string]string
}

// Frost, signal and hazard marks in single cells, the icebath set
func IcebathGlyphs() Glyphs {
	return Glyphs{
		Brand: "❆", Session: "❆", Trace: "⟁", Agent: "⟁", Approval: "⍰", Metrics: "⌬", Files: "◫", Findings: "⌖", Jobs: "⎔", Mission: "⟐", Sessions: "⧈", Settings: "◩", Config: "⎈", Palette: "❯", Card: "▰",
		Profiles: "⍟", Models: "⟡", Rules: "§", Hooks: "↯", MCP: "⇆", Tools: "⌗", Log: "⍝", Keys: "⌥",
		Read: "◇", Write: "◆", Edit: "◈", Glob: "✶", Grep: "⚲", Bash: "⌁", Output: "⇩", Kill: "⊗", Fetch: "⇶", Spawn: "⟁", Record: "⌖", Plan: "≔", Server: "⇆", Tool: "⊳",
		Speaker: "┃", Steer: "⤷", Thinking: "∴", Compact: "⇊", Hook: "↯", Note: "◦", Error: "✘", Flag: "⍰", Child: "⟁", Attach: "⎘", Idle: "⊙", Cursor: "►", Marked: "▮",
		Live: "◉", Hollow: "○", Check: "✓", Skip: "⊠", Todo: "□", Done: "■", Active: "◈", Bullet: "▹", Mask: "∙", Fork: "⋔", Changed: "◫",
		Sep: "∷", Ellipsis: "⋯", Close: "✕", Menu: "≣", Prompt: "❯", Send: "➜", Grip: "⊹", Back: "❮", CapLeft: "▐", CapRight: "▌", Info: "◈", Success: "✓", Warn: "◬", Fail: "✘", Search: "⚲", None: "∅",
		Rule: "─", Dashed: "┈", Bar: "▎", Fill: "━", Slide: "┈", Thumb: "◆", Caret: "▎", Up: "⌃", Down: "⌄", Track: "┊", Heavy: "┃", Tick: "╂", HeavyTick: "╋", Hit: "◈", Full: "▰", Empty: "▱",
		Ramp:    []rune("▁▂▃▄▅▆▇█"),
		Spinner: []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"},
		Churn:   []rune("░▒▓█▙▚▛▜▝▞▟╳╱╲01"),
		Chords:  map[string]string{"pgup": "⇞", "pgdown": "⇟", "home": "⇱", "end": "⇲", "up": "⇡", "down": "⇣"},
	}
}

// The same marks in printable ASCII for terminals without box drawing or symbol fonts
func ASCIIGlyphs() Glyphs {
	return Glyphs{
		Brand: "*", Session: "~", Trace: "&", Agent: "&", Approval: "?", Metrics: "%", Files: "#", Findings: "!", Jobs: "@", Mission: "o", Sessions: "=", Settings: "%", Config: "*", Palette: ">", Card: "*",
		Profiles: "@", Models: "o", Rules: "S", Hooks: "!", MCP: "=", Tools: "+", Log: "#", Keys: "k",
		Read: "r", Write: "w", Edit: "e", Glob: "*", Grep: "/", Bash: "$", Output: "v", Kill: "x", Fetch: "@", Spawn: "&", Record: "!", Plan: ":", Server: "=", Tool: ">",
		Speaker: "|", Steer: "~", Thinking: ":", Compact: "v", Hook: "!", Note: "-", Error: "x", Flag: "?", Child: "&", Attach: "+", Idle: "o", Cursor: ">", Marked: "#",
		Live: "*", Hollow: "o", Check: "+", Skip: "x", Todo: "_", Done: "#", Active: ">", Bullet: "-", Mask: "*", Fork: "Y", Changed: "#",
		Sep: "-", Ellipsis: "...", Close: "x", Menu: "=", Prompt: ">", Send: ">", Grip: "+", Back: "<", CapLeft: " ", CapRight: " ", Info: "*", Success: "+", Warn: "!", Fail: "x", Search: "/", None: "-",
		Rule: "-", Dashed: "-", Bar: "|", Fill: "=", Slide: "-", Thumb: "O", Caret: "|", Up: "^", Down: "v", Track: "|", Heavy: "#", Tick: "+", HeavyTick: "+", Hit: "*", Full: "#", Empty: "-",
		Ramp:    []rune(".:-=+*#%"),
		Spinner: []string{"|", "/", "-", "\\"},
		Churn:   []rune("#%&*+-=01"),
		Chords:  map[string]string{},
	}
}

// Frame with heavy top and bottom rails on light sides, the icebath border
func IcebathBorder() lipgloss.Border {
	return lipgloss.Border{Top: "━", Bottom: "━", Left: "│", Right: "│", TopLeft: "┍", TopRight: "┑", BottomLeft: "┕", BottomRight: "┙", MiddleLeft: "┝", MiddleRight: "┥", Middle: "┿", MiddleTop: "┯", MiddleBottom: "┷"}
}
