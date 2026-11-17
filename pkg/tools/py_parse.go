package tools

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var pyFloatTextRe = regexp.MustCompile(`\A[+-]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[+-]?[0-9]+)?|inf|infinity|nan)\z`)

// Integer text in a base the way int parses it, allowing whitespace, a sign, a base prefix and Unicode digits
func pyIntFromString(text string, base int) (*big.Int, error) {
	s := strings.TrimFunc(text, pyIsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	prefixes := map[int]string{2: "0b", 8: "0o", 16: "0x"}
	if p, ok := prefixes[base]; ok && len(s) > 2 && strings.EqualFold(s[:2], p) {
		s = s[2:]
	}
	if s == "" {
		return nil, fmt.Errorf("invalid literal for int() with base %d: %q", base, text)
	}
	n := new(big.Int)
	for _, r := range s {
		d := -1
		switch {
		case unicode.IsDigit(r):
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			d = int(r-start) % 10
		case r >= 'a' && r <= 'z':
			d = int(r-'a') + 10
		case r >= 'A' && r <= 'Z':
			d = int(r-'A') + 10
		}
		if d < 0 || d >= base {
			return nil, fmt.Errorf("invalid literal for int() with base %d: %q", base, text)
		}
		n.Mul(n, big.NewInt(int64(base)))
		n.Add(n, big.NewInt(int64(d)))
	}
	if neg {
		n.Neg(n)
	}
	return n, nil
}

// Float text the way float() parses it, surrounding whitespace and Unicode digits allowed and no hex form
func pyFloatFromString(text string) (float64, error) {
	var b strings.Builder
	for _, r := range strings.TrimFunc(text, pyIsSpace) {
		if unicode.IsDigit(r) && r > 0x7f {
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			r = '0' + (r-start)%10
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	if !pyFloatTextRe.MatchString(s) {
		return 0, fmt.Errorf("could not convert string to float: %q", text)
	}
	switch strings.TrimLeft(s, "+-") {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	return f, nil
}

// Anchor and parts of a path the way pathlib splits it, dot parts and repeated separators dropped
func pathlibParts(p string) (string, []string) {
	var anchor, rest string
	if isWindowsHost {
		p = strings.ReplaceAll(p, "/", `\`)
		drive, root, tail := ntSplitRoot(p)
		anchor, rest = drive+root, tail
	} else {
		switch {
		case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
			anchor = "//"
		case strings.HasPrefix(p, "/"):
			anchor = "/"
		}
		rest = strings.TrimLeft(p, "/")
	}
	sep := "/"
	if isWindowsHost {
		sep = `\`
	}
	var parts []string
	for _, part := range strings.Split(rest, sep) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return anchor, parts
}

// Final component of a path the way pathlib names it, empty for an anchor or the current directory
func pathlibName(p string) string {
	_, parts := pathlibParts(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
