package cards

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Wrapped lines under a table that scroll on their own beside a scrollbar capped with the detail keys
type detailPane struct {
	scroll int
	owner  int
	top    int
	rows   int
	total  int
}

// Moves the pane by n lines, kept inside its lines
func (d *detailPane) by(n int) {
	d.scroll = min(max(d.scroll+n, 0), max(d.total-d.rows, 0))
}

// Takes no rows, for a view that shows no detail
func (d *detailPane) hide() { d.rows, d.total = 0, 0 }

// Reports whether the pane holds more lines than it shows
func (d *detailPane) overflows() bool { return d.total > d.rows }

// Scrollbar of the pane capped with the keys that page it
func (d *detailPane) gutter(env *Env) gutter {
	return gutter{h: d.rows, total: d.total, offset: d.scroll, visible: d.rows, up: env.Keys.First("detail.scroll.up"), down: env.Keys.First("detail.scroll.down")}
}

// Lines of the pane for row owner placed top rows down and rows tall, wrapped to fit beside the scrollbar
func (d *detailPane) view(lines []string, owner, top, rows, w int, env *Env) string {
	if owner != d.owner {
		d.scroll, d.owner = 0, owner
	}
	bw := w
	if w >= minGutterWidth {
		bw = w - 2
	}
	var body []string
	for _, line := range lines {
		for _, part := range strings.Split(render.Wrap(line, max(bw-2, 1)), "\n") {
			body = append(body, "  "+part)
		}
	}
	d.top, d.rows, d.total = top, max(rows, 0), len(body)
	d.by(0)
	shown := body[min(d.scroll, len(body)):]
	shown = shown[:min(len(shown), d.rows)]
	text := strings.Join(shown, "\n")
	if bw < w && d.rows > 0 {
		text = d.gutter(env).beside(text, w, env.Theme)
	}
	return text
}

// Carries out a detail scroll action, reporting whether it was one
func (d *detailPane) act(id string) bool {
	switch id {
	case "detail.scroll.down":
		d.by(max(d.rows-1, 1))
	case "detail.scroll.up":
		d.by(-max(d.rows-1, 1))
	default:
		return false
	}
	return true
}

// Pages or jumps for a click on the scrollbar, reporting whether the click landed in the pane
func (d *detailPane) click(x, y, w int, env *Env) bool {
	row := y - d.top
	if d.rows <= 0 || row < 0 || row >= d.rows {
		return false
	}
	if x == w-1 && w >= minGutterWidth {
		switch hit, line := d.gutter(env).hit(row); hit {
		case hitPageUp:
			d.by(-max(d.rows-1, 1))
		case hitPageDown:
			d.by(max(d.rows-1, 1))
		case hitJump:
			d.scroll = line
		}
	}
	return true
}

// Scrolls by n for a wheel at row y, reporting whether the wheel was over the pane
func (d *detailPane) wheel(y, n int) bool {
	if d.rows <= 0 || y < d.top || y >= d.top+d.rows {
		return false
	}
	d.by(n)
	return true
}
