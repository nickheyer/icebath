package selfupdate

import (
	"strings"
	"testing"
)

func TestRunDoctorWalksEveryCheckOfTheTable(t *testing.T) {
	d, buf := doctorRawHome(t, doctorConnectivityConfig, "GMI_API_KEY=***\n")
	t.Setenv("GMI_API_KEY", "gmi-test-key")
	calls := doctorFakeDefaultTransport(t, func(string) int { return 200 })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the doctor walk panicked: %v\n%s", r, buf.String())
		}
	}()
	out, code := doctorWalk(t, d, buf, map[string]any{"fix": false}, doctorChecks...)
	for _, entry := range doctorChecks {
		if entry.Title != "" && !strings.Contains(out, "◆ "+entry.Title) {
			t.Fatalf("section %q missing from %q", entry.Title, out)
		}
	}
	if !strings.Contains(out, "GMI Cloud") || !doctorContains(*calls, "https://api.gmi-serving.com/v1/models") {
		t.Fatalf("calls = %q out = %q", *calls, out)
	}
	if strings.Contains(out, "All checks passed") != (code == 0) {
		t.Fatalf("exit status %d disagrees with the summary: %q", code, out)
	}
}
