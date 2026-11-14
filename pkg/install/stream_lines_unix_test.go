//go:build unix

package install

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stdout lines, exit code and abandonment of a streamed shell script, the sink stalling once on the stallAt line
func streamShell(t *testing.T, ctx context.Context, script string, grace time.Duration, stallAt string) ([]string, int, bool) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	code, abandoned, err := streamLines(exec.CommandContext(ctx, "sh", "-c", script), grace, func(stream LogStream, line string) {
		if stream != Stdout {
			return
		}
		if stallAt != "" && line == stallAt {
			time.Sleep(300 * time.Millisecond)
		}
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines, code, abandoned
}

// powershell.rs pump_child_returns_when_a_grandchild_still_holds_the_pipe
func TestStreamLinesReturnsWhenAGrandchildStillHoldsThePipe(t *testing.T) {
	started := time.Now()
	lines, code, abandoned := streamShell(t, context.Background(), "sleep 30 & echo hello; exit 7", 300*time.Millisecond, "")
	if !abandoned || code != 7 || !reflect.DeepEqual(lines, []string{"hello"}) {
		t.Fatalf("abandoned %v code %d lines %v", abandoned, code, lines)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

// powershell.rs pump_child_does_not_pay_the_grace_when_pipes_close_cleanly
func TestStreamLinesDoesNotPayTheGraceWhenPipesCloseCleanly(t *testing.T) {
	started := time.Now()
	lines, code, abandoned := streamShell(t, context.Background(), "echo one; echo two >&2; echo three; exit 3", 30*time.Second, "")
	if abandoned || code != 3 || !reflect.DeepEqual(lines, []string{"one", "three"}) {
		t.Fatalf("abandoned %v code %d lines %v", abandoned, code, lines)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

// powershell.rs pump_child_streams_a_flood_without_metering_it, the tail surviving a child that exits while the sink is behind
func TestStreamLinesKeepsEveryLineOfAFloodFromAChildThatOutrunsTheSink(t *testing.T) {
	started := time.Now()
	lines, code, abandoned := streamShell(t, context.Background(),
		"i=0; while [ $i -lt 20000 ]; do echo line$i; i=$((i+1)); done; exit 0", 30*time.Second, "line19000")
	if abandoned || code != 0 || len(lines) != 20000 || lines[len(lines)-1] != "line19999" {
		last := ""
		if len(lines) > 0 {
			last = lines[len(lines)-1]
		}
		t.Fatalf("abandoned %v code %d lines %d last %q", abandoned, code, len(lines), last)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

// powershell.rs pump_child_cancellation_returns_even_with_the_pipe_stranded
func TestStreamLinesCancellationReturnsEvenWithThePipeStranded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	started := time.Now()
	lines, code, abandoned := streamShell(t, ctx, "sleep 30 & echo working; sleep 30", 300*time.Millisecond, "")
	if !abandoned || code == 0 || !reflect.DeepEqual(lines, []string{"working"}) {
		t.Fatalf("abandoned %v code %d lines %v", abandoned, code, lines)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

// Line endings dropped, an unterminated tail reported and an overlong run split at the line cap
func TestLineWriterSplitsLines(t *testing.T) {
	var got []string
	w := &lineWriter{report: func(line string) { got = append(got, line) }}
	for _, chunk := range []string{"a\r\nb", "c\n\nbad\xff\n", "tail"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %d %v", n, err)
		}
	}
	w.Close()
	if want := []string{"a", "bc", "", "bad�", "tail"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	got = nil
	long := strings.Repeat("x", maxStreamedLine+5)
	fmt.Fprint(w, long[:10])
	fmt.Fprint(w, long[10:]+"\ny\n")
	w.Close()
	if len(got) != 3 || len(got[0]) != maxStreamedLine || got[1] != "xxxxx" || got[2] != "y" {
		t.Fatalf("overlong run split into %d lines", len(got))
	}
}
