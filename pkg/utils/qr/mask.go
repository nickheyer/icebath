package qrutil

// Penalty of a size by size module layout by the four ISO/IEC 18004 mask rules: long runs, two by two blocks, finder look-alikes and dark balance
func penalty(modules []bool, size int) int {
	score := 0
	column := make([]bool, size)
	for i := range size {
		score += linePenalty(modules[i*size : (i+1)*size])
		for k := range size {
			column[k] = modules[k*size+i]
		}
		score += linePenalty(column)
	}
	dark := 0
	for y := range size {
		row := modules[y*size : (y+1)*size]
		for _, m := range row {
			if m {
				dark++
			}
		}
		if y+1 == size {
			break
		}
		next := modules[(y+1)*size : (y+2)*size]
		for x := range size - 1 {
			if m := row[x]; m == row[x+1] && m == next[x] && m == next[x+1] {
				score += 3
			}
		}
	}
	total := size * size
	balance := 2*dark - total
	if balance < 0 {
		balance = -balance
	}
	return score + balance*10/total*10
}

// Run and finder look-alike penalties of one row or column, the quiet zone past its ends counting as light
func linePenalty(line []bool) int {
	score, run := 0, 0
	for i, m := range line {
		if i > 0 && m == line[i-1] {
			run++
		} else {
			run = 1
		}
		if run == 5 {
			score += 3
		} else if run > 5 {
			score++
		}
	}
	for s := 0; s+7 <= len(line); s++ {
		if line[s] && !line[s+1] && line[s+2] && line[s+3] && line[s+4] && !line[s+5] && line[s+6] &&
			(lightSpan(line, s-4, s) || lightSpan(line, s+7, s+11)) {
			score += 40
		}
	}
	return score
}

// Whether every module of line from lo up to hi is light, positions outside the line counting as light
func lightSpan(line []bool, lo, hi int) bool {
	for _, m := range line[max(lo, 0):min(hi, len(line))] {
		if m {
			return false
		}
	}
	return true
}
