package plugins

import (
	"strings"
	"testing"
)

// Each non blank stderr line of the child is logged with its profile, invalid bytes replaced one per subpart
func TestHostStderrPumpTagsEachLineWithTheProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := captureLogs(t)
	pumpHostStderr(strings.NewReader("first line  \n\n\x0b\x0c\nbad \xff\xfe end\x0c\nlast"), "work")
	out := logs.String()
	for _, want := range []string{`plugin host [work]: first line"`, "plugin host [work]: bad � end\"", `plugin host [work]: last"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.Count(out, "plugin host [work]") != 3 {
		t.Fatalf("blank and whitespace only lines are skipped: %s", out)
	}
}
