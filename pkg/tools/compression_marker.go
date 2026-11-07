package tools

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marker sentence with the omitted and total counts rendered with thousands separators
const compressionMarkerDisclaimer = " This is NOT part of the original tool call and must never be reproduced in new " +
	"output \u2014 always write full, untruncated content.\u27eb"

const markerCountPattern = `\d[\d,]*`

// Either marker prefix, the legacy spelling kept so older session artifacts still match
var compressionMarkerPrefixPattern = `(?:` + regexp.QuoteMeta(CompressionMarkerPrefix) + `|` + regexp.QuoteMeta(legacyCompressionMarkerPrefix) + `)`

// Full rendered first sentence of a minted marker
var compressionMarkerRe = regexp.MustCompile(compressionMarkerPrefixPattern + ` ` + markerCountPattern + ` of ` + markerCountPattern +
	regexp.QuoteMeta(" chars omitted here by ") + `(?:icebath's|Hermes's)` + regexp.QuoteMeta(" context compressor."))

// Whether the text carries a minted compression marker first sentence
func HasCompressionMarker(text string) bool { return compressionMarkerRe.MatchString(text) }

// Python format with the comma thousands separator
func pyCommaInt(n int) string {
	digits := strconv.Itoa(n)
	neg := strings.HasPrefix(digits, "-")
	if neg {
		digits = digits[1:]
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Full compression marker with the disclaimer sentence
func compressionMarker(omitted, total int) string {
	return CompressionMarkerPrefix + " " + pyCommaInt(omitted) + " of " + pyCommaInt(total) + " chars omitted here by icebath's context compressor." + compressionMarkerDisclaimer
}

// Elision marker, the first sentence of the compression marker with per instance counts
func elisionMarker(omitted, total int) string {
	return CompressionMarkerPrefix + " " + pyCommaInt(omitted) + " of " + pyCommaInt(total) + " chars omitted here by icebath's context compressor.⟫"
}

// Intra record truncation marker naming the omitted and total character counts
func ElisionMarker(omitted, total int) string { return elisionMarker(omitted, total) }

// Widest elision marker for any text under a terabyte
var elisionMarkerMaxLen = utf8.RuneCountInString(elisionMarker(1e12-1, 1e12-1))

// Longest elision marker any count can render, in code points
func ElisionMarkerMaxLen() int { return elisionMarkerMaxLen }

// Observer of every elide limit, installed by tests only
var elideObserver func(limit int)

// Caps text at limit characters with the elision marker, the marker alone when limit cannot hold it
func elide(text string, limit int) string {
	if elideObserver != nil {
		elideObserver(limit)
	}
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text
	}
	headLen := limit - utf8.RuneCountInString(elisionMarker(total, total))
	if headLen <= 0 {
		return elisionMarker(total, total)
	}
	kept := pyRStrip(runePrefix(text, headLen))
	return kept + elisionMarker(total-utf8.RuneCountInString(kept), total)
}

// Caps text at limit characters with the elision marker
func Elide(text string, limit int) string { return elide(text, limit) }

// Keeps head characters from the start and tail from the end with the marker between them
func elideMiddle(text string, head, tail int) string {
	runes := []rune(text)
	if len(runes) <= head+tail {
		return text
	}
	marker := elisionMarker(len(runes)-head-tail, len(runes))
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

// Text keeping head and tail code points around an elision marker when longer than both
func ElideMiddle(text string, head, tail int) string { return elideMiddle(text, head, tail) }
