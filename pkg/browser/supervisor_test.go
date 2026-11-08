package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeFrame struct {
	conn   int
	method string
	sid    string
}

// CDP endpoint giving each connection its own page session S1, S2 and so on
type fakeSupervisorCDP struct {
	mu     sync.Mutex
	frames []fakeFrame
	conns  []*testWSServer
	url    string
	delay  bool
}

func newFakeSupervisorCDP(t *testing.T, delay bool) *fakeSupervisorCDP {
	t.Helper()
	f := &fakeSupervisorCDP{delay: delay}
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		f.mu.Lock()
		f.conns = append(f.conns, s)
		conn := len(f.conns)
		f.mu.Unlock()
		for {
			op, payload, err := s.readMessage()
			if err != nil || op == opClose {
				return
			}
			decoded, err := pyjson.LoadsBytes(payload)
			if err != nil {
				return
			}
			msg := decoded.(*pyjson.Object)
			method, sid := objStr(msg, "method"), objStr(msg, "sessionId")
			id, _ := msg.Get("id")
			f.mu.Lock()
			f.frames = append(f.frames, fakeFrame{conn, method, sid})
			f.mu.Unlock()
			var res any
			if f.delay {
				res = obj("echo", method)
				switch method {
				case "Target.getTargets":
					res = obj("targetInfos", []any{obj("targetId", "fixture-parent", "type", "page", "url", "about:blank")})
				case "Target.attachToTarget":
					if anyGet(anyGet(msg, "params"), "targetId") == "late-child" {
						time.Sleep(200 * time.Millisecond)
						res = obj("sessionId", "late-owned-session")
					} else {
						res = obj("sessionId", "default-session")
					}
				}
			} else {
				if method == "Test.hang" {
					continue
				}
				if method == "Test.error" {
					text, _ := pyjson.Dumps(obj("id", id, "error", obj("code", -32000, "message", "boom")))
					_ = s.writeText(text)
					continue
				}
				params, _ := msg.Get("params")
				var sidVal any
				if sid != "" {
					sidVal = sid
				}
				res = obj("echo", method, "sessionId", sidVal, "params", params)
				switch method {
				case "Target.getTargets":
					res = obj("targetInfos", []any{obj("targetId", "T1", "type", "page", "url", "about:blank")})
				case "Target.attachToTarget":
					res = obj("sessionId", "S"+strconv.Itoa(conn))
				}
			}
			text, _ := pyjson.Dumps(obj("id", id, "result", res))
			if s.writeText(text) != nil {
				return
			}
		}
	})
	t.Cleanup(srv.Close)
	f.url = wsURL + "/devtools/browser/x"
	return f
}

// Closes the newest connection with a normal close frame
func (f *fakeSupervisorCDP) dropCurrent() {
	f.mu.Lock()
	s := f.conns[len(f.conns)-1]
	f.mu.Unlock()
	_ = s.writeFrame(true, opClose, closePayload(1000, ""))
	_ = s.conn.Close()
}

func (f *fakeSupervisorCDP) methodsOn(conn int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.frames {
		if fr.conn == conn {
			out = append(out, fr.method)
		}
	}
	return out
}

func (f *fakeSupervisorCDP) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func isClass(err error, class string) bool { return err != nil && pyrepr.ExceptionName(err) == class }

func countOf(items []string, want string) int {
	n := 0
	for _, s := range items {
		if s == want {
			n++
		}
	}
	return n
}

func contains(items []string, want string) bool { return countOf(items, want) > 0 }

func freshRegistry(t *testing.T) *supervisorRegistry {
	t.Helper()
	reg := newSupervisorRegistry()
	t.Cleanup(reg.stopAll)
	return reg
}

