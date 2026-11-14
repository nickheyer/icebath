//go:build windows

package prompt

import "strings"

func envKey(k string) string { return strings.ToUpper(k) }
