package pm

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Swaps a progress renderer seam for the length of one test
func cpSwap[T any](t *testing.T, slot *T, value T) {
	t.Helper()
	previous := *slot
	*slot = value
	t.Cleanup(func() { *slot = previous })
}

// Stream progress writes to, the live stream on a terminal or stdout off one
func cpCapture(t *testing.T, tty bool) (context.Context, *bytes.Buffer) {
	t.Helper()
	stream := &bytes.Buffer{}
	if tty {
		cpSwap(t, &progressStream, func(context.Context) io.Writer { return stream })
		return withSyncOutput(context.Background(), &bytes.Buffer{}), stream
	}
	cpSwap(t, &progressStream, func(context.Context) io.Writer { return nil })
	return withSyncOutput(context.Background(), stream), stream
}

const cpMiB = 1024 * 1024

func TestProgressRedrawsInPlaceOnATTY(t *testing.T) {
	ctx, stream := cpCapture(t, true)
	progress := newLiveProgress(ctx, "node")
	progress.report("download", 0, 100*cpMiB, "")
	progress.report("download", 100*cpMiB, 100*cpMiB, "")
	progress.finish()
	out := stream.String()
	if !strings.Contains(out, "\r\x1b[2K") || strings.Contains(out, "\n") || !strings.Contains(out, "100.0%") {
		t.Fatalf("out %q", out)
	}
}

func TestProgressOffATTYPrintsThrottledNewlineLines(t *testing.T) {
	ctx, stream := cpCapture(t, false)
	progress := newLiveProgress(ctx, "node")
	progress.report("download", 0, 100*cpMiB, "")
	progress.report("download", 1*cpMiB, 100*cpMiB, "")
	progress.report("download", 8*cpMiB, 100*cpMiB, "")
	progress.report("download", 100*cpMiB, 100*cpMiB, "")
	var lines []string
	for _, line := range strings.Split(stream.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "\r") {
			t.Fatalf("line %q redraws", line)
		}
	}
}

func TestUnpackPhaseIsAPhaseLineInBothModes(t *testing.T) {
	ctx, stream := cpCapture(t, false)
	newLiveProgress(ctx, "node").report("unpack", 0, 0, "1/2")
	if !strings.Contains(stream.String(), "unpacking 1/2") {
		t.Fatalf("out %q", stream.String())
	}
}

// Points stdout and stderr at distinct writers whose terminal answers the test controls
func cpTerminals(t *testing.T) (context.Context, io.Writer, io.Writer, map[io.Writer]bool) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	ttys := map[io.Writer]bool{}
	cpSwap(t, &enableVT, func(io.Writer) bool { return true })
	cpSwap(t, &streamIsTerminal, func(w io.Writer) bool { return ttys[w] })
	cpSwap(t, &stderrStream, func() io.Writer { return stderr })
	return withSyncOutput(context.Background(), stdout), stdout, stderr, ttys
}

func TestProgressStreamPrefersStdoutThenStderr(t *testing.T) {
	ctx, stdout, stderr, ttys := cpTerminals(t)
	if got := progressStream(ctx); got != nil {
		t.Fatalf("no terminal gave %v", got)
	}
	ttys[stderr] = true
	if got := progressStream(ctx); got != stderr {
		t.Fatalf("stderr terminal gave %v", got)
	}
	ttys[stdout] = true
	if got := progressStream(ctx); got != stdout {
		t.Fatalf("stdout terminal gave %v", got)
	}
}

func TestInteractiveTracksAnyTerminal(t *testing.T) {
	ctx, _, stderr, ttys := cpTerminals(t)
	if interactive(ctx) {
		t.Fatal("interactive without a terminal")
	}
	ttys[stderr] = true
	if !interactive(ctx) {
		t.Fatal("not interactive with a stderr terminal")
	}
}
