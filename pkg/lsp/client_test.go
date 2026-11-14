package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mockClient(t *testing.T, l *loop, rt *Runtime, dir, script string, extra map[string]string) *Client {
	t.Helper()
	return newClient(l, rt, ClientOptions{ServerID: "mock-" + script, WorkspaceRoot: dir, Command: mockCommand(t),
		Env: mockEnv(script, extra), Cwd: dir})
}

func shutdownClient(t *testing.T, l *loop, c *Client) {
	t.Helper()
	onLoop(t, l, func(ctx context.Context) (struct{}, error) { return struct{}{}, c.shutdown(ctx) })
}

func TestClientLifecycleClean(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	f := writeFile(t, filepath.Join(dir, "x.py"), "print('hi')\n")
	c := mockClient(t, l, rt, dir, "clean", nil)
	diags, err := onLoop(t, l, func(ctx context.Context) ([]map[string]any, error) {
		if err := c.start(ctx); err != nil {
			return nil, err
		}
		if !c.isRunning() {
			t.Error("client not running after start")
		}
		v, err := c.openFile(ctx, f, "python", nil)
		if err != nil || v != 0 {
			t.Errorf("open version %d %v", v, err)
		}
		if _, err := c.waitForDiagnostics(ctx, f, v, "document", 0); err != nil {
			return nil, err
		}
		return c.diagnosticsFor(f, false)
	})
	must(t, err)
	if len(diags) != 0 {
		t.Fatalf("clean server diags %v", diags)
	}
	shutdownClient(t, l, c)
	var running bool
	l.with(func() { running = c.isRunning() })
	if running {
		t.Fatal("still running after shutdown")
	}
}

func TestClientReceivesPublishedErrors(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	f := writeFile(t, filepath.Join(dir, "x.py"), "print('hi')\n")
	c := mockClient(t, l, rt, dir, "errors", nil)
	defer shutdownClient(t, l, c)
	diags, err := onLoop(t, l, func(ctx context.Context) ([]map[string]any, error) {
		if err := c.start(ctx); err != nil {
			return nil, err
		}
		v, err := c.openFile(ctx, f, "python", nil)
		if err != nil {
			return nil, err
		}
		if _, err := c.waitForDiagnostics(ctx, f, v, "document", 0); err != nil {
			return nil, err
		}
		return c.diagnosticsFor(f, false)
	})
	must(t, err)
	if len(diags) != 1 {
		t.Fatalf("diags %v", diags)
	}
	d := diags[0]
	if d["severity"] != int64(1) || d["code"] != "MOCK001" || d["source"] != "mock-lsp" || !strings.Contains(d["message"].(string), "synthetic error") {
		t.Fatalf("diag %v", d)
	}
}

func TestAbortedStartReportsExitStatusAndStderrTail(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	c := mockClient(t, l, rt, t.TempDir(), "oom_abort", nil)
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) { return struct{}{}, c.start(ctx) })
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("want protocol error, got %T %v", err, err)
	}
	if c.state != "error" || c.proc != nil {
		t.Fatalf("state %s proc %v", c.state, c.proc)
	}
	if msg := err.Error(); !strings.Contains(msg, "signal") || !strings.Contains(msg, "JavaScript heap out of memory") {
		t.Fatalf("message %q", msg)
	}
	shutdownClient(t, l, c)
}

func TestInitializeErrorResponseKeepsItsErrorType(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	c := mockClient(t, l, rt, t.TempDir(), "init_error", nil)
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) { return struct{}{}, c.start(ctx) })
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -32602 {
		t.Fatalf("want request error -32602, got %T %v", err, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "bad init") || !strings.Contains(msg, "server exited") {
		t.Fatalf("message %q", msg)
	}
	shutdownClient(t, l, c)
}

func TestReaderExitAtEndOfInitializationRetiresClient(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	c := mockClient(t, l, rt, t.TempDir(), "crash", nil)
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		if err := c.start(ctx); err != nil {
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Errorf("unexpected start error %T %v", err, err)
			}
			return struct{}{}, nil
		}
		if r := c.readerTask; r != nil {
			c.l.awaitAny(ctx, 3*time.Second, r.done)
		}
		return struct{}{}, nil
	})
	must(t, err)
	waitUntil(t, 5*time.Second, "the reader to retire the client", func() bool {
		var done bool
		l.with(func() { done = c.state == "error" && !c.isRunning() && c.proc == nil })
		return done
	})
	shutdownClient(t, l, c)
}

