package logging

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactDetachedStdioRedactsBothStreamsOnce(t *testing.T) {
	dir := t.TempDir()
	outFile, err := os.Create(filepath.Join(dir, "gateway-stdio.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outFile.Close() }()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, outFile
	t.Cleanup(func() {
		flushDetachedStdio()
		os.Stdout, os.Stderr = origOut, origErr
	})
	secret := "sk-proj-DETACHEDSTDIO" + strings.Repeat("c", 24)
	for range 2 {
		if err := RedactDetachedStdio(); err != nil {
			t.Fatal(err)
		}
	}
	stdioMu.Lock()
	wrapped := len(redactedStdios)
	stdioMu.Unlock()
	if wrapped != 2 {
		t.Fatalf("%d wrapped streams", wrapped)
	}
	_, _ = io.WriteString(os.Stdout, "provider said "+secret+"\n")
	_, _ = io.WriteString(os.Stderr, "stderr echo "+secret+"\n")
	flushDetachedStdio()
	if os.Stdout != outFile || os.Stderr != outFile {
		t.Fatal("streams not restored")
	}
	got := readText(t, outFile.Name())
	if strings.Contains(got, secret) || !strings.Contains(got, "provider said sk-...") || !strings.Contains(got, "stderr echo sk-...") {
		t.Fatalf("stdio log %q", got)
	}
}

// Handler that blocks every record until released
type wedgedHandler struct {
	handlerCore
	release chan struct{}
}

func (w *wedgedHandler) Handle(context.Context, *Record) bool {
	<-w.release
	return true
}

func (w *wedgedHandler) Close() error { return nil }

func TestDrainLogQueueBoundsAWedgedListener(t *testing.T) {
	testHome(t)
	w := &wedgedHandler{release: make(chan struct{})}
	registerQueuedHandler(w)
	GetLogger("agent.drain").Warning("wedged record")
	start := time.Now()
	DrainLogQueue(200 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain waited %v", elapsed)
	}
	close(w.release)
}

func TestDrainLogQueueWritesPendingRecords(t *testing.T) {
	h := testHome(t)
	mustSetup(t, Options{Home: h})
	GetLogger("agent.drain").Warning("drained before exit")
	DrainLogQueue(5 * time.Second)
	if !strings.Contains(readText(t, filepath.Join(h, "logs", "agent.log")), "drained before exit") {
		t.Fatal("pending record lost")
	}
}

func TestSetupHomeLoggingAddsTheModeLog(t *testing.T) {
	testHome(t)
	worker := otherDir(t, "cron-home")
	if err := SetupHomeLogging(worker, "gateway"); err != nil {
		t.Fatal(err)
	}
	GetLogger("gateway.run").Info("worker gateway line")
	FlushLogQueue()
	if !strings.Contains(readText(t, filepath.Join(worker, "logs", "gateway.log")), "worker gateway line") {
		t.Fatal("mode log missing")
	}
}
