//go:build windows

package pm

import (
	"context"
	"io"
	"os"
	"testing"
)

func TestWindowsTTYWithoutAConsoleGetsPlainLines(t *testing.T) {
	nul, err := os.OpenFile("NUL", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nul.Close()
	if !streamIsTerminal(nul) {
		t.Fatal("NUL is a character device that isatty answers true for")
	}
	cpSwap(t, &stderrStream, func() io.Writer { return nul })
	if got := progressStream(withSyncOutput(context.Background(), nul)); got != nil {
		t.Fatalf("NUL gave progress stream %v", got)
	}
}
