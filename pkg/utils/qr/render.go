package qrutil

import "strings"

// Rows of the symbol inside border light modules on every side, true for a dark module
func (c *Code) Matrix(border int) [][]bool {
	border = max(border, 0)
	width := c.size + 2*border
	cells := make([]bool, width*width)
	rows := make([][]bool, width)
	for y := range rows {
		rows[y] = cells[y*width : (y+1)*width : (y+1)*width]
	}
	for y := range c.size {
		copy(rows[y+border][border:], c.modules[y*c.size:(y+1)*c.size])
	}
	return rows
}

// Text of the symbol inside border light modules, two module rows per line of half block characters, every line ending in a newline and an odd last row leaving the lower half of its line unlit; invert draws the light modules as blocks for terminals with a dark background
func (c *Code) HalfBlocks(border int, invert bool) string {
	border = max(border, 0)
	width := c.size + 2*border
	glyphs := [4]string{" ", "▀", "▄", "█"}
	if invert {
		glyphs = [4]string{"█", "▄", "▀", " "}
	}
	dark := func(x, y int) bool {
		x, y = x-border, y-border
		return 0 <= x && x < c.size && 0 <= y && y < c.size && c.modules[y*c.size+x]
	}
	var b strings.Builder
	b.Grow((width + 1) / 2 * (3*width + 1))
	for y := 0; y < width; y += 2 {
		for x := range width {
			i := 0
			if dark(x, y) {
				i |= 1
			}
			if y+1 < width {
				if dark(x, y+1) {
					i |= 2
				}
			} else if invert {
				i |= 2
			}
			b.WriteString(glyphs[i])
		}
		b.WriteByte('\n')
	}
	return b.String()
}
