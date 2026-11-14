package config

import (
	"bytes"
	"strings"
	"testing"
)

func captureNonASCIIWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := nonASCIIWarnOutput
	nonASCIIWarnOutput = buf
	t.Cleanup(func() { nonASCIIWarnOutput = prev })
	return buf
}

func TestCheckNonAsciiCredentialAsciiKeyUnchanged(t *testing.T) {
	key := "sk-proj-" + strings.Repeat("a", 100)
	if got := CheckNonASCIICredential("TEST_API_KEY", key); got != key {
		t.Fatalf("ascii key changed: %q", got)
	}
}

func TestCheckNonAsciiCredentialStripsUnicodeVLookalike(t *testing.T) {
	stderr := captureNonASCIIWarnings(t)
	got := CheckNonASCIICredential("OPENROUTER_API_KEY", "sk-proj-abcʋdef")
	if got != "sk-proj-abcdef" || strings.Contains(got, "ʋ") {
		t.Fatalf("sanitized %q", got)
	}
	if !strings.Contains(stderr.String(), "non-ASCII") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestCheckNonAsciiCredentialStripsMultipleNonAscii(t *testing.T) {
	stderr := captureNonASCIIWarnings(t)
	if got := CheckNonASCIICredential("OPENAI_API_KEY", "sk-proj-aʋbécd"); got != "sk-proj-abcd" {
		t.Fatalf("sanitized %q", got)
	}
	if !strings.Contains(stderr.String(), "U+028B") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
