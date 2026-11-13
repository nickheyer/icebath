package scenario

import (
	"os"
	"strings"
	"testing"
)

// Every file here beside doc.go carries the linux test suffix so the suite compiles whole or not at all
func TestScenarioFilesTargetLinux(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "doc.go" && !strings.HasSuffix(e.Name(), "_linux_test.go") {
			t.Errorf("%s must be named <name>_linux_test.go", e.Name())
		}
	}
}
