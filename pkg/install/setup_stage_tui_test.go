package install

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// Setup stage opens the terminal UI, the bare installed command, never the setup wizard
func TestSetupStageLaunchesTheTerminalUI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(InstallBinaryPath(dir), fixtureBinary(t, "v9.9.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := openTerminal
	t.Cleanup(func() { openTerminal = prev })
	openTerminal = func() (*os.File, bool) {
		f, err := os.Open(os.DevNull)
		return f, err == nil
	}
	r, err := NewRunner(Options{Dir: dir, Home: t.TempDir()}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.stdout, r.stderr = &out, &out
	if err := r.stageSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "icebath fixture v9.9.9 []") {
		t.Fatalf("setup stage ran %q", out.String())
	}
	var events []Event
	openTerminal = func() (*os.File, bool) { return nil, false }
	r, err = NewRunner(Options{Dir: dir, Home: t.TempDir()}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	r.stdout, r.stderr = &out, &out
	if err := r.stageSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ev := range events {
		text.WriteString(ev.Line)
	}
	if !strings.Contains(text.String(), "run 'icebath' and use /setup after install") {
		t.Fatalf("events %+v", events)
	}
}
