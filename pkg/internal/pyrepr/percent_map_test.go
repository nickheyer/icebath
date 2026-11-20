package pyrepr

import "testing"

func TestPercentMapRendersLoggingFormats(t *testing.T) {
	values := map[string]any{"asctime": "2026-01-02 03:04:05,006", "levelname": "INFO", "name": "agent.x", "message": "hi", "session_tag": " [s1]", "msecs": 6.0, "levelno": 20}
	lookup := func(k string) (any, bool) { v, ok := values[k]; return v, ok }
	cases := []struct{ format, want string }{
		{"%(asctime)s %(levelname)s%(session_tag)s %(name)s: %(message)s", "2026-01-02 03:04:05,006 INFO [s1] agent.x: hi"},
		{"%(levelname)-8s|%(msecs)03d|%(levelno)5d|%(name).5s|%(message)r|100%%", "INFO    |006|   20|agent|'hi'|100%"},
		{"%(msecs)f %(levelno)#x", "6.000000 0x14"},
	}
	for _, c := range cases {
		got, err := PercentMap(c.format, lookup)
		if err != nil || got != c.want {
			t.Fatalf("PercentMap(%q) = %q, %v want %q", c.format, got, err, c.want)
		}
	}
	errs := map[string]string{
		"%(missing)s": "KeyError: 'missing'",
		"%(name)d":    "TypeError: %d format: a real number is required, not str",
		"%(name)y":    "ValueError: unsupported format character 'y' (0x79) at index 7",
		"%s":          "TypeError: format requires a mapping",
		"%(name":      "ValueError: incomplete format key",
		"trailing %":  "ValueError: incomplete format",
	}
	for format, want := range errs {
		if _, err := PercentMap(format, lookup); err == nil || err.Error() != want {
			t.Fatalf("PercentMap(%q) error = %v want %s", format, err, want)
		}
	}
}
