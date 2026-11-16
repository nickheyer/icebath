package app

// State of the prompt after a vi key sequence, compared against prompt_toolkit 3.0.53
type viOutcome struct {
	text   string
	cursor int
	mode   ptInputMode
	clip   ptClipboardData
	regs   map[string]ptClipboardData
	multi  []int
}

// Key press of a case token, angle brackets naming a special key
func viPress(tok string) ptKeyPress {
	named := map[string]ptKeyPress{"<escape>": {Key: "escape", Data: "\x1b"}, "<c-v>": {Key: "c-v", Data: "\x16"},
		"<c-k>": {Key: "c-k", Data: "\x0b"}, "<c-o>": {Key: "c-o", Data: "\x0f"}, "<backspace>": {Key: "c-h", Data: "\x7f"},
		"<enter>": {Key: "c-m", Data: "\r"}}
	if k, ok := named[tok]; ok {
		return k
	}
	return ptKeyPress{Key: tok, Data: tok}
}

// Styles of the input cells of a row, the prompt left out
func viRowStyles(content ptInputContent, row int) []string {
	var out []string
	for _, cell := range content.lines[row] {
		out = append(out, cell.style)
	}
	return out
}