func TestCancelledStartTerminatesSpawnedServer(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	c := mockClient(t, l, rt, t.TempDir(), "slow", nil)
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		var startErr error
		inner := l.spawn(ctx, func(ctx context.Context) { startErr = c.start(ctx) })
		for c.proc == nil {
			if err := l.sleep(ctx, time.Millisecond); err != nil {
				return struct{}{}, err
			}
		}
		p := c.proc
		if woke, _ := l.awaitAny(ctx, 50*time.Millisecond, inner.done); woke {
			t.Error("start finished before the outer budget")
		}
		inner.cancel()
		l.await(ctx, inner.done)
		if !isCancellation(startErr) {
			t.Errorf("start must end cancelled, got %v", startErr)
		}
		if woke, _ := l.awaitAny(ctx, 3*time.Second, p.exited); !woke {
			t.Error("the spawned server outlived its cancelled start")
		}
		if c.state != "error" || c.proc != nil {
			t.Errorf("state %s proc %v", c.state, c.proc)
		}
		return struct{}{}, nil
	})
	must(t, err)
}

func TestReaderFailureRetiresClientAndRejectsLaterWork(t *testing.T) {
	for _, script := range []string{"clean_eof", "malformed_frame"} {
		t.Run(script, func(t *testing.T) {
			rt, _, _ := testRuntime(t)
			l := testLoop(t)
			dir := t.TempDir()
			f := writeFile(t, filepath.Join(dir, "x.py"), "print('hi')\n")
			c := mockClient(t, l, rt, dir, script, nil)
			defer shutdownClient(t, l, c)
			_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
				if err := c.start(ctx); err != nil {
					return struct{}{}, err
				}
				p, reader := c.proc, c.readerTask
				v, err := c.openFile(ctx, f, "python", nil)
				if err != nil {
					return struct{}{}, err
				}
				c.l.awaitAny(ctx, 3*time.Second, reader.done)
				if c.isRunning() {
					t.Error("client still running after its reader ended")
				}
				c.l.awaitAny(ctx, 3*time.Second, p.exited)
				var pe *ProtocolError
				if _, err := c.waitForDiagnostics(ctx, f, v, "document", 3*time.Second); !errors.As(err, &pe) {
					t.Errorf("wait after failure: %v", err)
				}
				if _, err := c.openFile(ctx, f, "python", nil); !errors.As(err, &pe) {
					t.Errorf("open after failure: %v", err)
				}
				return struct{}{}, nil
			})
			must(t, err)
		})
	}
}

func TestShutdownNeverSignalsAServerThatHonoursExit(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	c := mockClient(t, l, rt, t.TempDir(), "clean", nil)
	signalled := 0
	prev := killServerTree
	killServerTree = func(p *serverProc) { signalled++; prev(p) }
	defer func() { killServerTree = prev }()
	var p *serverProc
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		if err := c.start(ctx); err != nil {
			return struct{}{}, err
		}
		p = c.proc
		return struct{}{}, c.shutdown(ctx)
	})
	must(t, err)
	if signalled != 0 {
		t.Fatalf("a server honouring exit was signalled %d times", signalled)
	}
	if rc := p.returncode(); rc == nil || *rc != 0 {
		t.Fatalf("returncode %v", rc)
	}
}

func TestDocsCacheIsLRUBoundedAndReopensEvicted(t *testing.T) {
	prevCap := maxTrackedFiles
	maxTrackedFiles = 3
	defer func() { maxTrackedFiles = prevCap }()
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	var files []string
	for i := range 5 {
		files = append(files, writeFile(t, filepath.Join(dir, "f"+strconv.Itoa(i)+".py"), "print('hi')\n"))
	}
	c := mockClient(t, l, rt, dir, "errors", nil)
	defer shutdownClient(t, l, c)
	var closed []string
	prevSend := clientSendNotification
	clientSendNotification = func(c *Client, ctx context.Context, method string, params any) error {
		if method == "textDocument/didClose" {
			closed = append(closed, params.(map[string]any)["textDocument"].(map[string]any)["uri"].(string))
		}
		return prevSend(c, ctx, method, params)
	}
	defer func() { clientSendNotification = prevSend }()
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		if err := c.start(ctx); err != nil {
			return struct{}{}, err
		}
		for _, f := range files {
			if _, err := c.openFile(ctx, f, "python", nil); err != nil {
				return struct{}{}, err
			}
		}
		if c.docs.len() != 3 || c.docs.get(files[0]) != nil {
			t.Errorf("docs %d, first kept %v", c.docs.len(), c.docs.get(files[0]) != nil)
		}
		if len(closed) != 2 || closed[0] != mustURI(t, files[0]) || closed[1] != mustURI(t, files[1]) {
			t.Errorf("closed %v", closed)
		}
		v, err := c.openFile(ctx, files[0], "python", nil)
		if err != nil || v != 0 || c.docs.len() != 3 {
			t.Errorf("reopen version %d docs %d %v", v, c.docs.len(), err)
		}
		if _, err := c.waitForDiagnostics(ctx, files[0], v, "document", 0); err != nil {
			return struct{}{}, err
		}
		if d, _ := c.diagnosticsFor(files[0], false); len(d) == 0 {
			t.Error("no diagnostics after reopen")
		}
		return struct{}{}, nil
	})
	must(t, err)
}

