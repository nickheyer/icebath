package pm

import (
	"bytes"
	"context"
	"testing"
)

// Install command steps recorded the way the closure tests spy on them, names accumulating over calls
type rabSpy struct{ names []string }

// Names a bare install requests after the recorded names are cleared
func (s *rabSpy) bareInstall(t *testing.T, without []string) []string {
	t.Helper()
	s.names = nil
	if code := rabInstall(t, InstallArgs{Without: without}); code != 0 {
		t.Fatalf("bare install exited %d", code)
	}
	return s.names
}

// Runs the install command and fails the test on an error escaping it
func rabInstall(t *testing.T, args InstallArgs) int {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdInstall(withSyncOutput(context.Background(), &out), args)
	if err != nil {
		t.Fatalf("install %v: %s", err, out.String())
	}
	return code
}
