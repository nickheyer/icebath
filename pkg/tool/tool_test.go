package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func echo() Func {
	return Func{Name: "echo", Description: "Echoes text", Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`), Hints: Annotations{ReadOnly: true}, Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
		var in struct{ Text string }
		if err := json.Unmarshal(args, &in); err != nil {
			return Result{}, err
		}
		return Text(in.Text), nil
	}}
}

func TestRegistryAddAndDefinitions(t *testing.T) {
	var r Registry
	if err := r.Add(echo(), Func{Name: "b", Description: "B", Parameters: json.RawMessage(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (Result, error) { return Text("b"), nil }}); err != nil {
		t.Fatal(err)
	}
	defs := r.Definitions()
	if len(defs) != 2 || defs[0].Name != "echo" || defs[1].Name != "b" || defs[0].Description != "Echoes text" {
		t.Fatalf("%+v", defs)
	}
	if _, ok := r.Lookup("echo"); !ok {
		t.Fatal("lookup")
	}
	if names := r.Names(); len(names) != 2 || names[0] != "echo" || r.Len() != 2 {
		t.Fatalf("%v", names)
	}
	if ro := r.ReadOnly(); len(ro) != 1 || ro[0] != "echo" {
		t.Fatalf("%v", ro)
	}
	for name, bad := range map[string]Func{
		"dup":        echo(),
		"bad name":   {Name: "has space", Parameters: json.RawMessage(`{}`)},
		"long name":  {Name: strings.Repeat("a", 65), Parameters: json.RawMessage(`{}`)},
		"no schema":  {Name: "ok"},
		"bad schema": {Name: "ok", Parameters: json.RawMessage(`{`)},
	} {
		if err := r.Add(bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if !r.Remove("echo") || r.Remove("echo") || r.Len() != 1 || r.Names()[0] != "b" {
		t.Fatal("remove")
	}
	if !ValidName("ok_name-1") || ValidName("no spaces") {
		t.Fatal("valid name")
	}
}

func TestRenameAndDescribe(t *testing.T) {
	renamed := Rename(echo(), "say")
	if renamed.Definition().Name != "say" || renamed.Definition().Description != "Echoes text" || !Describe(renamed).ReadOnly {
		t.Fatalf("%+v", renamed.Definition())
	}
	plain := Func{Name: "p", Parameters: json.RawMessage(`{}`)}
	if Describe(plain) != (Annotations{}) {
		t.Fatal("plain func must have every hint off")
	}
	res, err := renamed.Run(context.Background(), json.RawMessage(`{"text":"hi"}`))
	if err != nil || res.Content[0].(protocol.Text).Text != "hi" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestResultBuilders(t *testing.T) {
	if r := Textf("%d", 7); r.IsError || r.Content[0].(protocol.Text).Text != "7" {
		t.Fatalf("%+v", r)
	}
	if r := Errorf("bad %s", "x"); !r.IsError || r.Content[0].(protocol.Text).Text != "bad x" {
		t.Fatalf("%+v", r)
	}
	if r := Image("image/png", []byte{1}); r.IsError || r.Content[0].(protocol.Image).MediaType != "image/png" {
		t.Fatalf("%+v", r)
	}
}

func TestRunMapsEveryFailureToAnErrorResult(t *testing.T) {
	var r Registry
	boom := Func{Name: "boom", Parameters: json.RawMessage(`{}`), Fn: func(context.Context, json.RawMessage) (Result, error) { return Result{}, errors.New("kaboom") }}
	panics := Func{Name: "panics", Parameters: json.RawMessage(`{}`), Fn: func(context.Context, json.RawMessage) (Result, error) { panic("nope") }}
	fails := Func{Name: "fails", Parameters: json.RawMessage(`{}`), Fn: func(context.Context, json.RawMessage) (Result, error) { return Error("soft"), nil }}
	if err := r.Add(echo(), boom, panics, fails); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ok := r.Run(ctx, protocol.ToolCall{ID: "1", Name: "echo", Args: json.RawMessage(`{"text":"hi"}`)})
	if ok.IsError || ok.CallID != "1" || ok.Name != "echo" || ok.Content[0].(protocol.Text).Text != "hi" {
		t.Fatalf("%+v", ok)
	}
	for name, want := range map[string]string{"boom": "kaboom", "panics": "panicked", "fails": "soft", "missing": "unknown tool"} {
		res := r.Run(ctx, protocol.ToolCall{ID: name, Name: name, Args: json.RawMessage(`{}`)})
		if !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, want) {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	bad := r.Run(ctx, protocol.ToolCall{ID: "2", Name: "echo", Args: json.RawMessage(`{"text":1}`)})
	if !bad.IsError {
		t.Fatal("decode failure must be an error result")
	}
}

// Tool that records the peak number of simultaneous runs
func gauge(name string, hints Annotations, running, peak *atomic.Int32) Func {
	return Func{Name: name, Parameters: json.RawMessage(`{}`), Hints: hints, Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return Text(string(args)), nil
	}}
}

func TestRunAllRunsReadersTogetherAndWritersAlone(t *testing.T) {
	var r Registry
	var running, peak atomic.Int32
	if err := r.Add(gauge("read", Annotations{ReadOnly: true}, &running, &peak), gauge("write", Annotations{}, &running, &peak)); err != nil {
		t.Fatal(err)
	}
	calls := []protocol.ToolCall{
		{ID: "a", Name: "read", Args: json.RawMessage(`{"n":1}`)},
		{ID: "b", Name: "read", Args: json.RawMessage(`{"n":2}`)},
		{ID: "c", Name: "read", Args: json.RawMessage(`{"n":3}`)},
	}
	results := r.RunAll(context.Background(), calls)
	for i, res := range results {
		if res.CallID != calls[i].ID || res.Content[0].(protocol.Text).Text != string(calls[i].Args) {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	if peak.Load() < 2 {
		t.Fatalf("readers peak concurrency %d", peak.Load())
	}
	peak.Store(0)
	mixed := []protocol.ToolCall{
		{ID: "a", Name: "read", Args: json.RawMessage(`{}`)},
		{ID: "b", Name: "write", Args: json.RawMessage(`{}`)},
		{ID: "c", Name: "write", Args: json.RawMessage(`{}`)},
		{ID: "d", Name: "missing", Args: json.RawMessage(`{}`)},
	}
	results = r.RunAll(context.Background(), mixed)
	if peak.Load() != 1 {
		t.Fatalf("writers must run alone, peak %d", peak.Load())
	}
	if len(results) != 4 || results[1].IsError || !results[3].IsError || results[3].CallID != "d" {
		t.Fatalf("%+v", results)
	}
}

func TestRunAllSharesIdempotentRepeats(t *testing.T) {
	var r Registry
	var reads, writes, edits atomic.Int32
	count := func(name string, hints Annotations, n *atomic.Int32) Func {
		return Func{Name: name, Parameters: json.RawMessage(`{}`), Hints: hints, Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
			return Textf("%s %d", name, n.Add(1)), nil
		}}
	}
	if err := r.Add(count("read", Annotations{ReadOnly: true, Idempotent: true}, &reads), count("write", Annotations{Destructive: true, Idempotent: true}, &writes), count("edit", Annotations{Destructive: true}, &edits)); err != nil {
		t.Fatal(err)
	}
	calls := []protocol.ToolCall{
		{ID: "a", Name: "read", Args: json.RawMessage(`{"p":1}`)},
		{ID: "b", Name: "write", Args: json.RawMessage(`{"p":1}`)},
		{ID: "c", Name: "read", Args: json.RawMessage(`{"p":1}`)},
		{ID: "d", Name: "write", Args: json.RawMessage(`{"p":1}`)},
		{ID: "e", Name: "write", Args: json.RawMessage(`{"p":2}`)},
		{ID: "f", Name: "edit", Args: json.RawMessage(`{"p":1}`)},
		{ID: "g", Name: "edit", Args: json.RawMessage(`{"p":1}`)},
	}
	results := r.RunAll(context.Background(), calls)
	if reads.Load() != 1 || writes.Load() != 2 || edits.Load() != 2 {
		t.Fatalf("reads %d writes %d edits %d", reads.Load(), writes.Load(), edits.Load())
	}
	text := func(i int) string { return results[i].Content[0].(protocol.Text).Text }
	for i, c := range calls {
		if results[i].CallID != c.ID || results[i].Name != c.Name {
			t.Fatalf("%d: %+v", i, results[i])
		}
	}
	if text(0) != text(2) || text(1) != text(3) || text(3) == text(4) || text(5) == text(6) {
		t.Fatalf("%+v", results)
	}
}

func TestAddIsAtomicAndValidatesSchemas(t *testing.T) {
	var r Registry
	good := echo()
	bad := Func{Name: "bad", Parameters: json.RawMessage(`{"type":"array"}`)}
	if err := r.Add(good, bad); err == nil {
		t.Fatal("array schema accepted")
	}
	if r.Len() != 0 {
		t.Fatal("a failed add must register nothing")
	}
	if err := r.Add(echo(), echo()); err == nil || r.Len() != 0 {
		t.Fatal("duplicates within one add must be rejected without registering")
	}
	if ValidSchema(json.RawMessage(`{"properties":{}}`)) != nil || ValidSchema(json.RawMessage(`[]`)) == nil || ValidSchema(json.RawMessage(`null`)) == nil {
		t.Fatal("schema validation")
	}
	changes := 0
	r.OnChange(func() { changes++ })
	if err := r.Add(echo()); err != nil {
		t.Fatal(err)
	}
	r.Remove("echo")
	r.Remove("echo")
	if changes != 2 {
		t.Fatalf("%d change notifications", changes)
	}
}

func TestRunCarriesDataTimingAndProgress(t *testing.T) {
	var r Registry
	var reports []Progress
	slow := Func{Name: "slow", Parameters: json.RawMessage(`{}`), Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
		Report(ctx, Progress{Stream: "stdout", Text: "half"})
		if call, ok := CallFrom(ctx); !ok || call.Name != "slow" {
			return Error("no call in context"), nil
		}
		select {
		case <-time.After(30 * time.Millisecond):
			return Text("done").With(map[string]int{"n": 1}), nil
		case <-ctx.Done():
			return Error(ctx.Err().Error()), nil
		}
	}}
	if err := r.Add(slow); err != nil {
		t.Fatal(err)
	}
	ctx := WithReporter(context.Background(), func(p Progress) { reports = append(reports, p) })
	out := r.Run(ctx, protocol.ToolCall{ID: "c1", Name: "slow", Args: json.RawMessage(`{}`)})
	if out.IsError || out.Data.(map[string]int)["n"] != 1 || out.Duration < 30*time.Millisecond || out.Started.IsZero() {
		t.Fatalf("%+v", out)
	}
	if len(reports) != 1 || reports[0].CallID != "c1" || reports[0].Tool != "slow" || reports[0].Text != "half" {
		t.Fatalf("%+v", reports)
	}
	r.Timeout = 5 * time.Millisecond
	if out := r.Run(context.Background(), protocol.ToolCall{ID: "c2", Name: "slow"}); !out.IsError || !strings.Contains(out.Content[0].(protocol.Text).Text, "deadline") {
		t.Fatalf("timeout %+v", out)
	}
	r.Timeout = 0
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	outs := r.RunAll(cancelled, []protocol.ToolCall{{ID: "x", Name: "slow"}})
	if !outs[0].IsError || !strings.Contains(outs[0].Content[0].(protocol.Text).Text, "cancelled") {
		t.Fatalf("%+v", outs[0])
	}
	Report(context.Background(), Progress{Text: "nowhere"})
}

func TestRunEachReportsAsCallsFinish(t *testing.T) {
	var r Registry
	slow := Func{Name: "slow", Parameters: json.RawMessage(`{}`), Hints: Annotations{ReadOnly: true}, Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
		time.Sleep(40 * time.Millisecond)
		return Text("slow"), nil
	}}
	fast := Func{Name: "fast", Parameters: json.RawMessage(`{}`), Hints: Annotations{ReadOnly: true, Idempotent: true}, Fn: func(ctx context.Context, args json.RawMessage) (Result, error) {
		return Text("fast"), nil
	}}
	if err := r.Add(slow, fast); err != nil {
		t.Fatal(err)
	}
	var order []int
	outs := r.RunEach(context.Background(), []protocol.ToolCall{{ID: "a", Name: "slow"}, {ID: "b", Name: "fast"}, {ID: "c", Name: "fast"}}, func(i int, out Outcome) {
		order = append(order, i)
	})
	if len(order) != 3 || order[0] != 1 || order[1] != 0 || order[2] != 2 {
		t.Fatalf("completion order %v", order)
	}
	if outs[2].CallID != "c" || outs[2].Content[0].(protocol.Text).Text != "fast" {
		t.Fatalf("%+v", outs[2])
	}
}

func TestToolsTurnOffAndOn(t *testing.T) {
	var r Registry
	changed := 0
	r.OnChange(func() { changed++ })
	r.Add(Func{Name: "a", Parameters: []byte(`{"type":"object"}`), Fn: func(context.Context, json.RawMessage) (Result, error) { return Text("ran"), nil }})
	r.SetEnabled("a", false)
	r.SetEnabled("later", false)
	if r.Enabled("a") || !r.Enabled("b") || len(r.Definitions()) != 0 || len(r.Names()) != 1 || changed != 3 {
		t.Fatalf("an off tool leaves the definitions but stays registered %v %d", r.Names(), changed)
	}
	if got := r.Disabled(); len(got) != 2 || got[0] != "a" || got[1] != "later" {
		t.Fatalf("off names sorted, registered or not %v", got)
	}
	out := r.Run(context.Background(), protocol.ToolCall{ID: "1", Name: "a"})
	if !out.IsError || !strings.Contains(protocol.Response{Content: out.Content}.Text(), "turned off") {
		t.Fatalf("a call to an off tool is refused %+v", out)
	}
	r.SetEnabled("a", true)
	if out := r.Run(context.Background(), protocol.ToolCall{ID: "2", Name: "a"}); out.IsError || len(r.Definitions()) != 1 {
		t.Fatal("a tool turned back on runs")
	}
}
