package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Identifier of the running goroutine as the stack header prints it
func auxGoroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	return string(bytes.Fields(buf)[1])
}

type auxTestEvent struct{ set atomic.Bool }

func (e *auxTestEvent) IsSet() bool { return e.set.Load() }

func TestAuxInterruptProtectionIsReentrant(t *testing.T) {
	base := context.Background()
	outer := AuxInterruptProtection(base, true, nil, nil)
	if !auxInterruptProtected(outer) {
		t.Fatal("outer scope must be protected")
	}
	inner := AuxInterruptProtection(outer, true, nil, nil)
	if !auxInterruptProtected(inner) {
		t.Fatal("inner scope must be protected")
	}
	if !auxInterruptProtected(outer) {
		t.Fatal("inner exit must not clear the outer protection")
	}
	if auxInterruptProtected(base) {
		t.Fatal("outside every scope nothing is protected")
	}
}

func TestAuxInterruptProtectionRestoresOnError(t *testing.T) {
	base := context.Background()
	run := func(ctx context.Context) error { return errors.New("boom") }
	if err := run(AuxInterruptProtection(base, true, nil, nil)); err == nil {
		t.Fatal("expected the error")
	}
	if auxInterruptProtected(base) {
		t.Fatal("an error inside the scope must leave the caller unprotected")
	}
}

func TestNestedScopesInheritTheCancelSourcesAndPreferTheEvent(t *testing.T) {
	ev := &auxTestEvent{}
	checked := 0
	check := AuxCancelFunc(func() (bool, error) { checked++; return false, nil })
	ctx := AuxInterruptProtection(context.Background(), true, check, nil)
	if auxInterruptCancelRequested(ctx) || checked != 1 {
		t.Fatalf("the check is the only source, checked %d", checked)
	}
	ctx = AuxInterruptProtection(ctx, true, nil, ev)
	inner := AuxInterruptProtection(ctx, false, nil, nil)
	ev.set.Store(true)
	if !auxInterruptCancelRequested(inner) || checked != 1 {
		t.Fatalf("nested scope must inherit the event and prefer it over the check, checked %d", checked)
	}
	failing := AuxInterruptProtection(context.Background(), true, AuxCancelFunc(func() (bool, error) { return true, errors.New("dead") }), nil)
	if auxInterruptCancelRequested(failing) {
		t.Fatal("a failing source reads as not cancelled")
	}
}

func TestCancellationDecisionLinearizesCancelAgainstTimeout(t *testing.T) {
	var cancelled atomic.Bool
	source := AuxCancelFunc(func() (bool, error) { return cancelled.Load(), nil })
	timedOut := newAuxCancellationDecision(source)
	if !timedOut.BeginTimeoutCleanup() {
		t.Fatal("timeout wins while no cancel is pending")
	}
	cancelled.Store(true)
	if got, _ := timedOut.CancelRequested(); got {
		t.Fatal("a timed out attempt never turns into a cancellation")
	}
	won := newAuxCancellationDecision(source)
	if got, _ := won.CancelRequested(); !got {
		t.Fatal("cancel freezes the attempt")
	}
	cancelled.Store(false)
	if got, _ := won.CancelRequested(); !got || won.BeginTimeoutCleanup() {
		t.Fatal("a cancelled attempt stays cancelled and refuses timeout cleanup")
	}
}

func TestAuxProgressHookInstalledAndRestored(t *testing.T) {
	ticks := 0
	base := context.Background()
	auxNotifyAuxProgress(AuxProgressHook(base, func() error { ticks++; return nil }))
	auxNotifyAuxProgress(base)
	if ticks != 1 {
		t.Fatalf("ticks %d, want 1", ticks)
	}
	if auxProgressActive(base) || !auxProgressActive(AuxProgressHook(base, func() error { return nil })) {
		t.Fatal("progress is active only under an installed hook")
	}
	outer := AuxProgressHook(base, func() error { ticks += 10; return nil })
	auxNotifyAuxProgress(AuxProgressHook(outer, nil))
	if ticks != 11 {
		t.Fatalf("a nil hook passes the previous one through, ticks %d", ticks)
	}
	auxNotifyAuxProgress(AuxProgressHook(base, func() error { return errors.New("hook down") }))
}

