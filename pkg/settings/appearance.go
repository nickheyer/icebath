package settings

// Light, dark or match the terminal
var Theme = Register(&Enum{Meta: Meta{Key: "display.tui_theme", Label: "Theme",
	Short: "Light, dark, or match the terminal.",
	Group: GroupAppearance, Order: 10, Slash: "theme"},
	Default: "auto",
	Values: []Choice{
		{"auto", "Match terminal", "Follow the terminal's background."},
		{"light", "Light", ""},
		{"dark", "Dark", ""},
	}})

// Named palette and symbols drawn on top of the theme
var Skin = Register(&String{Meta: Meta{Key: "display.skin", Label: "Skin",
	Short: "The colour palette and symbols drawn on top of the theme.",
	Group: GroupAppearance, Order: 20, Slash: "skin"},
	Default: "default"})

// Language of the interface
var Language = Register(&String{Meta: Meta{Key: "display.language", Label: "Language",
	Short: "Language of menus, notices and help.",
	Group: GroupAppearance, Order: 30, Slash: "language"},
	Default: "en"})

// Tighter spacing
var Compact = Register(&Bool{Meta: Meta{Key: "display.tui_compact", Label: "Compact",
	Short: "Less spacing so more of the transcript fits.",
	Group: GroupAppearance, Order: 40, Slash: "density"},
	Default: false})

// Time beside each message
var Timestamps = Register(&Bool{Meta: Meta{Key: "display.timestamps", Label: "Timestamps",
	Short: "Show the time beside each message.",
	Group: GroupAppearance, Order: 50, Slash: "timestamps"},
	Default: false})

// Mouse clicks and scrolling
var Mouse = Register(&Enum{Meta: Meta{Key: "display.mouse_tracking", Label: "Mouse",
	Short: "Which mouse actions the terminal UI takes over.",
	Help:  "Off leaves the mouse to the terminal, so you can select text the usual way.",
	Group: GroupAppearance, Order: 60, Slash: "mouse"},
	Default: "all", On: "all", Off: "off",
	Values: []Choice{
		{"all", "All", "Clicks, drag, wheel and hover."},
		{"buttons", "Drag", "Clicks, drag and the wheel."},
		{"wheel", "Wheel", "Clicks and the wheel."},
		{"off", "Off", "None; the terminal keeps the mouse."},
	}})

// Busy indicator style
var BusyIndicator = Register(&Enum{Meta: Meta{Key: "display.tui_status_indicator", Label: "Busy indicator",
	Short: "Symbols of the spinner shown while the agent works.",
	Group: GroupAppearance, Order: 70, Slash: "indicator"},
	Default: "unicode",
	Values: []Choice{
		{"unicode", "Unicode", "Clean line symbols."},
		{"emoji", "Emoji", ""},
		{"ascii", "ASCII", "Plain characters for any terminal."},
	}})

// Animated companion in the corner
var Pet = Register(&Bool{Meta: Meta{Key: "display.pet.enabled", Label: "Pet",
	Short: "Show a small animated companion that reacts to the agent's work.",
	Group: GroupAppearance, Order: 80},
	Default: false})
