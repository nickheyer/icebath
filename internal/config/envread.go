package config

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// Reports a variable holding 1, true, yes or on in any case, def standing in while it is unset
func EnvEnabled(name, def string) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		value = def
	}
	switch strings.ToLower(trimEnvSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Number text in ASCII digits with the single underscores between digits dropped, ok false for a misplaced underscore
func plainNumberText(raw string) (string, bool) {
	var b strings.Builder
	digit := false
	runes := []rune(raw)
	for i, r := range runes {
		switch {
		case r == '_':
			if !digit || i+1 >= len(runes) || !unicode.IsDigit(runes[i+1]) {
				return "", false
			}
			digit = false
		case unicode.IsDigit(r):
			zero := r
			for unicode.IsDigit(zero - 1) {
				zero--
			}
			b.WriteByte(byte('0' + (r-zero)%10))
			digit = true
		default:
			b.WriteRune(r)
			digit = false
		}
	}
	return b.String(), true
}

// Whole number a variable holds, def when it is unset, blank or no whole number, clamped to the int64 range
func EnvInt(name string, def int64) int64 {
	text, ok := plainNumberText(trimEnvSpace(os.Getenv(name)))
	if !ok || text == "" {
		return def
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return def
	}
	return v
}

// Number a variable holds, def when it is unset, blank or no decimal number
func EnvFloat(name string, def float64) float64 {
	text, ok := plainNumberText(trimEnvSpace(os.Getenv(name)))
	if !ok || text == "" || strings.ContainsAny(text, "xXpP") {
		return def
	}
	if strings.EqualFold(strings.TrimLeft(text, "+-"), "nan") && len(text) <= 4 {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return def
	}
	return v
}
