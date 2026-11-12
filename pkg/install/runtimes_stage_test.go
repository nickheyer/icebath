package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Runtimes stage runs the installed command's pm install with the browser and computer use opt outs
func TestRuntimesStageRunsPMInstallWithTheOptOuts(t *testing.T) {
	for _, tc := range []struct {
		skipBrowser, skipComputerUse bool
		args                         string
	}{
		{false, false, "[pm install]"},
		{true, false, "[pm install --without agent-browser]"},
		{true, true, "[pm install --without agent-browser --without cua-driver]"},
	} {
		dir, homeDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(InstallBinaryPath(dir), fixtureBinary(t, "v9.9.9"), 0o755); err != nil {
			t.Fatal(err)
		}
		var events []string
		r, err := NewRunner(Options{Dir: dir, Home: homeDir, NonInteractive: true, SkipBrowser: tc.skipBrowser, SkipComputerUse: tc.skipComputerUse},
			func(ev Event) { events = append(events, fmt.Sprintf("%+v", ev)) })
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		r.stdout, r.stderr = &out, &out
		res, code := r.RunStage(context.Background(), "runtimes")
		if !res.OK || code != 0 {
			t.Fatalf("stage %+v %d\n%s", res, code, out.String())
		}
		if !strings.Contains(out.String(), "icebath fixture v9.9.9 "+tc.args) || !strings.Contains(strings.Join(events, "\n"), "dependencies installed") {
			t.Fatalf("stage output %q events %v", out.String(), events)
		}
	}
	dir := t.TempDir()
	r, err := NewRunner(Options{Dir: dir, Home: t.TempDir(), NonInteractive: true}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.stdout, r.stderr = &out, &out
	res, code := r.RunStage(context.Background(), "runtimes")
	if res.OK || code == 0 || res.Reason == nil || *res.Reason != "pm install failed" {
		t.Fatalf("missing command %+v %d", res, code)
	}
	var se *StageError
	if !errors.As(r.lastErr, &se) || se.Class != "deps_install_failed" {
		t.Fatalf("failure class %v", r.lastErr)
	}
}