func TestCallRoundTripsOnCapturedConnection(t *testing.T) {
	env := testEnv(t)
	cdp := newFakeSupervisorCDP(t, false)
	reg := freshRegistry(t)
	if _, err := reg.getOrStart(env, "t", cdp.url, DefaultDialogPolicy, DefaultDialogTimeoutS, 5); err != nil {
		t.Fatal(err)
	}
	handle, err := reg.capture("t", 10)
	if err != nil || !handle.IsValid() || handle.PageSessionID != "S1" {
		t.Fatalf("capture %v %v", handle, err)
	}
	reply, err := handle.Call("Runtime.evaluate", obj("expression", "1"), handle.PageSessionID, 10.0, nil)
	want := `{"echo": "Runtime.evaluate", "sessionId": "S1", "params": {"expression": "1"}}`
	if got, _ := pyjson.Dumps(anyGet(reply, "result")); err != nil || got != want {
		t.Fatalf("got %s %v", got, err)
	}
	reply, err = handle.Call("Target.getTargets", nil, "", 10.0, nil)
	if err != nil || anyGet(dig(reply, "result", "targetInfos").([]any)[0], "targetId") != "T1" {
		t.Fatalf("browser level %v %v", reply, err)
	}
	if _, err := handle.Call("Test.error", nil, "", 10.0, nil); !isClass(err, "RuntimeError") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error reply %v", err)
	}
	started := time.Now()
	if _, err := handle.Call("Test.hang", nil, "", 0.3, nil); !isClass(err, "TimeoutError") || time.Since(started) >= 2*time.Second {
		t.Fatalf("hang %v", err)
	}
	if !handle.IsValid() {
		t.Fatal("error or timeout invalidated the handle")
	}
	if _, err := reg.capture("no-such-task", 10); !isClass(err, "CapturedCDPInvalid") {
		t.Fatalf("capture missing %v", err)
	}
}

func TestHandleNeverFollowsAReconnectOrReplacement(t *testing.T) {
	env := testEnv(t)
	cdp := newFakeSupervisorCDP(t, false)
	reg := freshRegistry(t)
	pin(t, &supervisorSleep, func(<-chan struct{}, time.Duration) {})
	sup, err := reg.getOrStart(env, "t", cdp.url, DefaultDialogPolicy, DefaultDialogTimeoutS, 5)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := reg.capture("t", 10)
	errs := make(chan error, 1)
	go func() {
		_, err := old.Call("Test.hang", nil, "", 30.0, nil)
		errs <- err
	}()
	waitFor(t, func() bool { return contains(cdp.methodsOn(1), "Test.hang") })
	started := time.Now()
	cdp.dropCurrent()
	select {
	case err := <-errs:
		if !isClass(err, "CapturedCDPInvalid") || time.Since(started) >= 5*time.Second {
			t.Fatalf("in flight call %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in flight call never failed")
	}
	waitFor(t, func() bool { return cdp.connCount() == 2 && sup.snapshot().Active })
	if old.IsValid() {
		t.Fatal("old handle followed the reconnect")
	}
	if _, err := old.Call("Probe.afterReconnect", nil, old.PageSessionID, 10.0, nil); !isClass(err, "CapturedCDPInvalid") {
		t.Fatalf("after reconnect %v", err)
	}
	if contains(cdp.methodsOn(2), "Probe.afterReconnect") {
		t.Fatal("call retargeted the new connection")
	}
	fresh, _ := reg.capture("t", 10)
	if fresh.PageSessionID != "S2" {
		t.Fatalf("fresh session %s", fresh.PageSessionID)
	}
	if reply, err := fresh.Call("Probe.fresh", nil, fresh.PageSessionID, 10.0, nil); err != nil || dig(reply, "result", "sessionId") != "S2" {
		t.Fatalf("fresh call %v %v", reply, err)
	}
	reg.stop("t")
	if fresh.IsValid() {
		t.Fatal("stop left the handle valid")
	}
	if _, err := fresh.Call("Probe.afterStop", nil, "", 10.0, nil); !isClass(err, "CapturedCDPInvalid") {
		t.Fatalf("after stop %v", err)
	}
}

type capturePair struct {
	server *fakeSupervisorCDP
	reg    *supervisorRegistry
	sup    *cdpSupervisor
	handle *CapturedCDP
}

func newCapturePair(t *testing.T) capturePair {
	t.Helper()
	env := testEnv(t)
	server := newFakeSupervisorCDP(t, true)
	reg := freshRegistry(t)
	sup, err := reg.getOrStart(env, "fixture", server.url, DefaultDialogPolicy, DefaultDialogTimeoutS, 5)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := reg.capture("fixture", 10)
	if err != nil {
		t.Fatal(err)
	}
	return capturePair{server, reg, sup, handle}
}

func TestUnboundedReplyRetainsLateAttachmentForConsumerCleanup(t *testing.T) {
	p := newCapturePair(t)
	var abandoned sync.WaitGroup
	abandoned.Add(1)
	var outcomes []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		reply, err := p.handle.Call("Target.attachToTarget", obj("targetId", "late-child", "flatten", true), "", nil, nil)
		if err != nil {
			return
		}
		outcomes = append(outcomes, dig(reply, "result", "sessionId").(string))
		abandoned.Wait()
		_, _ = p.handle.Call("Target.detachFromTarget", obj("sessionId", outcomes[len(outcomes)-1]), "", 10.0, nil)
	}()
	waitFor(t, func() bool { return countOf(p.server.methodsOn(1), "Target.attachToTarget") == 2 })
	abandoned.Done()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker still alive")
	}
	if len(outcomes) != 1 || outcomes[0] != "late-owned-session" || countOf(p.server.methodsOn(1), "Target.detachFromTarget") != 1 {
		t.Fatalf("outcomes %v methods %v", outcomes, p.server.methodsOn(1))
	}
}

