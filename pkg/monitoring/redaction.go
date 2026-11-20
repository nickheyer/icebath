package monitoring

import (
	"regexp"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
)

// RedactionUnavailable is what egress carries when the secret redactor itself fails
const RedactionUnavailable = memory.RedactionUnavailable

var (
	exportEmailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	exportPhoneRE = mustCompilePy(`(?<!\w)(?:\+?\d{1,3}[\s.\-]?)?(?:\(\d{2,4}\)[\s.\-]?)?\d{3}[\s.\-]?\d{3,4}(?:[\s.\-]?\d{2,4})?(?!\w)`)
	exportUUIDRE  = mustCompilePy(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
)

func mustCompilePy(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Secret scrub of the shared egress redactor, a seam for the fail closed test
var egressSecretRedactor = memory.RedactForEgress

// Secret scrub that yields the unavailable marker instead of the raw text when the redactor panics
func redactForEgress(text string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = RedactionUnavailable
		}
	}()
	return egressSecretRedactor(text)
}

// Scrubs a value for egress, secrets first, then email, uuid and phone shapes, false for None
func RedactForExport(text any) (string, bool) {
	if text == nil {
		return "", false
	}
	out := redactForEgress(pyrepr.Str(text))
	out = exportEmailRE.ReplaceAllLiteralString(out, "[email]")
	out, _ = exportUUIDRE.Replace(out, "[id]", -1, -1)
	out, _ = exportPhoneRE.Replace(out, "[phone]", -1, -1)
	return out, true
}

// Redacted and length bounded text of a value with the default limit and markers
func RedactBounded(raw any) string {
	return RedactBoundedWith(raw, 500, "[redacted]", RedactionUnavailable)
}

// Redacted text of a falsy defaulted value cut to limit characters, empty and failure markers substituted
func RedactBoundedWith(raw any, limit int, empty, unavailable string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = unavailable
		}
	}()
	if !pyrepr.Truthy(raw) {
		raw = ""
	}
	text, _ := RedactForExport(pyrepr.Str(raw))
	if text == "" {
		text = empty
	}
	rs := pystr.Runes(text)
	end := limit
	if end < 0 {
		end = max(len(rs)+end, 0)
	}
	if end < len(rs) {
		text = pystr.FromRunes(rs[:end])
	}
	return text
}
