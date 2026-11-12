package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Run of a usage line, styled green, yellow or red when it is the filled part of a gauge bar
type gaugeSegment struct {
	Style string
	Text  string
}

// Inner text of a gauge bar, pipes then spaces
var gaugeInnerRe = regexp.MustCompile(`^\|* *$`)

// Byte spans of the ten cell gauge bars of a line
func gaugeBars(text string) [][2]int {
	var out [][2]int
	for i := 0; i+11 < len(text); i++ {
		if text[i] != '[' || text[i+11] != ']' {
			continue
		}
		inner := text[i+1 : i+11]
		if strings.Trim(inner, "| ") != "" || !gaugeInnerRe.MatchString(inner) {
			continue
		}
		out = append(out, [2]int{i, i + 12})
		i += 11
	}
	return out
}

// Line holds a usage gauge bar
func hasUsageGauge(text string) bool { return len(gaugeBars(text)) > 0 }

// Color of a gauge bar by its used fill, red above eighty percent and yellow above half
func gaugeBarStyle(bar string) string {
	inner := bar[1 : len(bar)-1]
	used := 0.0
	if inner != "" {
		used = float64(strings.Count(inner, "|")) / float64(len(inner))
	}
	switch {
	case used > 0.8:
		return "red"
	case used > 0.5:
		return "yellow"
	}
	return "green"
}

// Usage line split so only the filled cells of each gauge bar carry a color
func splitUsageGaugeLine(text string) []gaugeSegment {
	var segs []gaugeSegment
	last := 0
	for _, b := range gaugeBars(text) {
		bar := text[b[0]:b[1]]
		inner := bar[1 : len(bar)-1]
		pipes := strings.Repeat("|", strings.Count(inner, "|"))
		empty := inner[len(pipes):]
		if b[0] > last {
			segs = append(segs, gaugeSegment{Text: text[last:b[0]]})
		}
		segs = append(segs, gaugeSegment{Text: "["})
		if pipes != "" {
			segs = append(segs, gaugeSegment{Style: gaugeBarStyle(bar), Text: pipes})
		}
		if empty != "" {
			segs = append(segs, gaugeSegment{Text: empty})
		}
		segs = append(segs, gaugeSegment{Text: "]"})
		last = b[1]
	}
	if last < len(text) {
		segs = append(segs, gaugeSegment{Text: text[last:]})
	}
	if len(segs) == 0 {
		return []gaugeSegment{{Text: text}}
	}
	return segs
}

var (
	canonicalLocalIDRe = regexp.MustCompile(`^local-([0-9a-f]{8})[0-9a-f]{56}$`)
	storedSessionIDRe  = regexp.MustCompile(`(?i)^\d{8}_\d{6}_[0-9a-z]+$`)
)

// Short label of a session id, the eight hex of a canonical local id or a stable hash of a stored id
func sessionLabel(sid string) string {
	if sid == "" {
		return sid
	}
	if m := canonicalLocalIDRe.FindStringSubmatch(sid); m != nil {
		return m[1]
	}
	if storedSessionIDRe.MatchString(sid) {
		sum := sha256.Sum256([]byte(sid))
		return hex.EncodeToString(sum[:])[:8]
	}
	return sid
}