func TestProviderResponseTicksTimingThenProgress(t *testing.T) {
	var order []string
	ctx := AuxThreadLocalHook(AuxProgressHook(context.Background(), func() error { order = append(order, "progress"); return nil }),
		AuxProviderResponseLocal, func() error { order = append(order, "response"); return nil })
	ctx = AuxThreadLocalHook(ctx, AuxDispatchLocal, func() error { order = append(order, "dispatch"); return nil })
	auxNotifyAuxDispatch(ctx)
	auxNotifyAuxProviderResponse(ctx)
	auxNotifyAuxTimingResponse(ctx)
	if !reflect.DeepEqual(order, []string{"dispatch", "response", "progress", "response"}) {
		t.Fatalf("hook order %v", order)
	}
}

func TestDeadlineScopeRestoresThePreviousValue(t *testing.T) {
	base := context.Background()
	outer := time.Now().Add(900 * time.Second)
	outerCtx := AuxStreamDeadline(base, AuxStreamDeadlineAt(outer))
	if got, ok := auxCurrentAuxStreamDeadline(outerCtx); !ok || !got.Equal(outer) {
		t.Fatalf("outer deadline %v %v", got, ok)
	}
	innerCtx := AuxStreamDeadline(outerCtx, AuxStreamDeadlineAt(time.Now().Add(10*time.Second)))
	if got, _ := auxCurrentAuxStreamDeadline(innerCtx); got.Equal(outer) {
		t.Fatal("inner deadline must replace the outer one")
	}
	if got, _ := auxCurrentAuxStreamDeadline(AuxStreamDeadline(outerCtx, nil)); !got.Equal(outer) {
		t.Fatal("nil is a passthrough")
	}
	if got, _ := auxCurrentAuxStreamDeadline(outerCtx); !got.Equal(outer) {
		t.Fatal("outer deadline restored")
	}
	if _, ok := auxCurrentAuxStreamDeadline(base); ok {
		t.Fatal("no deadline outside the scope")
	}
}

func TestProtectedProviderDaemonInheritsTheHostDeadline(t *testing.T) {
	var seenDeadline time.Time
	var seenGoroutine string
	callback := func(ctx context.Context, _ map[string]any) (any, error) {
		seenDeadline, _ = auxCurrentAuxStreamDeadline(ctx)
		seenGoroutine = auxGoroutineID()
		return "ok", nil
	}
	deadline := time.Now().Add(42 * time.Second)
	ctx := AuxProgressHook(context.Background(), func() error { return nil })
	ctx = AuxInterruptProtection(ctx, true, nil, &auxTestEvent{})
	ctx = AuxStreamDeadline(ctx, AuxStreamDeadlineAt(deadline))
	got, err := auxRunProtectedSyncProviderCall(ctx, callback, map[string]any{})
	if err != nil || got != "ok" {
		t.Fatalf("result %v %v", got, err)
	}
	if seenGoroutine == auxGoroutineID() {
		t.Fatal("protected callback must run on its own goroutine")
	}
	if !seenDeadline.Equal(deadline) {
		t.Fatalf("worker deadline %v, want %v", seenDeadline, deadline)
	}
}

func TestProtectedSilentProviderRaisesFrozenExplicitCancel(t *testing.T) {
	ev := &auxTestEvent{}
	started := make(chan struct{})
	release := make(chan struct{})
	var workerSawCancel atomic.Bool
	callback := func(ctx context.Context, _ map[string]any) (any, error) {
		close(started)
		<-release
		workerSawCancel.Store(auxInterruptCancelRequested(ctx))
		return "late", nil
	}
	ctx := AuxInterruptProtection(context.Background(), true, nil, ev)
	go func() {
		<-started
		ev.set.Store(true)
	}()
	begin := time.Now()
	_, err := auxRunProtectedSyncProviderCall(ctx, callback, map[string]any{})
	elapsed := time.Since(begin)
	var cancel *AuxiliaryExplicitCancellation
	if !errors.As(err, &cancel) || cancel.Cause() != "explicit_host_cancel" || !auxIsCancellation(err) {
		t.Fatalf("want explicit cancellation, got %v", err)
	}
	if elapsed >= 750*time.Millisecond {
		t.Fatalf("owner must unwind at once, took %v", elapsed)
	}
	ev.set.Store(false)
	close(release)
	time.Sleep(50 * time.Millisecond)
	if !workerSawCancel.Load() {
		t.Fatal("the worker reads the frozen decision, not the cleared host event")
	}
	unprotected, err := auxRunProtectedSyncProviderCall(context.Background(), func(context.Context, map[string]any) (any, error) {
		return auxGoroutineID(), nil
	}, nil)
	if err != nil || unprotected != auxGoroutineID() {
		t.Fatal("an unprotected call runs directly on the caller")
	}
}

