package provider

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// (icebath status surface)

// Status sink printing the status row, detail and section layout without colour
type statusTestSink struct {
	buf      bytes.Buffer
	env      map[string]string
	provider string
}

func (s *statusTestSink) Section(title string) { fmt.Fprintf(&s.buf, "\n◆ %s\n", title) }

func (s *statusTestSink) Row(name string, ok bool, text string, width int, sep string) {
	mark := "✗"
	if ok {
		mark = "✓"
	}
	fmt.Fprintf(&s.buf, "  %-*s%s%s %s\n", width, name, sep, mark, text)
}

func (s *statusTestSink) Detail(label string, value any) {
	text := "None"
	if value != nil {
		text = pyStr(value)
	}
	fmt.Fprintf(&s.buf, "    %-12s%s\n", label, text)
}

func (s *statusTestSink) Print(line string) { s.buf.WriteString(line + "\n") }

func (s *statusTestSink) EnvValue(name string) string {
	if v, ok := s.env[name]; ok {
		return v
	}
	return os.Getenv(name)
}

func (s *statusTestSink) FirstEnvValue(names ...string) string {
	for _, n := range names {
		if v := s.EnvValue(n); v != "" {
			return v
		}
	}
	return ""
}

func (s *statusTestSink) EffectiveProviderLabel() string { return s.provider }

func TestStatusAuthFormattersAndOAuthBlockGates(t *testing.T) {
	if statusFormatISOTimestamp(nil) != "(unknown)" || statusFormatISOTimestamp("  ") != "(unknown)" || statusFormatISOTimestamp(" not a date") != " not a date" {
		t.Fatal("timestamp fallbacks")
	}
	want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04:05 MST")
	if got := statusFormatISOTimestamp("2030-01-01T00:00:00Z"); got != want {
		t.Fatalf("timestamp %v want %s", got, want)
	}
	if got, err := statusQwenExpiry(int64(1893456000123)); err != nil || got != "2030-01-01T00:00:00.123000+00:00" {
		t.Fatalf("qwen expiry %q %v", got, err)
	}
	if got, err := statusQwenExpiry("1893456000000"); err != nil || got != "2030-01-01T00:00:00+00:00" {
		t.Fatalf("qwen expiry %q %v", got, err)
	}
	if _, err := statusQwenExpiry("soon"); err == nil {
		t.Fatal("a non integer expiry must raise")
	}
	sink := &statusTestSink{}
	block := statusOAuthBlocks[2]
	if err := statusOAuthBlockRender(sink, block, map[string]any{"logged_in": false, "region": "cn", "expires_at": "x", "error": "boom"}); err != nil {
		t.Fatal(err)
	}
	out := sink.buf.String()
	if !strings.Contains(out, "not logged in (run: icebath auth add minimax-oauth)") || strings.Contains(out, "Region:") ||
		!strings.Contains(out, "    Access exp: x") || !strings.Contains(out, "    Error:      boom") {
		t.Fatalf("logged out block:\n%s", out)
	}
	sink.buf.Reset()
	_ = statusOAuthBlockRender(sink, block, map[string]any{"logged_in": true, "region": "cn", "error": "boom"})
	if out := sink.buf.String(); !strings.Contains(out, "✓ logged in") || !strings.Contains(out, "Region:     cn") || strings.Contains(out, "Error:") {
		t.Fatalf("logged in block:\n%s", out)
	}
}
