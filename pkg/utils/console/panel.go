package consoleutil

import "strings"

// Box drawn around markup or a renderable, a title set in its top border and a subtitle in its bottom one, as wide as it prints unless fitted
type Panel struct {
	// Markup inside the box when Content is nil
	Body string
	// Markup set in the top border
	Title string
	// Markup set in the bottom border
	Subtitle string
	// Style definition of the border
	BorderStyle string
	// Box sized to its content instead of the whole width
	Fit bool
	// Renderable drawn inside the box in place of Body
	Content Renderable
	// Border drawn: rounded corners when empty, horizontals for rules above and below only
	Box string
	// Side the title and subtitle sit on: centered when empty, left or right
	TitleAlign string
	// Style definition laid under the border, the padding and what the box holds
	Style string
	// Lines of padding above and below and cells of padding on each side inside the border, none and one when nil
	Padding *[2]int
	// Most cells the box takes, the print width when zero
	Width int
}

func (p Panel) padding() (vertical, horizontal int) {
	if p.Padding == nil {
		return 0, 1
	}
	return max(0, p.Padding[0]), max(0, p.Padding[1])
}

// Lines of the body markup, unwrapped
func (p Panel) bodyText(o layout) segments {
	t := markupText(p.Body)
	if !o.links {
		t = inlineLinks(t)
	}
	return t
}

// Styled text of a border title, newlines turned to spaces
func borderTitle(markup string, o layout) segments {
	t := markupText(" " + strings.ReplaceAll(markup, "\n", " ") + " ")
	if !o.links {
		t = inlineLinks(t)
	}
	return t
}

// Outer width of a fitted box within width cells: what it holds plus padding, widened for the title
func (p Panel) fitWidth(o layout, width, padX int) int {
	child := width - 2
	if p.Content != nil {
		child = min(p.Content.measure(layout{width: width - 2, links: o.links})+2*padX, width-2)
	} else {
		longest := 0
		for _, l := range p.bodyText(o).lines() {
			longest = max(longest, l.width())
		}
		child = min(min(longest, width-2)+2*padX, width-2)
	}
	if p.Title != "" {
		child = min(width-2, max(child, borderTitle(p.Title, o).width()+2))
	}
	return child + 2
}

func (p Panel) measure(o layout) int {
	width := o.width
	if p.Width > 0 {
		width = min(width, p.Width)
	}
	if p.Fit {
		_, padX := p.padding()
		return p.fitWidth(o, width, padX)
	}
	return width
}

func (p Panel) render(o layout) []segments {
	width := p.measure(o)
	base := styleOf(p.Style)
	border := base.add(styleOf(p.BorderStyle))
	padY, padX := p.padding()
	inner := max(1, width-2-2*padX)
	topLeft, topRight, bottomLeft, bottomRight, side := "╭", "╮", "╰", "╯", "│"
	if p.Box == "horizontals" {
		topLeft, topRight, bottomLeft, bottomRight, side = " ", " ", " ", " ", " "
	}
	edge := func(left, right, title string) segments {
		if title == "" || width <= 4 {
			return segments{{left + strings.Repeat("─", max(0, width-2)) + right, border}}
		}
		t := borderTitle(title, o).over(border).crop(width - 4)
		excess := width - 4 - t.width()
		l := excess / 2
		switch p.TitleAlign {
		case "left":
			l = 0
		case "right":
			l = excess
		}
		return segments{{left + "─" + strings.Repeat("─", l), border}}.join(t).with(strings.Repeat("─", excess-l)+"─"+right, border)
	}
	var body []segments
	if p.Content != nil {
		for _, l := range p.Content.render(layout{width: inner, links: o.links}) {
			body = append(body, l.crop(inner))
		}
	} else {
		for _, l := range wrap(p.bodyText(o), inner, wrapOptions{}) {
			body = append(body, l.truncate(inner, "ellipsis", false, Style{}))
		}
	}
	gap := spaces(padX)
	blank := segments{{side, border}, {spaces(width - 2), base}, {side, border}}
	lines := make([]segments, 0, len(body)+2*padY+2)
	lines = append(lines, edge(topLeft, topRight, p.Title))
	for range padY {
		lines = append(lines, blank)
	}
	for _, l := range body {
		row := segments{{side, border}}.with(gap, base).join(l.over(base))
		row = row.with(spaces(inner-l.width()), base).with(gap, base).with(side, border)
		lines = append(lines, row)
	}
	for range padY {
		lines = append(lines, blank)
	}
	return append(lines, edge(bottomLeft, bottomRight, p.Subtitle))
}

// Lines of the box at width cells, styled for the terminal when color is set
func (p Panel) Render(color bool, width int) string { return renderString(p, color, width) }
