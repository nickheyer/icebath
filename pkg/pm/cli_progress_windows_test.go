//go:build windows

package pm

import (
	"context"
	"io"
	"os"
	"testing"
)

func TestCPWindowsTTYWithoutAConsoleGetsPlainLines(t *testing.T) {
	nul, err := os.OpenFile("NUL", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nul.Close()
	cpSwap(t, &stderrStream, func() io.Writer { return nul })
	if got := progressStream(withSyncOutput(context.Background(), nul)); got != nil {
		t.Fatalf("NUL gave progress stream %v", got)
	}
}
