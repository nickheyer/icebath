package provider

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Usage bar geometry, cells and the basis points of a full allowance
const (
	BarWidth        = 10
	BarFill         = "|"
	BarEmpty        = " "
	basisPointsFull = 10000
)

// Clock the reset durations read, swappable so tests pin the time
var usageBarNow = time.Now

// Rounds to the given decimal places the way Python round does on the exact binary value
func roundPlaces(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', places, 64), 64)
	return r
}
func clampFraction(v float64) float64 {
	if v < 0.0 {
		return 0.0
	}
	if 1.0 < v {
		return 1.0
	}
	return v
}
func filledCells(fraction float64, width int) int {
	value := clampFraction(fraction)
	filled := int(math.RoundToEven(value * float64(width)))
	if value > 0.0 && filled == 0 {
		filled = 1
	}
	if value < 1.0 && filled == width {
		filled = width - 1
	}
	return filled
}

// RenderBar draws a bracketed bar whose filled cells show the fraction, never empty above zero or full below one
func RenderBar(fraction float64, width int) string {
	filled := filledCells(fraction, width)
	return "[" + strings.Repeat(BarFill, filled) + strings.Repeat(BarEmpty, width-filled) + "]"
}

// FormatPercentUsed floors the used share to a whole percent, a sliver reading below one percent
func FormatPercentUsed(usedFraction float64) string {
	bp := int64(roundPlaces(clampFraction(usedFraction)*basisPointsFull, 6))
	if bp <= 0 {
		return "0%"
	}
	if bp < 100 {
		return "<1%"
	}
	return fmt.Sprintf("%d%%", bp/100)
}

// Time until the reset as days and hours, hours, or minutes, empty when past, under a minute or unknown
func compactDurationAt(resetsAtMs, nowMs *int64) string {
	if resetsAtMs == nil {
		return ""
	}
	now := usageBarNow().UnixMilli()
	if nowMs != nil {
		now = *nowMs
	}
	seconds := max(0, floorDivInt64(*resetsAtMs-now, 1000))
	if seconds < 60 {
		return ""
	}
	days, rem := seconds/86_400, seconds%86_400
	hours, rem := rem/3_600, rem%3_600
	minutes := rem / 60
	if days != 0 {
		if hours != 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
	if hours != 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", minutes)
}
func floorDivInt64(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// FormatResetsIn reads the reset as resets in a duration, or resets soon when past or unknown
func FormatResetsIn(resetsAtMs, nowMs *int64) string {
	if d := compactDurationAt(resetsAtMs, nowMs); d != "" {
		return "resets in " + d
	}
	return "resets soon"
}