func TestDispatchDeadlineRefusesQueuedCommandAfterTimeout(t *testing.T) {
	p := newCapturePair(t)
	entered := make(chan struct{})
	p.sup.currentLoop().submit(func() {
		close(entered)
		time.Sleep(400 * time.Millisecond)
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("loop never ran the blocker")
	}
	if _, err := p.handle.Call("Probe.expired", nil, "", 0.1, nil); !isClass(err, "TimeoutError") {
		t.Fatalf("got %v", err)
	}
	if _, err := p.handle.Call("Target.getTargets", nil, "", 2.0, nil); err != nil {
		t.Fatal(err)
	}
	if contains(p.server.methodsOn(1), "Probe.expired") {
		t.Fatal("expired command was sent")
	}
}

func TestBeforeSendChecksCancellationOnSupervisorLoop(t *testing.T) {
	p := newCapturePair(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var abandoned sync.Mutex
	isAbandoned := false
	p.sup.currentLoop().submit(func() {
		close(entered)
		select {
		case <-release:
		case <-time.After(4 * time.Second):
		}
	})
	<-entered
	failures := make(chan error, 1)
	go func() {
		_, err := p.handle.Call("Probe.cancelled", nil, "", 2.0, func() any {
			if !p.sup.currentLoop().onLoop() {
				return errors.New("before_send ran off the supervisor loop")
			}
			abandoned.Lock()
			defer abandoned.Unlock()
			if isAbandoned {
				return pyError("ValueError", "fixture cancelled")
			}
			return nil
		})
		failures <- err
	}()
	abandoned.Lock()
	isAbandoned = true
	abandoned.Unlock()
	close(release)
	select {
	case err := <-failures:
		if !isClass(err, "ValueError") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller never returned")
	}
	if contains(p.server.methodsOn(1), "Probe.cancelled") {
		t.Fatal("cancelled command was sent")
	}
}

func TestUnboundedReplyFailsPromptlyOnSocketClose(t *testing.T) {
	p := newCapturePair(t)
	failures := make(chan error, 1)
	go func() {
		_, err := p.handle.Call("Target.attachToTarget", obj("targetId", "late-child"), "", nil, nil)
		failures <- err
	}()
	waitFor(t, func() bool { return countOf(p.server.methodsOn(1), "Target.attachToTarget") == 2 })
	p.server.dropCurrent()
	select {
	case err := <-failures:
		if !isClass(err, "CapturedCDPInvalid") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unbounded call still waiting")
	}
}

func TestUnboundedWaitEndsWhenLoopClosesWithDispatchQueued(t *testing.T) {
	p := newCapturePair(t)
	pin(t, &submitCaptureSend, func(*supervisorLoop, func()) bool { return true })
	failures := make(chan error, 1)
	go func() {
		_, err := p.handle.Call("Probe.dropped", nil, "", nil, nil)
		failures <- err
	}()
	p.reg.stopAll()
	select {
	case err := <-failures:
		if !isClass(err, "CapturedCDPInvalid") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("unbounded wait never ended")
	}
}

func TestInvalidTimeout(t *testing.T) {
	p := newCapturePair(t)
	for _, timeout := range []any{0, -1, math.Inf(1), math.NaN(), 1e300, true, "5"} {
		before := len(p.server.methodsOn(1))
		if _, err := p.handle.Call("Probe.invalid", nil, "", timeout, nil); !isClass(err, "ValueError") {
			t.Fatalf("%v: %v", timeout, err)
		}
		if len(p.server.methodsOn(1)) != before {
			t.Fatalf("%v reached the server", timeout)
		}
	}
}

func TestAsyncValidatorRefuses(t *testing.T) {
	p := newCapturePair(t)
	before := len(p.server.methodsOn(1))
	awaitable := func() any { return func() {} }
	if _, err := p.handle.Call("Probe.async", nil, "", 10.0, awaitable); !isClass(err, "TypeError") || !strings.Contains(err.Error(), "synchronously") {
		t.Fatalf("got %v", err)
	}
	if len(p.server.methodsOn(1)) != before {
		t.Fatal("refused command reached the server")
	}
}

// Start stand in giving the supervisor a live thread and loop until stop
func fakeSupervisorStart(t *testing.T) *[]*cdpSupervisor {
	t.Helper()
	var created []*cdpSupervisor
	pin(t, &supervisorStart, func(s *cdpSupervisor, _ float64) error {
		loop := newSupervisorLoop()
		quit, done, stop := make(chan struct{}), make(chan struct{}), make(chan struct{})
		s.ctlMu.Lock()
		s.loop, s.threadDone, s.stopCh = loop, done, stop
		s.ctlMu.Unlock()
		go loop.serve(quit)
		go func() {
			<-stop
			close(quit)
			close(done)
		}()
		waitFor(t, loop.isRunning)
		created = append(created, s)
		return nil
	})
	return &created
}

func TestCacheHitReturnsSameInstanceWhenHealthy(t *testing.T) {
	env := testEnv(t)
	reg := newSupervisorRegistry()
	created := fakeSupervisorStart(t)
	first, _ := reg.getOrStart(env, "t1", "http://h/1", DefaultDialogPolicy, DefaultDialogTimeoutS, 15)
	second, _ := reg.getOrStart(env, "t1", "http://h/1", DefaultDialogPolicy, DefaultDialogTimeoutS, 15)
	if first != second || len(*created) != 1 {
		t.Fatalf("recreated a healthy supervisor: %d", len(*created))
	}
	first.stop(5)
}

func TestMissingThreadAndLoopAttrsTriggerRecreate(t *testing.T) {
	env := testEnv(t)
	reg := newSupervisorRegistry()
	fakeSupervisorStart(t)
	broken := &cdpSupervisor{cdpURL: "http://h/4"}
	reg.byTask["t4"] = broken
	fresh, err := reg.getOrStart(env, "t4", "http://h/4", DefaultDialogPolicy, DefaultDialogTimeoutS, 15)
	if err != nil || fresh == broken || reg.byTask["t4"] != fresh {
		t.Fatalf("broken entry kept: %v", err)
	}
	fresh.stop(5)
}

type closingWS struct{}

func (closingWS) WriteText(context.Context, []byte) error { return nil }
func (closingWS) ReadMessage(context.Context) (byte, []byte, error) {
	return 0, nil, &ErrWSClosed{Code: 1000}
}
func (closingWS) Close(int, string) error { return nil }

// Env whose logger records every message at warning level and above
func warningLog(t *testing.T) (*tools.Env, *bytes.Buffer) {
	t.Helper()
	env := testEnv(t)
	buf := &bytes.Buffer{}
	env.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return env, buf
}

func TestPostAttachReconnectsStopAtBudgetAndUnregister(t *testing.T) {
	env, logs := warningLog(t)
	sup, _ := newCDPSupervisor(env, "bounded-reconnect", "ws://127.0.0.1:9222", DefaultDialogPolicy, DefaultDialogTimeoutS)
	supervisors.mu.Lock()
	supervisors.byTask[sup.taskID] = sup
	supervisors.mu.Unlock()
	t.Cleanup(func() { supervisors.pop(sup.taskID) })
	budget := maxPostAttachReconnectFailures
	dials := 0
	pin(t, &supervisorDial, func(string) (supervisorWS, error) {
		dials++
		if dials == 1 {
			return closingWS{}, nil
		}
		if dials <= budget+1 {
			return nil, pyError("ConnectionError", "CDP endpoint is gone")
		}
		select {}
	})
	pin(t, &supervisorAttachInitialPage, func(*cdpSupervisor) error { return nil })
	pin(t, &supervisorReadLoop, func(*cdpSupervisor, context.Context) {})
	pin(t, &supervisorSleep, func(<-chan struct{}, time.Duration) {})
	done := make(chan error, 1)
	go func() { done <- sup.run() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect loop never stopped")
	}
	final := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "stopped after") {
			final++
			if !strings.Contains(line, strconv.Itoa(budget)+" failed reconnect") {
				t.Fatalf("final line %s", line)
			}
		}
	}
	if dials != budget+1 || final != 1 || sup.snapshot().Active || supervisors.get(sup.taskID) != nil {
		t.Fatalf("dials %d final %d", dials, final)
	}
}

