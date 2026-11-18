// Colors and chrome shared by every icebath surface
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette and border set that a deck paints with
type Theme struct {
	Name        string
	Bg          color.Color
	Surface     color.Color
	Text        color.Color
	TextDim     color.Color
	Steel       color.Color
	SteelBright color.Color
	Ghost       color.Color
	Accent      color.Color
	Ice         color.Color
	Pulse       color.Color
	Alive       color.Color
	Alarm       color.Color
	Ring        []color.Color
	Border      lipgloss.Border
	Glyphs      Glyphs
	Plain       bool
	// Sixteen ANSI colors programs in a terminal paint with, a nil entry leaving the host terminal color
	Terminal [16]color.Color
}

// Sixteen colors from their 24 bit values, dark then bright
func palette(v ...uint32) [16]color.Color {
	var out [16]color.Color
	for i := 0; i < len(v) && i < 16; i++ {
		out[i] = Hex(v[i])
	}
	return out
}

// Near black palette of icebath.app
func Blackwall() Theme {
	return Theme{
		Name:        "blackwall",
		Bg:          Hex(0x05070a),
		Surface:     Hex(0x0a0e13),
		Text:        Hex(0xdfe9f2),
		TextDim:     Hex(0x6c7f92),
		Steel:       Hex(0x18222d),
		SteelBright: Hex(0x2a3a4b),
		Ghost:       Hex(0x131b25),
		Accent:      Hex(0xe3122b),
		Ice:         Hex(0x9cc7e8),
		Pulse:       Hex(0xe3122b),
		Alive:       Hex(0x7fc7a5),
		Alarm:       Hex(0xff4757),
		Ring:        []color.Color{Hex(0x3b5169)},
		Border:      IcebathBorder(),
		Glyphs:      IcebathGlyphs(),
		Terminal:    palette(0x18222d, 0xe3122b, 0x7fc7a5, 0xe0b35a, 0x5b8fd6, 0xb07cd8, 0x9cc7e8, 0xdfe9f2, 0x3b5169, 0xff4757, 0x9fe3c1, 0xf0cc7a, 0x7fb0f0, 0xcf9cf0, 0xbfe0f8, 0xffffff),
	}
}

// Paper palette for bright rooms with the same red accent
func Light() Theme {
	return Theme{
		Name:        "light",
		Bg:          Hex(0xeef1f5),
		Surface:     Hex(0xfafbfd),
		Text:        Hex(0x141c26),
		TextDim:     Hex(0x647081),
		Steel:       Hex(0xd3dae3),
		SteelBright: Hex(0xb3bfcc),
		Ghost:       Hex(0xe3e8ee),
		Accent:      Hex(0xc8102e),
		Ice:         Hex(0x2f6f9f),
		Pulse:       Hex(0xc8102e),
		Alive:       Hex(0x2e8b63),
		Alarm:       Hex(0xd12f3f),
		Ring:        []color.Color{Hex(0x7d93a9)},
		Border:      IcebathBorder(),
		Glyphs:      IcebathGlyphs(),
		Terminal:    palette(0x141c26, 0xc8102e, 0x2e8b63, 0x9a6b00, 0x2f6f9f, 0x7a3fa0, 0x1d7f8f, 0x5c6b7d, 0x8a95a3, 0xd12f3f, 0x3aa077, 0xb8860b, 0x3f86bf, 0x9150c0, 0x2697ab, 0x2b3542),
	}
}

