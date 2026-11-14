package selfupdate

import (
	"runtime"
	"strings"
	"testing"
)

func TestStatusEnvironmentStatesGoRuntimeVersion(t *testing.T) {
	h := stHome(t)
	out := stShow(t, h, stStatusArgs(t, "--full"))
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "Project:") {
			continue
		}
		fields := strings.Fields(lines[i+1])
		if len(fields) != 2 || fields[0] != "Go:" || fields[1] != strings.TrimPrefix(strings.Fields(runtime.Version())[0], "go") {
			t.Fatalf("line after Project is %q:\n%s", lines[i+1], out)
		}
		if !strings.HasPrefix(strings.TrimSpace(lines[i+2]), ".env file:") {
			t.Fatalf("line after Go is %q:\n%s", lines[i+2], out)
		}
		if strings.Contains(out, "Python:") {
			t.Fatalf("status of the Go binary states Python:\n%s", out)
		}
		return
	}
	t.Fatalf("no Project line:\n%s", out)
}
