package usage

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func fixed(x float64, places int) string { return strconv.FormatFloat(x, 'f', places, 64) }

func parseFloatText(s string) (float64, error) {
	text := strings.TrimSpace(s)
	lower := strings.ToLower(strings.TrimLeft(text, "+-"))
	if strings.HasPrefix(lower, "0x") {
		return 0, errMalformed
	}
	if strings.Contains(text, "_") {
		for i, r := range text {
			if r != '_' {
				continue
			}
			before, _ := utf8.DecodeLastRuneInString(text[:i])
			after, _ := utf8.DecodeRuneInString(text[i+1:])
			if !unicode.IsDigit(before) || !unicode.IsDigit(after) {
				return 0, errMalformed
			}
		}
		text = strings.ReplaceAll(text, "_", "")
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, errMalformed
	}
	return f, nil
}

func roundTo(x float64, places int) float64 {
	v, err := strconv.ParseFloat(fixed(x, places), 64)
	if err != nil {
		return x
	}
	return v
}

func FormatUSD(v float64) string {
	text := fixed(v, 2)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	whole, frac, _ := strings.Cut(text, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return "$" + sign + b.String() + "." + frac
}

func titleCase(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if prevCased {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(unicode.ToTitle(r))
		}
		prevCased = unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
	}
	return b.String()
}

func titleCaseSlug(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return ""
	}
	return titleCase(strings.NewReplacer("_", " ", "-", " ").Replace(cleaned))
}

var isoLayouts = func() []string {
	layouts := []string{"2006-01-02"}
	for _, sep := range []string{"T", " "} {
		for _, clock := range []string{"15", "15:04", "15:04:05"} {
			for _, zone := range []string{"", "Z07:00", "-0700", "-07"} {
				layouts = append(layouts, "2006-01-02"+sep+clock+zone)
			}
		}
	}
	return layouts
}()

func parseISO(text string) (time.Time, bool) {
	value := strings.TrimSpace(text)
	if strings.HasSuffix(value, "Z") {
		value = value[:len(value)-1] + "+00:00"
	}
	for _, layout := range isoLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func padRight(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func plural(count int) string {
	if count != 1 {
		return "s"
	}
	return ""
}

func ratFixed(r *big.Rat, places int) string {
	neg := r.Sign() < 0
	abs := new(big.Rat).Abs(r)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	num := new(big.Int).Mul(abs.Num(), scale)
	quo, rem := new(big.Int).QuoRem(num, abs.Denom(), new(big.Int))
	switch new(big.Int).Lsh(rem, 1).Cmp(abs.Denom()) {
	case 1:
		quo.Add(quo, big.NewInt(1))
	case 0:
		if quo.Bit(0) == 1 {
			quo.Add(quo, big.NewInt(1))
		}
	}
	digits := quo.String()
	if len(digits) <= places {
		digits = strings.Repeat("0", places-len(digits)+1) + digits
	}
	text := digits
	if places > 0 {
		text = digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	}
	if neg {
		text = "-" + text
	}
	return text
}

func FormatCostLabel(amount *big.Rat) string {
	if amount == nil || amount.Sign() == 0 {
		return "$0.00"
	}
	if amount.Cmp(big.NewRat(1, 100)) < 0 {
		label := "~$" + ratFixed(amount, 4)
		if label == "~$0.0000" {
			return "~$<0.0001"
		}
		return label
	}
	return "~$" + ratFixed(amount, 2)
}

func FormatDurationCompact(d time.Duration) string {
	seconds := d.Seconds()
	if seconds < 60 {
		return fixed(seconds, 0) + "s"
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fixed(minutes, 0) + "m"
	}
	hours := minutes / 60
	if hours < 24 {
		remaining := int(math.Mod(minutes, 60))
		if remaining != 0 {
			return strconv.Itoa(int(hours)) + "h " + strconv.Itoa(remaining) + "m"
		}
		return strconv.Itoa(int(hours)) + "h"
	}
	return fixed(hours/24, 1) + "d"
}

func FormatTokenCountCompact(value int64) string {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	if abs < 1000 {
		return strconv.FormatInt(value, 10)
	}
	sign := ""
	if value < 0 {
		sign = "-"
	}
	threshold, suffix := int64(1000), "K"
	switch {
	case abs >= 1_000_000_000:
		threshold, suffix = 1_000_000_000, "B"
	case abs >= 1_000_000:
		threshold, suffix = 1_000_000, "M"
	}
	scaled := float64(abs) / float64(threshold)
	var text string
	switch {
	case scaled < 10:
		text = fixed(scaled, 2)
	case scaled < 100:
		text = fixed(scaled, 1)
	default:
		text = fixed(scaled, 0)
	}
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return sign + text + suffix
}
