package app

import (
	"strings"
	"testing"
)

func TestClipboardHelperWithoutDestinationExitsNonZero(t *testing.T) {
	var out, errOut strings.Builder
	if code := RunTUIClipboardImage(t.Context(), nil, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatalf("%d %q %q", code, out.String(), errOut.String())
	}
}
