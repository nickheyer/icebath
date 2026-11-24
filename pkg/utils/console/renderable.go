package consoleutil

// Space and settings a renderable lays its lines out in
type layout struct {
	// cells a line may take
	width int
	// lines left whole for a terminal that wraps them itself
	noWrap bool
	// whether output can carry terminal hyperlinks, link addresses written out after their text when it cannot
	links bool
}

// Something a console prints as lines laid out at its width: Text, Markdown, a Table or a Panel
type Renderable interface {
	render(o layout) []segments
	measure(o layout) int
}

// ANSI styled or plain text, its colors, attributes and hyperlinks kept, printed as it is and wrapped at the width
type Text string

func (t Text) render(o layout) []segments {
	decoded := decodeANSI(string(t))
	if !o.links {
		decoded = inlineLinks(decoded)
	}
	return wrap(decoded, o.width, wrapOptions{noWrap: o.noWrap})
}

func (t Text) measure(o layout) int { return o.width }

// Settings of one renderable print beyond the console's own
type PrintOptions struct {
	// Style definition laid under everything printed
	Style string
	// Lines left unwrapped and uncut so the terminal wraps them
	SoftWrap bool
}

// Lines of a renderable at width joined by newlines, styled for the terminal when color is set
func renderString(r Renderable, color bool, width int) string {
	p := colorFor(color)
	links := color
	return renderLines(r.render(layout{width: width, links: links}), p, links)
}
