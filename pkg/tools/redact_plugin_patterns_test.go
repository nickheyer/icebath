package tools

import (
	"testing"
)

func TestRedactPrefixSearchExportMatchesThePrefixScan(t *testing.T) {
	for _, text := range []string{"https://x.test/?k=sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", "plain text"} {
		if RedactPrefixSearch(text) != prefixSearch(text) {
			t.Fatalf("prefix search diverges for %q", text)
		}
	}
}
