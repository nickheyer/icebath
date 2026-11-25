package config

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Text of a config value as a person reads it: text as it is, null, true or false, numbers and times as YAML writes them, containers as compact JSON
func ShowValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case float64:
		return representFloat(x)
	case float32:
		return representFloat(float64(x))
	case time.Time:
		return representTime(x)
	case *time.Time:
		if x != nil {
			return representTime(*x)
		}
	}
	return anyutil.String(v)
}

// Config value as a message quotes it: text in double quotes, anything else as ShowValue gives it
func quoteValue(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return ShowValue(v)
}

var shlexUnsafe = regexp.MustCompile(`[^\w@%+=:,./-]`)

// Word quoted for a POSIX shell: as it is when every character is safe unquoted, otherwise in single quotes with each inner single quote closed, escaped and reopened
func ShlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shlexUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
