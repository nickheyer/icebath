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
