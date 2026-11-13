package app

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// Runs fn with stdout and stderr redirected to pipes and returns what each received
func captureStdio(t *testing.T, fn func()) (string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	fn()
	os.Stdout, os.Stderr = stdout, stderr
	outW.Close()
	errW.Close()
	return <-outC, <-errC
}

func TestJourneySlashRefusesHelpOptionsOfItsHelplessParser(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := &CLI{ctx: context.Background()}
	for _, tc := range []struct{ line, token string }{
		{"/journey -h", "-h"},
		{"/journey --help", "--help"},
		{"/journey --help=yes", "--help=yes"},
		{"/journey --hel", "--hel"},
		{"/journey --fps 3 -hx", "-hx"},
	} {
		out, errText := captureStdio(t, func() { c.handleJourneyCommand(tc.line) })
		if !strings.Contains(errText, "/journey: error: unrecognized arguments: "+tc.token+"\n") {
			t.Fatalf("%s stderr %q stdout %q", tc.line, errText, out)
		}
		if !strings.HasPrefix(errText, "usage: /journey [--reveal 0..1]") || strings.Contains(errText, "[-h]") {
			t.Fatalf("%s usage %q", tc.line, errText)
		}
		if out != "" {
			t.Fatalf("%s stdout %q", tc.line, out)
		}
	}
}

func TestJourneySlashSubcommandKeepsItsHelp(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := &CLI{ctx: context.Background()}
	out, errText := captureStdio(t, func() { c.handleJourneyCommand("/journey list -h") })
	if errText != "" || !strings.Contains(out, "usage: /journey list [-h]") || !strings.Contains(out, "show this help message and exit") {
		t.Fatalf("stdout %q stderr %q", out, errText)
	}
}