func TestInitialConnectFailureStaysFatalForStart(t *testing.T) {
	env := testEnv(t)
	sup, _ := newCDPSupervisor(env, "initial-failure", "ws://127.0.0.1:9222", DefaultDialogPolicy, DefaultDialogTimeoutS)
	pin(t, &supervisorDial, func(string) (supervisorWS, error) {
		return nil, pyError("ConnectionError", "CDP endpoint is unavailable")
	})
	_ = sup.run()
	if !isClass(sup.startErr, "ConnectionError") || !sup.ready.IsSet() || sup.snapshot().Active {
		t.Fatalf("start error %v", sup.startErr)
	}
}

// Supervisor with a running loop and an attached page whose CDP replies come from fn
func evalSupervisor(t *testing.T, sessionID string, fn func(method string, params any) (*pyjson.Object, error)) *cdpSupervisor {
	t.Helper()
	sup, _ := newCDPSupervisor(testEnv(t), "eval", "ws://x", DefaultDialogPolicy, DefaultDialogTimeoutS)
	sup.active, sup.pageSessionID = true, sessionID
	loop, quit := newSupervisorLoop(), make(chan struct{})
	sup.loop = loop
	go loop.serve(quit)
	t.Cleanup(func() { close(quit) })
	waitFor(t, loop.isRunning)
	pin(t, &supervisorCDP, func(_ *cdpSupervisor, method string, params any, _ string, _ float64) (*pyjson.Object, error) {
		return fn(method, params)
	})
	return sup
}