// Opaque color from a 24 bit RGB value
func Hex(v uint32) color.Color {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// Color t of the way from a to b, treating a nil side as the other side
func Mix(a, b color.Color, t float64) color.Color {
	if a == nil {
		a = b
	}
	if b == nil {
		b = a
	}
	if a == nil || t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return color.RGBA{R: lerp8(ar, br, t), G: lerp8(ag, bg, t), B: lerp8(ab, bb, t), A: 0xff}
}

// Darkens c toward black by amount
func Shade(c color.Color, amount float64) color.Color {
	return Mix(c, color.Black, amount)
}

// Produces n colors that run through the stops and return to the first so they tile seamlessly
func Ring(n int, stops ...color.Color) []color.Color {
	if len(stops) == 0 {
		return nil
	}
	if len(stops) == 1 || n < 2 {
		out := make([]color.Color, max(n, 1))
		for i := range out {
			out[i] = stops[0]
		}
		return out
	}
	if stops[0] != stops[len(stops)-1] {
		stops = append(append([]color.Color{}, stops...), stops[0])
	}
	return lipgloss.Blend1D(n, stops...)
}

func lerp8(a, b uint32, t float64) uint8 {
	fa, fb := float64(a>>8), float64(b>>8)
	return uint8(fa + (fb-fa)*t + 0.5)
}

// Every mark the interface draws, swapped as one set so the whole surface keeps one look
type Glyphs struct {
	Session   string
	Trace     string
	Agent     string
	Approval  string
	Metrics   string
	Findings  string
	Jobs      string
	Mission   string
	Sessions  string
	Settings  string
	Config    string
	Palette   string
	Terminal  string
	Card      string
	Profiles  string
	Models    string
	Rules     string
	Hooks     string
	MCP       string
	Tools     string
	Keys      string
	Ext       string
	Flow      string
	Read      string
	Write     string
	Edit      string
	Glob      string
	Grep      string
	Shell     string
	Output    string
	Kill      string
	Fetch     string
	Spawn     string
	Record    string
	Plan      string
	Server    string
	Tool      string
	Steer     string
	Thinking  string
	Compact   string
	Hook      string
	Note      string
	Error     string
	Flag      string
	Ask       string
	Child     string
	Attach    string
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
	Sep       string
	Ellipsis  string
	Close     string
	Prompt    string
	Notch     string
	Grip      string
	Back      string
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
	SwitchOn  string
	SwitchOff string
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
		Session: "❆", Trace: "⟁", Agent: "⟁", Approval: "!", Metrics: "⌬", Findings: "⌖", Jobs: "⎔", Mission: "⟐", Sessions: "⧈", Settings: "◩", Config: "⎈", Palette: "$", Terminal: "❯", Card: "▰",
		Profiles: "⍟", Models: "⟡", Rules: "§", Hooks: "↯", MCP: "⇆", Tools: "⌗", Keys: "⌥", Ext: "⧉", Flow: "⟳",
		Read: "◇", Write: "◆", Edit: "◈", Glob: "✶", Grep: "⚲", Shell: "⌁", Output: "⇩", Kill: "⊗", Fetch: "⇶", Spawn: "⟁", Record: "⌖", Plan: "≔", Server: "⇆", Tool: "⊳",
		Steer: "⤷", Thinking: "∴", Compact: "⇊", Hook: "↯", Note: "◦", Error: "✘", Flag: "!", Ask: "?", Child: "⟁", Attach: "⎘", Cursor: "▍", Marked: "▮",
		Live: "◉", Hollow: "○", Check: "✓", Skip: "⊠", Todo: "□", Done: "■", Active: "◈", Bullet: "▹", Mask: "∙", Fork: "⋔",
		Sep: "│", Ellipsis: "⋯", Close: "✕", Prompt: "$", Notch: "▍", Grip: "⊹", Back: "❮", Info: "◈", Success: "✓", Warn: "◬", Fail: "✘", Search: "⚲", None: "∅",
		Rule: "─", Dashed: "┈", Bar: "▎", Fill: "━", Slide: "┈", Thumb: "◆", SwitchOn: "━━●", SwitchOff: "●┈┈", Caret: "▎", Up: "⌃", Down: "⌄", Track: "┊", Heavy: "┃", Tick: "╂", HeavyTick: "╋", Hit: "◈", Full: "━", Empty: "─",
		Ramp:    []rune("▁▂▃▄▅▆▇█"),
		Spinner: []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"},
		Churn:   []rune("░▒▓█▙▚▛▜▝▞▟╳╱╲01"),
		Chords:  map[string]string{"pgup": "⇞", "pgdown": "⇟", "home": "⇱", "end": "⇲", "up": "⇡", "down": "⇣"},
	}
}

// The same marks in printable ASCII for terminals without box drawing or symbol fonts
func ASCIIGlyphs() Glyphs {
	return Glyphs{
		Session: "~", Trace: "&", Agent: "&", Approval: "!", Metrics: "%", Findings: "!", Jobs: "@", Mission: "o", Sessions: "=", Settings: "%", Config: "*", Palette: "$", Terminal: ">", Card: "*",
		Profiles: "@", Models: "o", Rules: "S", Hooks: "!", MCP: "=", Tools: "+", Keys: "k", Ext: "x", Flow: "@",
		Read: "r", Write: "w", Edit: "e", Glob: "*", Grep: "/", Shell: "$", Output: "v", Kill: "x", Fetch: "@", Spawn: "&", Record: "!", Plan: ":", Server: "=", Tool: ">",
		Steer: "~", Thinking: ":", Compact: "v", Hook: "!", Note: "-", Error: "x", Flag: "!", Ask: "?", Child: "&", Attach: "+", Cursor: ">", Marked: "#",
		Live: "*", Hollow: "o", Check: "+", Skip: "x", Todo: "_", Done: "#", Active: ">", Bullet: "-", Mask: "*", Fork: "Y",
		Sep: "|", Ellipsis: "...", Close: "x", Prompt: "$", Notch: "|", Grip: "+", Back: "<", Info: "*", Success: "+", Warn: "!", Fail: "x", Search: "/", None: "-",
		Rule: "-", Dashed: "-", Bar: "|", Fill: "=", Slide: "-", Thumb: "O", SwitchOn: "==O", SwitchOff: "O--", Caret: "|", Up: "^", Down: "v", Track: "|", Heavy: "#", Tick: "+", HeavyTick: "+", Hit: "*", Full: "#", Empty: "-",
		Ramp:    []rune(".:-=+*#%"),
		Spinner: []string{"|", "/", "-", "\\"},
		Churn:   []rune("#%&*+-=01"),
		Chords:  map[string]string{},
	}
}

// Single line frame with square corners, the icebath border
func IcebathBorder() lipgloss.Border {
	return lipgloss.NormalBorder()
}
