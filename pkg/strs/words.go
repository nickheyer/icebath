package strs

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Number in digits grouped in thousands, with as many decimals as it has and no more than three
func Number(n float64) string {
	text := strconv.FormatFloat(math.Round(n*1000)/1000, 'f', -1, 64)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	whole, fraction, _ := strings.Cut(text, ".")
	var grouped []string
	for len(whole) > 3 {
		grouped = append(grouped, whole[len(whole)-3:])
		whole = whole[:len(whole)-3]
	}
	grouped = append(grouped, whole)
	slices.Reverse(grouped)
	text = sign + strings.Join(grouped, ",")
	if fraction != "" {
		text += "." + fraction
	}
	return text
}

// Number and the word for what it counts, given in the plural and said in the singular for exactly one: 1 call, 50,000 characters
func Counted(n float64, what string) string {
	if n == 1 {
		what = Singular(what)
	}
	return Number(n) + " " + what
}

// Count of things named in the singular: 1 session, 2 sessions
func Count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return Number(float64(n)) + " " + Plural(noun)
}

// Word for one of what the plural names: a short unit such as MB or ms is left as it is
func Singular(word string) string {
	switch {
	case len(word) <= 3:
		return word
	case strings.HasSuffix(word, "ies"):
		return strings.TrimSuffix(word, "ies") + "y"
	case strings.HasSuffix(word, "sses"), strings.HasSuffix(word, "ches"), strings.HasSuffix(word, "shes"), strings.HasSuffix(word, "xes"):
		return strings.TrimSuffix(word, "es")
	case strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss"):
		return strings.TrimSuffix(word, "s")
	}
	return word
}

// Word for several of what the singular names
func Plural(word string) string {
	switch {
	case strings.HasSuffix(word, "s"), strings.HasSuffix(word, "x"), strings.HasSuffix(word, "ch"), strings.HasSuffix(word, "sh"):
		return word + "es"
	case len(word) > 1 && strings.HasSuffix(word, "y") && !strings.ContainsRune("aeiou", rune(word[len(word)-2])):
		return strings.TrimSuffix(word, "y") + "ies"
	}
	return word + "s"
}

// Count of bytes in the largest unit that fits, the units of 1024 for a count they divide and of 1000 for any other: 512 bytes, 1.5 KB, 5 MB
func Size(n float64) string {
	base := 1000.0
	if n >= 1024 && math.Mod(n, 1024) == 0 {
		base = 1024
	}
	units := []string{"bytes", "KB", "MB", "GB", "TB"}
	at := 0
	for math.Abs(n) >= base && at < len(units)-1 {
		n /= base
		at++
	}
	if at == 0 {
		return Counted(n, units[0])
	}
	return Number(math.Round(n*10)/10) + " " + units[at]
}

// Length of time in words, in the largest unit that divides it: 1 hour, 90 minutes, 1.5 seconds
func Span(length time.Duration) string {
	for _, unit := range []struct {
		length time.Duration
		name   string
	}{{24 * time.Hour, "days"}, {time.Hour, "hours"}, {time.Minute, "minutes"}, {time.Second, "seconds"}} {
		if length != 0 && length%unit.length == 0 {
			return Counted(float64(length/unit.length), unit.name)
		}
	}
	if length == 0 {
		return "0 seconds"
	}
	if length > -time.Second && length < time.Second {
		return Counted(float64(length)/float64(time.Millisecond), "milliseconds")
	}
	return Counted(length.Seconds(), "seconds")
}
