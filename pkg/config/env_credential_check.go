package config

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var nonASCIIWarnOutput io.Writer = os.Stderr

// CheckNonASCIICredential strips non ASCII characters from a credential and warns on stderr which ones went
func CheckNonASCIICredential(key, value string) string {
	ascii := true
	for _, r := range value {
		if r > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return value
	}
	var badChars []string
	var sanitized strings.Builder
	for i, ch := range []rune(value) {
		if ch > 127 {
			badChars = append(badChars, fmt.Sprintf("  position %d: %s (U+%04X)", i, pyrepr.StrRepr(string(ch)), ch))
			continue
		}
		sanitized.WriteRune(ch)
	}
	shown := badChars
	if len(shown) > 5 {
		shown = shown[:5]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  Warning: %s contains non-ASCII characters that will break API requests.\n"+
		"  This usually happens when copy-pasting from a PDF, rich-text editor,\n"+
		"  or web page that substitutes lookalike Unicode glyphs for ASCII letters.\n\n", key)
	for i, line := range shown {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + line)
	}
	if len(badChars) > 5 {
		b.WriteString("\n  ... and more")
	}
	b.WriteString("\n\n  The non-ASCII characters have been stripped automatically.\n" +
		"  If authentication fails, re-copy the key from the provider's dashboard.\n")
	fmt.Fprintln(nonASCIIWarnOutput, b.String())
	return sanitized.String()
}
