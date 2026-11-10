package pyrich

import "strings"

// Text is a rich Text renderable of ANSI styled or plain lines wrapped at the render width
type Text struct{ Plain string }

// Lines wrapped at width with styling kept only when color is on
func (t *Text) RenderLines(color bool, _ string, width int) []string {
	var out []string
	for _, line := range strings.Split(t.Plain, "\n") {
		if !color {
			line = ansiRE.ReplaceAllString(line, "")
		}
		out = append(out, WrapStyled(line, width, true)...)
	}
	return out
}

// Width the text asks for, the whole render width
func (t *Text) MeasureMax(width int) int { return width }

// Prints any renderable fitted to the console width
func (c *Console) PrintRenderable(r Renderable) {
	c.write(r.RenderLines(c.Color, c.system(), c.Width))
}