func decodeObj(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	return decodeResult(t, text)
}

func TestEvaluateRuntimeResponseShapingPrimitiveValue(t *testing.T) {
	reply := decodeObj(t, `{"id": 1, "result": {"result": {"type": "number", "value": 42}}}`)
	sup := evalSupervisor(t, "test-session-id", func(string, any) (*pyjson.Object, error) { return reply, nil })
	if got, _ := pyjson.Dumps(sup.evaluateRuntime("1 + 41", true, true, 10)); got != `{"ok": true, "result": 42, "result_type": "number"}` {
		t.Fatalf("got %s", got)
	}
}

func TestEvaluateRuntimeResponseShapingObjectValueReturnedByValue(t *testing.T) {
	reply := decodeObj(t, `{"id": 1, "result": {"result": {"type": "object", "value": {"foo": "bar", "n": 7}}}}`)
	sup := evalSupervisor(t, "test-session-id", func(string, any) (*pyjson.Object, error) { return reply, nil })
	out := sup.evaluateRuntime(`({foo:"bar", n:7})`, true, true, 10)
	if got, _ := pyjson.Dumps(anyGet(out, "result")); anyGet(out, "ok") != true || got != `{"foo": "bar", "n": 7}` || anyGet(out, "result_type") != "object" {
		t.Fatalf("got %v", out)
	}
}

func TestEvaluateRuntimeResponseShapingNoSessionAttachedReturnsError(t *testing.T) {
	sup := evalSupervisor(t, "", func(string, any) (*pyjson.Object, error) { return nil, nil })
	out := sup.evaluateRuntime("1+1", true, true, 10)
	if anyGet(out, "ok") != false || !strings.Contains(strings.ToLower(objStr(out, "error")), "session") {
		t.Fatalf("got %v", out)
	}
}

func TestEvaluateRuntimeDomNodeCrashRetryReferenceChainCrashRetriesWithoutByValue(t *testing.T) {
	var mu sync.Mutex
	var calls []any
	reply := decodeObj(t, `{"id": 8, "result": {"result": {"type": "object", "subtype": "node", "description": "body"}}}`)
	sup := evalSupervisor(t, "test-session-id", func(_ string, params any) (*pyjson.Object, error) {
		byValue := anyGet(params, "returnByValue")
		mu.Lock()
		calls = append(calls, byValue)
		mu.Unlock()
		if byValue == true {
			return nil, runtimeError("CDP error on id=7: {'code': -32000, 'message': 'Object reference chain is too long'}")
		}
		return reply, nil
	})
	out := sup.evaluateRuntime("document.body", true, true, 10)
	if anyGet(out, "ok") != true || anyGet(out, "result") != "body" || anyGet(out, "result_type") != "object" ||
		len(calls) != 2 || calls[0] != true || calls[1] != false {
		t.Fatalf("got %v calls %v", out, calls)
	}
}

