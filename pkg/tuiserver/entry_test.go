package tuiserver

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Fake collaborators of the stdio entry every case installs, stdin fed from text and stdio flagged as not the channel
func fakeEntryStartup(t *testing.T, stdin string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapVar(t, &entryInstallSidecarPublisher, func() {})
	swapVar(t, &entryEnsureMCPDiscovery, func(context.Context) {})
	swapVar(t, &entryResolveSkin, func(context.Context) any { return "default" })
	swapVar(t, &entryEnsureSkinWatcher, func(context.Context) {})
	swapVar(t, &entryHandleSpuriousEOF, func(*[]float64, func(string)) bool { return false })
	swapVar(t, &entryStdin, io.Reader(strings.NewReader(stdin)))
	wasChannel := tuigateway.StdioIsRPCChannel()
	tuigateway.SetStdioIsRPCChannel(false)
	t.Cleanup(func() { tuigateway.SetStdioIsRPCChannel(wasChannel) })
}

// Runs main of a fresh entry and returns its status
func runEntryMain() int {
	return (&stdioEntry{ctx: context.Background(), exit: func(int) {}}).main()
}

func TestEntryRefusesAnOwnedHomeAndOwnsAFreeOne(t *testing.T) {
	fakeEntryStartup(t, "")
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", home)
	var mu sync.Mutex
	var opened []string
	note := func(entry string) {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, entry)
	}
	swapVar(t, &entryStartHeartbeat, func(context.Context) { note("state.db") })
	swapVar(t, &entryScheduleOrphanSweep, func(context.Context) {})
	swapVar(t, &entryLogExit, note)
	swapVar(t, &entryWriteJSON, func(context.Context, *pyjson.Object) bool { return true })
	ctx := context.Background()
	holder := gateway.NewProfileOwnership()
	if err := holder.Reserve(ctx, []string{home}, ""); err != nil {
		t.Fatal(err)
	}
	refused := runEntryMain()
	holder.Release(home)
	if refused != 1 {
		t.Fatalf("main on an owned home = %d", refused)
	}
	if slices.Contains(opened, "state.db") || len(opened) == 0 || !strings.Contains(opened[len(opened)-1], "owned by a running gateway") {
		t.Fatalf("opened = %v", opened)
	}
	var contender []string
	swapVar(t, &entryDispatch, func(_ context.Context, req any) map[string]any {
		var conflict *gateway.OwnershipConflict
		if err := gateway.NewProfileOwnership().Reserve(ctx, []string{home}, ""); !errors.As(err, &conflict) {
			t.Errorf("a second owner reserved the served home: %v", err)
		}
		contender = append(contender, "refused")
		return map[string]any{"jsonrpc": "2.0", "id": req.(*pyjson.Object).GetOr("id", nil), "result": map[string]any{}}
	})
	swapVar(t, &entryStdin, io.Reader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")))
	runEntryMain()
	if !reflect.DeepEqual(contender, []string{"refused"}) {
		t.Fatalf("contender = %v", contender)
	}
	after := gateway.NewProfileOwnership()
	if err := after.Reserve(ctx, []string{home}, ""); err != nil {
		t.Fatalf("the home was not released at exit: %v", err)
	}
	after.Release(home)
}

func TestMainReportsInlineHandlerErrorAndKeepsReading(t *testing.T) {
	var lines []string
	for _, req := range []map[string]any{{"jsonrpc": "2.0", "id": "broken", "method": "clipboard.save"},
		{"jsonrpc": "2.0", "id": "next", "method": "ping"}} {
		text, err := pyjson.Dumps(req)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, text+"\n")
	}
	fakeEntryStartup(t, strings.Join(lines, ""))
	swapVar(t, &entryStartHeartbeat, func(context.Context) {})
	swapVar(t, &entryScheduleOrphanSweep, func(context.Context) {})
	var replies []map[string]any
	swapVar(t, &entryWriteJSON, func(_ context.Context, payload *pyjson.Object) bool {
		replies = append(replies, pyjson.Plain(payload).(map[string]any))
		return true
	})
	var breadcrumbs []string
	swapVar(t, &entryAppendCrashLog, func(header string, _ func(io.Writer)) { breadcrumbs = append(breadcrumbs, header) })
	swapVar(t, &entryDispatch, func(_ context.Context, req any) map[string]any {
		id := req.(*pyjson.Object).GetOr("id", nil)
		if id == "broken" {
			panic(errors.New("clipboard backend is unavailable"))
		}
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"ok": true}}
	})
	runEntryMain()
	if len(replies) != 3 {
		t.Fatalf("replies = %v", replies)
	}
	refusal, _ := replies[1]["error"].(map[string]any)
	if replies[1]["id"] != "broken" || refusal["code"] != int64(-32000) && refusal["code"] != -32000 {
		t.Fatalf("broken reply = %v", replies[1])
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "clipboard backend is unavailable") {
		t.Fatalf("broken reply = %v", replies[1])
	}
	want := map[string]any{"jsonrpc": "2.0", "id": "next", "result": map[string]any{"ok": true}}
	if !reflect.DeepEqual(replies[2], want) {
		t.Fatalf("next reply = %v", replies[2])
	}
	if len(breadcrumbs) != 1 || !strings.Contains(breadcrumbs[0], "clipboard.save") {
		t.Fatalf("breadcrumbs = %v", breadcrumbs)
	}
}

// Runs main with fake collaborators and records each written event type and each prewarm in call order
func runEntryMainRecording(t *testing.T, prewarm func(context.Context)) [][2]string {
	t.Helper()
	fakeEntryStartup(t, "")
	swapVar(t, &entryLogExit, func(string) {})
	var mu sync.Mutex
	var events [][2]string
	swapVar(t, &entryWriteJSON, func(_ context.Context, payload *pyjson.Object) bool {
		kind := payload.GetOr("method", nil)
		if params, isDict := payload.GetOr("params", nil).(*pyjson.Object); isDict && params.GetOr("type", nil) != nil {
			kind = params.GetOr("type", nil)
		}
		mu.Lock()
		defer mu.Unlock()
		events = append(events, [2]string{"write", kind.(string)})
		return true
	})
	if prewarm == nil {
		prewarm = func(context.Context) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, [2]string{"prewarm"})
		}
	}
	swapVar(t, &entryPrewarmPickerCache, func(ctx context.Context) {
		prewarm(ctx)
	})
	runEntryMain()
	mu.Lock()
	defer mu.Unlock()
	return append([][2]string(nil), events...)
}

func TestMainSurvivesPrewarmFailure(t *testing.T) {
	var prewarmed bool
	events := runEntryMainRecording(t, func(context.Context) {
		prewarmed = true
		panic(errors.New("provider registry exploded"))
	})
	if !prewarmed {
		t.Fatal("the prewarm never ran")
	}
	if !slices.Contains(events, [2]string{"write", "gateway.ready"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestMainMarksStdoutAsTheRpcChannel(t *testing.T) {
	runEntryMainRecording(t, nil)
	if !tuigateway.StdioIsRPCChannel() {
		t.Fatal("main left stdout unmarked as the RPC channel")
	}
}