func TestAnthropicRequiresNonemptyDeltaPayload(t *testing.T) {
	cases := []struct {
		delta map[string]any
		want  bool
	}{
		{map[string]any{"type": "text_delta", "text": "", "thinking": nil}, false},
		{map[string]any{"type": "text_delta", "text": "token", "thinking": nil}, true},
		{map[string]any{"type": "thinking_delta", "text": nil, "thinking": "thought"}, true},
		{map[string]any{"type": "input_json_delta", "partial_json": ""}, false},
		{map[string]any{"type": "input_json_delta", "partial_json": `{"x"`}, true},
		{map[string]any{"type": "signature_delta", "signature": "sig-1"}, true},
		{map[string]any{"type": "signature_delta", "signature": ""}, false},
		{map[string]any{"type": "citations_delta", "citation": map[string]any{"cited": 1}}, true},
	}
	for _, c := range cases {
		if got := auxAnthropicEventHasContent(map[string]any{"type": "content_block_delta", "delta": c.delta}); got != c.want {
			t.Errorf("delta %v content %v, want %v", c.delta, got, c.want)
		}
	}
}

func TestAnthropicToolStartRequiresIdentity(t *testing.T) {
	cases := []struct {
		block map[string]any
		want  bool
	}{
		{map[string]any{"type": "text"}, false},
		{map[string]any{"type": "tool_use", "id": nil, "name": nil}, false},
		{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup"}, true},
	}
	for _, c := range cases {
		if got := auxAnthropicEventHasContent(map[string]any{"type": "content_block_start", "content_block": c.block}); got != c.want {
			t.Errorf("block %v content %v, want %v", c.block, got, c.want)
		}
	}
}

func TestCodexContentBearingEvents(t *testing.T) {
	empty := []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_item.added", "item": map[string]any{"type": "message"}},
		{"type": "response.content_part.added", "part": map[string]any{"type": "output_text"}},
		{"type": "response.output_text.delta", "delta": ""},
		{"type": "response.reasoning_summary_text.delta", "delta": ""},
		{"type": "response.function_call_arguments.delta", "delta": ""},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call"}},
	}
	for _, e := range empty {
		if codexEventHasContent(e) {
			t.Errorf("structural event %v is not progress", e)
		}
	}
	full := []map[string]any{
		{"type": "response.output_text.delta", "delta": "token"},
		{"type": "response.reasoning_summary_text.delta", "delta": "thought"},
		{"type": "response.function_call_arguments.delta", "delta": `{"x"`},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item_1", "call_id": "call_1", "name": "lookup"}},
	}
	for _, e := range full {
		if !codexEventHasContent(e) {
			t.Errorf("event %v is progress", e)
		}
	}
}

func TestAnthropicStreamHookStopsAtHostDeadlineAndCancel(t *testing.T) {
	var progress, timing int
	ctx := AuxThreadLocalHook(AuxProgressHook(context.Background(), func() error { progress++; return nil }),
		AuxProviderResponseLocal, func() error { timing++; return nil })
	hook := auxAnthropicAuxStreamEventHook(AuxStreamDeadline(ctx, AuxStreamDeadlineAt(time.Now().Add(-time.Second))))
	err := hook(map[string]any{"type": "ping"})
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(timeout.Error(), "timed out at the host compression deadline after 0s") {
		t.Fatalf("want host deadline timeout, got %v", err)
	}
	if progress != 0 || timing != 1 {
		t.Fatalf("a keepalive is timing only, progress %d timing %d", progress, timing)
	}
	ev := &auxTestEvent{}
	ev.set.Store(true)
	cancelHook := auxAnthropicAuxStreamEventHook(AuxInterruptProtection(ctx, true, nil, ev))
	delta := map[string]any{"type": "content_block_delta", "delta": map[string]any{"text": "t"}}
	if err := cancelHook(delta); !auxIsCancellation(err) || progress != 1 {
		t.Fatalf("content ticks progress then the cancel wins, got %v progress %d", err, progress)
	}
	if err := auxAnthropicAuxStreamEventHook(ctx)(delta); err != nil {
		t.Fatalf("without deadline or cancel the stream runs, got %v", err)
	}
}

type auxDeclaringClient struct{ auxProbeClient }

func (auxDeclaringClient) AuxDeclares(flag string) bool { return flag == "ICEBATH_SKIP_TRANSPORT_WRAP" }

