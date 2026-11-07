package tools

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestBrowserSafetyExportsMatchTheURLSafetyFloor(t *testing.T) {
	meta, public := netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("8.8.8.8")
	if !IsAlwaysBlockedIP(meta) || IsAlwaysBlockedIP(public) {
		t.Fatal("always blocked ip classification")
	}
	for _, raw := range []string{"http://169.254.169.254/latest/meta-data", "http://8.8.8.8/", "http:/\\\\evil"} {
		if IsAlwaysBlockedURL(nil, raw) != isAlwaysBlockedURL(nil, raw) {
			t.Fatalf("always blocked url diverges for %q", raw)
		}
	}
	if !IsAlwaysBlockedURL(nil, "http://169.254.169.254/latest/meta-data") {
		t.Fatal("metadata url passed the floor")
	}
	if got := NormalizeURLForRequest("http://example.com/a b"); got != normalizeURLForRequest("http://example.com/a b") || !strings.Contains(got, "%20") {
		t.Fatalf("normalized %q", got)
	}
	env := &Env{Home: t.TempDir(), Config: config.Tree{"security": map[string]any{"fake_ip_ranges": []any{"198.18.0.0/15"}}}}
	fake := netip.MustParseAddr("198.18.0.7")
	if !IsDeclaredFakeIP(env, fake) || IsDeclaredFakeIP(nil, fake) || IsDeclaredFakeIP(env, public) {
		t.Fatal("declared fake ip classification")
	}
}

func TestIsBlockedHostnameExportReadsTheMetadataBlocklist(t *testing.T) {
	if !IsBlockedHostname("metadata.google.internal") || IsBlockedHostname("example.com") {
		t.Fatal("blocked hostname classification")
	}
}
