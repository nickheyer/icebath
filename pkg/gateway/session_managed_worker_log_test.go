package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerLogIsBoundedAndScrubsTheWorkersExactSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", managedWorkerLogName)
	assignment := "opaque-assignment-secret-value"
	noise := strings.Repeat("line "+assignment+" "+strings.Repeat("x", 200)+"\n", 12000) + strings.Repeat("y", 300000) + "\nlast\n"
	drainWorkerStderr(context.Background(), io.NopCloser(bytes.NewReader([]byte(noise))), path, 7, func() []string { return []string{assignment} })
	files := []string{filepath.Join(dir, "logs", managedWorkerLogName+".1"), path}
	var text strings.Builder
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxWorkerLogBytes {
			t.Fatalf("%s holds %d bytes", file, info.Size())
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(data)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "logs", managedWorkerLogName+".2")); len(matches) != 0 {
		t.Fatalf("second rotation %v", matches)
	}
	got := text.String()
	if strings.Contains(got, assignment) || !strings.Contains(got, "[truncated]") || !strings.HasSuffix(got, "[pid 7] last\n") {
		t.Fatalf("log tail %q", got[max(0, len(got)-200):])
	}
}