func TestClientDeclaresAndSafeIsInstance(t *testing.T) {
	if !auxClientDeclares(auxDeclaringClient{}, "ICEBATH_SKIP_TRANSPORT_WRAP") || auxClientDeclares(auxDeclaringClient{}, "OTHER") ||
		auxClientDeclares(&auxProbeClient{}, "ICEBATH_SKIP_TRANSPORT_WRAP") || auxClientDeclares(nil, "X") {
		t.Fatal("declaration is opt in per flag")
	}
	auxClientType := reflect.TypeOf((*AuxClient)(nil)).Elem()
	if !auxSafeIsInstance(&auxProbeClient{}, reflect.TypeOf(&auxProbeClient{})) || !auxSafeIsInstance(&auxProbeClient{}, nil, auxClientType) {
		t.Fatal("matching type or interface")
	}
	if auxSafeIsInstance(nil, auxClientType) || auxSafeIsInstance("x", nil) || auxSafeIsInstance(auxProbeClient{}, reflect.TypeOf(&auxProbeClient{})) {
		t.Fatal("nil values and non types match nothing")
	}
}

func TestExtractURLQueryParams(t *testing.T) {
	clean, q, err := auxExtractURLQueryParams("https://h.example/v1?api-version=2024-10&x=a+b&x=second&blank=&flag&%41=%zz")
	if err != nil || clean != "https://h.example/v1" {
		t.Fatalf("clean %q %v", clean, err)
	}
	if !reflect.DeepEqual(q, map[string]string{"api-version": "2024-10", "x": "a b", "A": "%zz"}) {
		t.Fatalf("params %v", q)
	}
	if clean, q, _ := auxExtractURLQueryParams("https://h.example/v1"); clean != "https://h.example/v1" || q != nil {
		t.Fatalf("no query keeps the URL and gives nil, got %q %v", clean, q)
	}
	if _, q, _ := auxExtractURLQueryParams("https://h.example/v1?blank="); q == nil || len(q) != 0 {
		t.Fatalf("only blank params give an empty mapping, got %v", q)
	}
	if _, _, err := auxExtractURLQueryParams("http://[::1/v1?a=b"); err == nil {
		t.Fatal("urlparse ValueError must surface")
	}
}

func TestProbeModeIsScopedReentrantAndPerCallChain(t *testing.T) {
	base := context.Background()
	if auxProbeActive(base) {
		t.Fatal("inactive outside a probe")
	}
	outer := AuxProbeMode(base)
	if !auxProbeActive(AuxProbeMode(outer)) || !auxProbeActive(outer) {
		t.Fatal("nested scopes stay active")
	}
	seen := make(chan bool)
	go func() { seen <- auxProbeActive(context.Background()) }()
	if <-seen {
		t.Fatal("another call chain never sees the probe scope")
	}
	_, err := (&auxProbeClient{}).CreateChatCompletion(outer, nil)
	if err == nil || !strings.Contains(err.Error(), "availability checks only") {
		t.Fatalf("probe client must fail loudly on runtime use, got %v", err)
	}
}

func TestResolveAuxVerifySSLVerifyFalse(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	cfg := config.Tree{"custom_providers": []any{map[string]any{"name": "ollama", "base_url": "https://ollama.example.com/v1", "ssl_verify": false}}}
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
	if got := auxResolveAuxVerify(context.Background(), "https://ollama.example.com/v1"); !got.Insecure {
		t.Fatalf("ssl_verify false must disable verification, got %+v", got)
	}
	if got := auxResolveAuxVerify(context.Background(), "https://other.example.com/v1"); got.Insecure {
		t.Fatalf("an unrelated host keeps verification, got %+v", got)
	}
}

func TestProbeModeReturnsProbeClientWithoutBuildingAClient(t *testing.T) {
	built := false
	prev := auxBuildKeepaliveHTTPClientFn
	auxBuildKeepaliveHTTPClientFn = func(string, bool, TLSVerify) *http.Client { built = true; return nil }
	t.Cleanup(func() { auxBuildKeepaliveHTTPClientFn = prev })
	client := auxCreateOpenAIClient(AuxProbeMode(context.Background()), "k", "https://x.invalid/v1", auxOpenAIOptions{})
	probe, ok := client.(*auxProbeClient)
	if !ok || probe.APIKey() != "k" || probe.BaseURL() != "https://x.invalid/v1" || built {
		t.Fatalf("probe mode must return the probe client without building a client, got %T built %v", client, built)
	}
}