func TestClientHandlesLargeAndOversizedStderrLines(t *testing.T) {
	for _, tc := range []struct {
		script string
		extra  map[string]string
	}{{"large_stderr", nil}, {"oversized_stderr", map[string]string{"MOCK_LSP_STDERR_BYTES": strconv.Itoa(streamLimit + 1)}}} {
		t.Run(tc.script, func(t *testing.T) {
			rt, _, _ := testRuntime(t)
			l := testLoop(t)
			dir := t.TempDir()
			f := writeFile(t, filepath.Join(dir, "x.py"), "print('hi')\n")
			c := mockClient(t, l, rt, dir, tc.script, tc.extra)
			diags, err := onLoop(t, l, func(ctx context.Context) ([]map[string]any, error) {
				if err := c.start(ctx); err != nil {
					return nil, err
				}
				if !c.isRunning() {
					t.Error("not running")
				}
				v, err := c.openFile(ctx, f, "python", nil)
				if err != nil {
					return nil, err
				}
				if _, err := c.waitForDiagnostics(ctx, f, v, "document", 0); err != nil {
					return nil, err
				}
				return c.diagnosticsFor(f, false)
			})
			must(t, err)
			if len(diags) != 0 {
				t.Fatalf("diags %v", diags)
			}
			shutdownClient(t, l, c)
		})
	}
}

func TestVersionlessPushReadDuringDidChangeSendIsFresh(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "x.py"), "bad\n")
	c := mockClient(t, l, rt, dir, "versionless", nil)
	defer shutdownClient(t, l, c)
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		if err := c.start(ctx); err != nil {
			return struct{}{}, err
		}
		first, err := c.openFile(ctx, src, "python", nil)
		if err != nil {
			return struct{}{}, err
		}
		if ok, err := c.waitForDiagnostics(ctx, src, first, "document", 5*time.Second); err != nil || !ok {
			t.Errorf("first wait %v %v", ok, err)
		}
		prev := clientSendNotification
		clientSendNotification = func(c *Client, ctx context.Context, method string, params any) error {
			seen := c.pushCounter
			if err := prev(c, ctx, method, params); err != nil {
				return err
			}
			if method == "textDocument/didChange" {
				for c.pushCounter == seen {
					if err := c.l.sleep(ctx, time.Millisecond); err != nil {
						return err
					}
				}
			}
			return nil
		}
		defer func() { clientSendNotification = prev }()
		must(t, os.WriteFile(src, []byte("clean\n"), 0o644))
		version, err := c.openFile(ctx, src, "python", nil)
		if err != nil {
			return struct{}{}, err
		}
		if ok, err := c.waitForDiagnostics(ctx, src, version, "document", time.Second); err != nil || !ok {
			t.Errorf("versionless push during the send must be fresh: %v %v", ok, err)
		}
		if d, _ := c.diagnosticsFor(src, true); len(d) != 0 {
			t.Errorf("fresh diags %v", d)
		}
		return struct{}{}, nil
	})
	must(t, err)
}

func TestIncrementalReplacementsPreserveServerDocument(t *testing.T) {
	for _, original := range []string{"const face = \"\U0001F600\";", "// header\nconst letter = \"\U00010400\";",
		"const word = \"中文\";", "const face = \"\U0001F600\";\n", ""} {
		rt, _, _ := testRuntime(t)
		l := testLoop(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "sample.ts")
		c := newClient(l, rt, ClientOptions{ServerID: "incremental", WorkspaceRoot: dir, Command: mockCommand(t),
			Env: mockEnv("incremental", nil)})
		_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
			if err := c.start(ctx); err != nil {
				return struct{}{}, err
			}
			for _, text := range []string{original, "const next = \"\U0001F680\";", "const done = true;"} {
				must(t, os.WriteFile(path, []byte(text), 0o644))
				if _, err := c.openFile(ctx, path, "typescript", nil); err != nil {
					return struct{}{}, err
				}
				hover, err := waitFor(ctx, 5*time.Second, func(ctx context.Context) (any, error) {
					return c.sendRequest(ctx, "textDocument/hover", map[string]any{
						"textDocument": map[string]any{"uri": mustURI(t, path)}, "position": map[string]any{"line": 0, "character": 0}})
				})
				if err != nil {
					return struct{}{}, err
				}
				if got := hover.(map[string]any)["contents"].(map[string]any)["value"]; got != text {
					t.Errorf("server mirror %q, disk %q", got, text)
				}
			}
			return struct{}{}, c.shutdown(ctx)
		})
		must(t, err)
	}
}
