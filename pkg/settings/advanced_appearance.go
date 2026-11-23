package settings

// Glyph layout or boxed transcript in the classic command line
var DisplayCliLayout = Register(&Enum{Meta: Meta{Key: "display.cli_layout", Label: "Classic CLI layout", Short: "Glyph layout or boxed transcript in the classic command line.", Group: GroupAppearance, Order: 1000, Advanced: true}, Default: "modern", Values: []Choice{
	{Value: "modern", Label: "Modern", Meaning: "Glyph-led transcript."},
	{Value: "classic", Label: "Classic", Meaning: "Boxed transcript."},
}})

// How the pet is drawn in your terminal
var DisplayPetRenderMode = Register(&Enum{Meta: Meta{Key: "display.pet.render_mode", Label: "Pet drawing mode", Short: "How the pet is drawn in your terminal.", Group: GroupAppearance, Order: 1010, Advanced: true}, Default: "auto", Values: []Choice{
	{Value: "auto", Label: "Auto", Meaning: "Best mode this terminal supports."},
	{Value: "kitty", Label: "Kitty", Meaning: "Kitty graphics protocol."},
	{Value: "iterm", Label: "iTerm", Meaning: "iTerm inline images."},
	{Value: "sixel", Label: "Sixel", Meaning: "Sixel graphics."},
	{Value: "unicode", Label: "Text blocks", Meaning: "Block characters, works anywhere."},
	{Value: "off", Label: "Off", Meaning: "Do not draw the pet."},
}})

// How large the pet is drawn
var DisplayPetScale = Register(&Float{Meta: Meta{Key: "display.pet.scale", Label: "Pet size", Short: "How large the pet is drawn.", Help: "Values are kept between 0.1 and 3.", Group: GroupAppearance, Order: 1020, Advanced: true}, Default: 0.33, Min: at(0.1), Max: at(3.0)})

// Which installed pet is shown
var DisplayPetSlug = Register(&String{Meta: Meta{Key: "display.pet.slug", Label: "Which pet", Short: "Which installed pet is shown.", Help: "Empty shows none.", Group: GroupAppearance, Order: 1030, Advanced: true}, Default: ""})

// Width of the pet in text blocks, in terminal columns
var DisplayPetUnicodeCols = Register(&Int{Meta: Meta{Key: "display.pet.unicode_cols", Label: "Text pet width", Short: "Width of the pet in text blocks, in terminal columns.", Help: "Zero works the width out from the pet size.", Group: GroupAppearance, Order: 1040, Advanced: true}, Default: 0, Unit: "columns"})

// Your own wording for the short progress messages in chat apps
var DisplayStatusPhrases = Register(&Map{Meta: Meta{Key: "display.status_phrases", Label: "Status phrases", Short: "Your own wording for the short progress messages in chat apps.", Group: GroupAppearance, Order: 1050, Advanced: true}})

// How message times are written in the classic command line
var DisplayTimestampFormat = Register(&String{Meta: Meta{Key: "display.timestamp_format", Label: "Time format", Short: "How message times are written in the classic command line.", Help: "Uses strftime codes. Times only show when timestamps are turned on.", Group: GroupAppearance, Order: 1060, Advanced: true}, Default: "%H:%M"})

// Show a hint about the agents view in the terminal UI
var DisplayTuiAgentsNudge = Register(&Bool{Meta: Meta{Key: "display.tui_agents_nudge", Label: "Agents view hint", Short: "Show a hint about the agents view in the terminal UI.", Group: GroupAppearance, Order: 1070, Advanced: true}, Default: true})

// Let your terminal handle mouse selection and scrolling
var DisplayTuiNative = Register(&Bool{Meta: Meta{Key: "display.tui_native", Label: "Native terminal mode", Short: "Let your terminal handle mouse selection and scrolling.", Help: "Turns off mouse tracking in the terminal UI. The native flag on the command line does the same.", Group: GroupAppearance, Order: 1080, Advanced: true}, Default: false})

// Pasted text longer than this is folded into a short placeholder
var PasteCollapseCharThreshold = Register(&Int{Meta: Meta{Key: "paste_collapse_char_threshold", Label: "Paste collapse size", Short: "Pasted text longer than this is folded into a short placeholder.", Group: GroupAppearance, Order: 1090, Advanced: true}, Default: 2000, Unit: "characters"})

// Pasted text with more lines than this is folded into a placeholder
var PasteCollapseThreshold = Register(&Int{Meta: Meta{Key: "paste_collapse_threshold", Label: "Paste collapse lines", Short: "Pasted text with more lines than this is folded into a placeholder.", Group: GroupAppearance, Order: 1100, Advanced: true}, Default: 5, Unit: "lines"})
