package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// Plist that is not well formed XML, a raw && in ProgramArguments the way an operator chains commands
const malformedPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.bad</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/icebath</string>
    <string>&&</string>
  </array>
</dict>
</plist>
`

const goodPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>ai.icebath.dashboard.test</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/icebath</string>
    <string>dashboard</string>
    <string>--port</string>
    <string>9119</string>
  </array>
</dict>
</plist>
`

// Pins the macOS branch and records the labels probed against launchd, each answered with the given state
func pinLaunchdProbe(t *testing.T, loaded bool, pid int, hasPID bool) *[]string {
	t.Helper()
	pinServeGrammar(t)
	swapSeam(t, &dashboardGOOS, "darwin")
	probed := &[]string{}
	swapSeam(t, &dashboardLaunchdPrintPID, func(_ context.Context, _, label string) (bool, int, bool, error) {
		*probed = append(*probed, label)
		return loaded, pid, hasPID, nil
	})
	return probed
}

func TestMalformedPlistIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "com.example.bad.plist"), []byte(malformedPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	probed := pinLaunchdProbe(t, false, 0, false)
	if jobs := loadedLaunchdBackendJobs(context.Background(), []launchdPlistDir{{"agent", dir}}); len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if len(*probed) != 0 {
		t.Fatalf("probed = %v", *probed)
	}
}

func TestMalformedSiblingDoesNotHideTheGoodJob(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{"com.example.bad.plist": malformedPlist, "ai.icebath.dashboard.test.plist": goodPlist} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	probed := pinLaunchdProbe(t, true, 4321, true)
	jobs := loadedLaunchdBackendJobs(context.Background(), []launchdPlistDir{{"agent", dir}})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	job := jobs[0]
	if job.Domain != "gui/"+strconv.Itoa(os.Getuid()) || job.Label != "ai.icebath.dashboard.test" || job.LivePID == nil || *job.LivePID != 4321 ||
		!slices.Equal(job.Argv, []string{"/usr/local/bin/icebath", "dashboard", "--port", "9119"}) {
		t.Fatalf("job = %+v", job)
	}
	if !slices.Equal(*probed, []string{"ai.icebath.dashboard.test"}) {
		t.Fatalf("probed = %v", *probed)
	}
}