func TestEvaluateRuntimeDomNodeCrashRetryUnrelatedErrorDoesNotRetry(t *testing.T) {
	var calls []any
	sup := evalSupervisor(t, "test-session-id", func(_ string, params any) (*pyjson.Object, error) {
		calls = append(calls, anyGet(params, "returnByValue"))
		return nil, runtimeError("CDP error on id=3: {'message': 'Target closed'}")
	})
	out := sup.evaluateRuntime("document.body", true, true, 10)
	if anyGet(out, "ok") != false || !strings.Contains(objStr(out, "error"), "Target closed") || len(calls) != 1 || calls[0] != true {
		t.Fatalf("got %v calls %v", out, calls)
	}
}

// Headless Chromium with a DevTools endpoint on an ephemeral port, its browser websocket url returned
func chromeCDP(t *testing.T) string {
	t.Helper()
	var bin string
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Fatal("no Chrome binary found")
	}
	profile := t.TempDir()
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "--remote-debugging-port=0", "--user-data-dir="+profile, "--no-first-run",
		"--no-default-browser-check", "--headless=new", "--disable-gpu", "--site-per-process")
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			port := strings.SplitN(string(raw), "\n", 2)[0]
			if resp, err := client.Get("http://127.0.0.1:" + port + "/json/version"); err == nil {
				var info struct {
					WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
				}
				err = json.NewDecoder(resp.Body).Decode(&info)
				resp.Body.Close()
				if err == nil && info.WebSocketDebuggerURL != "" {
					return info.WebSocketDebuggerURL
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Chrome didn't expose CDP in time: %s", stderr.String())
	return ""
}

const supervisorTestPage = `<!doctype html>
<html><head><title>Supervisor pytest</title></head><body>
<h1>Supervisor pytest</h1>
<iframe id="inner" srcdoc="<body><h2>frame-marker</h2></body>" width="400" height="100"></iframe>
</body></html>`

// Navigates the first page target to the test page and fires expression in it
func fireOnPage(t *testing.T, cdpURL, expression string) {
	t.Helper()
	conn, err := DialCDP(t.Context(), cdpURL, supervisorMaxMessage)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	infos, err := conn.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pageID string
	for _, ti := range infos {
		if ti.Type == "page" {
			pageID = ti.TargetID
			break
		}
	}
	sid, err := conn.Attach(ctx, pageID)
	if err != nil {
		t.Fatal(err)
	}
	pageURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(supervisorTestPage))
	if _, err := conn.Call(ctx, "Page.navigate", map[string]any{"url": pageURL}, sid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := conn.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, sid); err != nil {
		t.Fatal(err)
	}
}

func chromeSupervisor(t *testing.T, taskID string) (*cdpSupervisor, string) {
	t.Helper()
	cdpURL := chromeCDP(t)
	t.Cleanup(supervisors.stopAll)
	sup, err := supervisors.getOrStart(testEnv(t), taskID, cdpURL, DefaultDialogPolicy, DefaultDialogTimeoutS, supervisorStartTimeoutS)
	if err != nil {
		t.Fatal(err)
	}
	return sup, cdpURL
}

