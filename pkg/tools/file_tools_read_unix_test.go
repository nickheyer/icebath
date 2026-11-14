//go:build !windows

package tools

import (
	"strings"
	"testing"
)

func TestFileToolsReadSpecialFileKindCharDevice(t *testing.T) {
	if k := specialFileKind("/dev/null"); !strings.Contains(k, "character device") {
		t.Fatalf("kind %q", k)
	}
}
