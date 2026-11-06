package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type tsoBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *tsoBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *tsoBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Binds w as the process stream like a sys attribute assignment and returns the restore
func tsoBind(attr string, w io.Writer) func() {
	streamInstallLock.Lock()
	prev := processStreams[attr]
	processStreams[attr] = w
	streamInstallLock.Unlock()
	return func() {
		streamInstallLock.Lock()
		processStreams[attr] = prev
		streamInstallLock.Unlock()
	}
}

// Swaps the install registries for empty ones like the monkeypatched module dicts
func tsoFreshRegistries(t *testing.T) {
	streamInstallLock.Lock()
	installed, sinks, states := installedStreams, streamSinks, routingStates
	installedStreams, streamSinks, routingStates = map[string]*threadRoutingStream{}, map[string]io.Writer{}, map[string]*routingState{}
	streamInstallLock.Unlock()
	t.Cleanup(func() {
		streamInstallLock.Lock()
		installedStreams, streamSinks, routingStates = installed, sinks, states
		streamInstallLock.Unlock()
	})
}

func TestStderrIsAlsoRoutedPerThread(t *testing.T) {
	realErr := &tsoBuffer{}
	restore := tsoBind("stderr", realErr)
	defer restore()
	silenced := ThreadScopedSilence(context.Background())
	io.WriteString(ScopedStderr(silenced), "err-dropped\n")
	io.WriteString(ScopedStderr(context.Background()), "err-kept\n")
	out := realErr.String()
	if strings.Contains(out, "err-dropped") || !strings.Contains(out, "err-kept") {
		t.Fatalf("stderr routing wrong %q", out)
	}
}

func TestManyConcurrentSilencedAndLoudThreads(t *testing.T) {
	realOut := &tsoBuffer{}
	restore := tsoBind("stdout", realOut)
	defer restore()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx := ThreadScopedSilence(context.Background())
			fmt.Fprintln(ScopedStdout(ctx), fmt.Sprintf("S%d", i))
			time.Sleep(50 * time.Millisecond)
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			time.Sleep(20 * time.Millisecond)
			fmt.Fprintln(ScopedStdout(context.Background()), fmt.Sprintf("L%d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	captured := realOut.String()
	for i := 0; i < 5; i++ {
		if strings.Contains(captured, fmt.Sprintf("S%d", i)) {
			t.Fatalf("silenced S%d leaked", i)
		}
		if !strings.Contains(captured, fmt.Sprintf("L%d", i)) {
			t.Fatalf("loud L%d swallowed", i)
		}
	}
}

func TestRepeatedContextsNeverWriteToAClosedSink(t *testing.T) {
	for i := 0; i < 3; i++ {
		ctx := ThreadScopedSilence(context.Background())
		if _, err := io.WriteString(ScopedStdout(ctx), "hidden\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := ScopedStdout(ctx).(routedWriter).Fd(); err != nil {
			t.Fatalf("silenced fileno failed %v", err)
		}
	}
}

type tsoSink struct{ bytes.Buffer }

func TestTemporaryGlobalRedirectsDoNotAllocateNewSinks(t *testing.T) {
	tsoFreshRegistries(t)
	var opened []io.Writer
	prevOpen := openStreamSink
	openStreamSink = func() (io.Writer, error) {
		s := &tsoSink{}
		opened = append(opened, s)
		return s, nil
	}
	defer func() { openStreamSink = prevOpen }()
	defer tsoBind("stdout", &tsoBuffer{})()
	defer tsoBind("stderr", &tsoBuffer{})()
	ThreadScopedSilence(context.Background())
	if len(opened) != 2 {
		t.Fatalf("opened %d sinks", len(opened))
	}
	original := map[string]*threadRoutingStream{"stdout": installedStreams["stdout"], "stderr": installedStreams["stderr"]}
	for i := 0; i < 20; i++ {
		restoreOut := tsoBind("stdout", &tsoBuffer{})
		restoreErr := tsoBind("stderr", &tsoBuffer{})
		ctx := ThreadScopedSilence(context.Background())
		fmt.Fprintln(ScopedStdout(ctx), "hidden")
		restoreErr()
		restoreOut()
	}
	ThreadScopedSilence(context.Background())
	if len(opened) != 2 {
		t.Fatalf("redirects opened %d sinks", len(opened))
	}
	if installedStreams["stdout"] != original["stdout"] || installedStreams["stderr"] != original["stderr"] || len(installedStreams) != 2 {
		t.Fatal("installed proxies changed across redirects")
	}
}

func TestAgentBuildsKeepTheInstalledRoutingProxy(t *testing.T) {
	tsoFreshRegistries(t)
	defer tsoBind("stdout", &tsoBuffer{})()
	defer tsoBind("stderr", &tsoBuffer{})()
	ThreadScopedSilence(context.Background())
	routers := [2]io.Writer{processStreams["stdout"], processStreams["stderr"]}
	for i := 0; i < 3; i++ {
		ThreadScopedSilence(context.Background())
	}
	if processStreams["stdout"] != routers[0] || processStreams["stderr"] != routers[1] {
		t.Fatal("repeated silences stacked new routing proxies")
	}
}

func TestSilenceSurvivesRedirectRestoringAnOlderProxy(t *testing.T) {
	tsoFreshRegistries(t)
	passthrough := &tsoBuffer{}
	defer tsoBind("stdout", passthrough)()
	ThreadScopedSilence(context.Background())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	redirected := &tsoBuffer{}
	restore := tsoBind("stdout", redirected)
	go func() {
		defer close(done)
		ctx := ThreadScopedSilence(context.Background())
		close(entered)
		<-release
		fmt.Fprintln(ScopedStdout(ctx), "must-stay-silenced")
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never entered")
	}
	restore()
	close(release)
	<-done
	if strings.Contains(passthrough.String(), "must-stay-silenced") || strings.Contains(redirected.String(), "must-stay-silenced") {
		t.Fatal("silenced output leaked after redirect restore")
	}
}
