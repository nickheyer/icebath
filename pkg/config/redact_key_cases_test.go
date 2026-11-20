package config

import (
	"strings"
	"testing"
)

func TestRedactKeyLongKeyShowsPrefixSuffix(t *testing.T) {
	result := RedactKey("sk-1234567890abcdef")
	if !strings.HasPrefix(result, "sk-1") || !strings.HasSuffix(result, "cdef") || !strings.Contains(result, "...") {
		t.Fatalf("redact_key = %q", result)
	}
}

func TestRedactKeyShortKeyFullyMasked(t *testing.T) {
	if got := RedactKey("short"); got != "***" {
		t.Fatalf("redact_key = %q", got)
	}
}