func waitForDialog(sup *cdpSupervisor) []PendingDialog {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := sup.snapshot(); len(snap.PendingDialogs) > 0 {
			return snap.PendingDialogs
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func TestSupervisorStartAndSnapshot(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-1")
	fireOnPage(t, cdpURL, "/* no dialog */ void 0")
	time.Sleep(time.Second)
	snap := sup.snapshot()
	if !snap.Active || snap.TaskID != "pytest-1" || len(snap.PendingDialogs) != 0 || anyGet(snap.FrameTree, "top") == nil {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestMainFrameAlertDetectionAndDismiss(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-2")
	fireOnPage(t, cdpURL, "setTimeout(() => alert('PYTEST-MAIN-ALERT'), 50)")
	dialogs := waitForDialog(sup)
	if len(dialogs) == 0 || dialogs[0].Type != "alert" || !strings.Contains(dialogs[0].Message, "PYTEST-MAIN-ALERT") {
		t.Fatalf("dialogs %+v", dialogs)
	}
	if res := sup.respondToDialog("dismiss", nil, "", 10); anyGet(res, "ok") != true {
		t.Fatalf("respond %v", res)
	}
	time.Sleep(300 * time.Millisecond)
	if len(sup.snapshot().PendingDialogs) != 0 {
		t.Fatal("dialog still pending")
	}
}

func TestIframeContentwindowAlert(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-3")
	fireOnPage(t, cdpURL, "setTimeout(() => document.querySelector('#inner').contentWindow.alert('PYTEST-IFRAME'), 50)")
	dialogs := waitForDialog(sup)
	found := false
	for _, d := range dialogs {
		found = found || strings.Contains(d.Message, "PYTEST-IFRAME")
	}
	if !found {
		t.Fatalf("dialogs %+v", dialogs)
	}
	if res := sup.respondToDialog("accept", nil, "", 10); anyGet(res, "ok") != true {
		t.Fatalf("respond %v", res)
	}
}

func TestPromptDialogWithResponseText(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-4")
	fireOnPage(t, cdpURL, "setTimeout(() => { window.__promptResult = prompt('give me a token', 'default-x'); }, 50)")
	dialogs := waitForDialog(sup)
	if len(dialogs) == 0 || dialogs[0].Type != "prompt" || dialogs[0].DefaultPrompt != "default-x" {
		t.Fatalf("dialogs %+v", dialogs)
	}
	reply := "PYTEST-PROMPT-REPLY"
	if res := sup.respondToDialog("accept", &reply, "", 10); anyGet(res, "ok") != true {
		t.Fatalf("respond %v", res)
	}
}

func TestBrowserDialogToolEndToEnd(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-tool")
	fireOnPage(t, cdpURL, "setTimeout(() => alert('PYTEST-TOOL-END2END'), 50)")
	if len(waitForDialog(sup)) == 0 {
		t.Fatal("no dialog detected via wait_for_dialog")
	}
	r := decodeResult(t, browserDialog("dismiss", nil, nil, "pytest-tool"))
	if anyGet(r, "success") != true || anyGet(r, "action") != "dismiss" || !strings.Contains(objStr(anyGet(r, "dialog"), "message"), "PYTEST-TOOL-END2END") {
		t.Fatalf("got %v", r)
	}
}

func TestEvaluateRuntimeUnserializableValue(t *testing.T) {
	sup, cdpURL := chromeSupervisor(t, "pytest-eval-5")
	fireOnPage(t, cdpURL, "void 0")
	time.Sleep(500 * time.Millisecond)
	if out := sup.evaluateRuntime("Infinity", true, true, 10); anyGet(out, "ok") != true || anyGet(out, "result") != "Infinity" {
		t.Fatalf("got %v", out)
	}
}

func TestBrowserSupervisorRedactionPendingAndRecentDialogMessagesRedacted(t *testing.T) {
	fakeKey := "sk-" + "SUPERVISORDIALOGSECRET1234567890"
	snap := SupervisorSnapshot{
		PendingDialogs: []PendingDialog{{ID: "d1", Type: "prompt", Message: "Enter API key " + fakeKey, DefaultPrompt: fakeKey,
			OpenedAt: 1.0, CDPSessionID: "session-1"}},
		RecentDialogs: []DialogRecord{{ID: "d2", Type: "alert", Message: "Recent key " + fakeKey, OpenedAt: 1.0, ClosedAt: 2.0, ClosedBy: "agent"}},
		FrameTree:     obj("top", obj("frame_id", "f1", "url", "about:blank", "origin", "null", "is_oopif", false)),
		Active:        true, CDPURL: "ws://example.invalid/devtools/browser/mock", TaskID: "test",
	}
	res := snap.ToDict()
	pending := anyGet(res, "pending_dialogs").([]any)[0]
	recent := anyGet(res, "recent_dialogs").([]any)[0]
	if strings.Contains(pyrepr.Repr(res), "SUPERVISORDIALOGSECRET") || !strings.HasPrefix(objStr(pending, "message"), "Enter API key sk-") ||
		!strings.HasPrefix(objStr(pending, "default_prompt"), "sk-") || !strings.HasPrefix(objStr(recent, "message"), "Recent key sk-") {
		t.Fatalf("got %s", pyrepr.Repr(res))
	}
}
