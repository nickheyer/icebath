package facts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/session"
)

func TestStoreLifecycleAndPersistence(t *testing.T) {
	sess := session.New()
	s := New()
	s.Attach(sess)
	var changes []Change
	s.OnChange(func(c Change) { changes = append(changes, c) })
	a, err := s.Add(Fact{Title: " Port 22 open ", Kind: "Finding", Tags: []string{"net", "NET", ""}, Confidence: 0.9, Evidence: []Evidence{{Ref: "scan.txt", Line: 3, Quote: "22/tcp open"}}, Agent: "root"})
	if err != nil || a.ID != "f1" || a.Title != "Port 22 open" || a.Kind != "finding" || a.Status != StatusOpen || len(a.Tags) != 1 || a.Evidence[0].Kind != "file" || a.Time.IsZero() {
		t.Fatalf("%v %+v", err, a)
	}
	b, err := s.Add(Fact{Title: "Weak cipher", Kind: "risk", Parent: "f1", Confidence: 0.4})
	if err != nil || b.ID != "f2" {
		t.Fatalf("%v %+v", err, b)
	}
	if _, err := s.Add(Fact{Title: "orphan", Parent: "nope"}); err == nil {
		t.Fatal("missing parent accepted")
	}
	if _, err := s.Add(Fact{Title: ""}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := s.Add(Fact{Title: "x", Confidence: 2}); err == nil {
		t.Fatal("confidence above one accepted")
	}
	if _, err := s.Add(Fact{Title: "x", Status: "weird"}); err == nil {
		t.Fatal("unknown status accepted")
	}
	if _, err := s.Add(Fact{Title: "x", Evidence: []Evidence{{Ref: " "}}}); err == nil {
		t.Fatal("empty evidence reference accepted")
	}
	if _, err := s.Add(Fact{ID: "f1", Title: "dup"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	b.Body = "TLS 1.0 enabled"
	if _, err := s.Update(b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(Fact{ID: "zzz", Title: "x"}); err == nil {
		t.Fatal("update of a missing fact accepted")
	}
	if _, err := s.SetStatus("f1", StatusResolved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStatus("none", StatusResolved); err == nil {
		t.Fatal("status of a missing fact set")
	}
	c, _ := s.Add(Fact{ID: "f9", Title: "custom id", Kind: "note"})
	if c.ID != "f9" {
		t.Fatal(c.ID)
	}
	d, _ := s.Add(Fact{Title: "after custom"})
	if d.ID != "f10" {
		t.Fatalf("ids must advance past custom ones, got %s", d.ID)
	}
	if !s.Remove("f10") || s.Remove("f10") || s.Len() != 3 {
		t.Fatal("remove")
	}
	if len(changes) != 7 || changes[0].Op != OpAdd || changes[2].Op != OpUpdate || changes[6].Op != OpRemove {
		t.Fatalf("%d changes %+v", len(changes), changes)
	}
	got, ok := s.Get("f1")
	if !ok || got.Status != StatusResolved {
		t.Fatalf("%+v", got)
	}
	got.Tags[0] = "mutated"
	if again, _ := s.Get("f1"); again.Tags[0] != "net" {
		t.Fatal("get must copy")
	}
	restored := New()
	if err := restored.Restore(sess); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 3 {
		t.Fatalf("restored %d", restored.Len())
	}
	if f, _ := restored.Get("f2"); f.Body != "TLS 1.0 enabled" || f.Parent != "f1" {
		t.Fatalf("%+v", f)
	}
	if f, _ := restored.Get("f1"); f.Status != StatusResolved {
		t.Fatalf("%+v", f)
	}
	if n, _ := restored.Add(Fact{Title: "next"}); n.ID != "f11" {
		t.Fatalf("restored counter must continue, got %s", n.ID)
	}
	corrupt := session.FromHeader(session.Header{ID: "c"}, nil, []session.Record{{Seq: 1, Kind: RecordKind, Data: json.RawMessage(`{"op":`)}})
	if err := New().Restore(corrupt); err == nil {
		t.Fatal("corrupt record accepted")
	}
}

func TestQueriesTreeAndExports(t *testing.T) {
	s := New()
	s.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	s.Add(Fact{Title: "Alpha host", Kind: "finding", Tags: []string{"host"}, Confidence: 1, Agent: "a", Evidence: []Evidence{{Kind: "url", Ref: "http://alpha"}}})
	s.Add(Fact{Title: "Beta risk", Kind: "risk", Tags: []string{"host", "tls"}, Confidence: 0.3, Agent: "b", Parent: "f1", Body: "expires soon"})
	s.Add(Fact{Title: "Gamma", Kind: "note", Confidence: 0.5, Status: StatusRetracted})
	if n := len(s.List(Query{Kind: "RISK"})); n != 1 {
		t.Fatalf("kind %d", n)
	}
	if n := len(s.List(Query{Tag: "host"})); n != 2 {
		t.Fatalf("tag %d", n)
	}
	if n := len(s.List(Query{Status: StatusRetracted})); n != 1 {
		t.Fatalf("status %d", n)
	}
	if n := len(s.List(Query{Agent: "b"})); n != 1 {
		t.Fatalf("agent %d", n)
	}
	if n := len(s.List(Query{MinConfidence: 0.5})); n != 2 {
		t.Fatalf("confidence %d", n)
	}
	if n := len(s.List(Query{Text: "ALPHA"})); n != 1 {
		t.Fatalf("text %d", n)
	}
	if n := len(s.List(Query{Text: "expires"})); n != 1 {
		t.Fatalf("body text %d", n)
	}
	if n := len(s.List(Query{Text: "tls"})); n != 1 {
		t.Fatalf("tag text %d", n)
	}
	if n := len(s.List(Query{Text: "alpha"})); n != 1 {
		t.Fatalf("evidence text %d", n)
	}
	if n := len(s.List(Query{Text: "nothing"})); n != 0 {
		t.Fatalf("no match %d", n)
	}
	if k := s.Kinds(); k["finding"] != 1 || k["risk"] != 1 || k["note"] != 1 {
		t.Fatalf("%v", k)
	}
	if tg := s.Tags(); tg["host"] != 2 || tg["tls"] != 1 {
		t.Fatalf("%v", tg)
	}
	tree := s.Tree()
	if len(tree) != 2 || tree[0].Fact.ID != "f1" || len(tree[0].Children) != 1 || tree[0].Children[0].Fact.ID != "f2" || tree[1].Fact.ID != "f3" {
		t.Fatalf("%+v", tree)
	}
	groups := s.ByKind()
	if len(groups) != 3 || groups[0][0].Kind != "finding" || groups[1][0].Kind != "note" || groups[2][0].Kind != "risk" {
		t.Fatalf("%+v", groups)
	}
	md := s.Markdown()
	for _, want := range []string{"# Findings", "## Finding", "## Risk", "- [ ] **Alpha host** (f1, 100%, a) `host`", "  - url http://alpha", "- [-] **Gamma**", "  expires soon"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	js, err := s.JSON()
	if err != nil || !strings.Contains(string(js), `"id": "f1"`) {
		t.Fatalf("%v %s", err, js)
	}
	f, _ := s.Get("f2")
	f.Status = StatusResolved
	f.Evidence = []Evidence{{Kind: "file", Ref: "a.go", Line: 7, Quote: "x"}}
	if line := f.Markdown(); !strings.Contains(line, "- [x]") || !strings.Contains(line, "a.go:7") {
		t.Fatalf("%s", line)
	}
}

func TestToolRecordsAndRefines(t *testing.T) {
	s := New()
	tl := Tool{Store: s}
	def := tl.Definition()
	if def.Name != ToolName || !strings.Contains(def.Description, "finding, risk") || !tl.Annotations().Idempotent {
		t.Fatalf("%+v", def)
	}
	ctx := policy.WithAgent(context.Background(), "scout")
	res, err := tl.Run(ctx, json.RawMessage(`{"title":"Open redirect","kind":"finding","tags":["web"],"confidence":0.8,"evidence":[{"ref":"app.go","line":12}]}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	f := res.Data.(Fact)
	if f.ID != "f1" || f.Agent != "scout" || f.Confidence != 0.8 || f.Evidence[0].Kind != "file" {
		t.Fatalf("%+v", f)
	}
	res, _ = tl.Run(ctx, json.RawMessage(`{"id":"f1","title":"Open redirect on /login","status":"resolved"}`))
	if res.IsError {
		t.Fatalf("%+v", res)
	}
	f = res.Data.(Fact)
	if f.Title != "Open redirect on /login" || f.Status != StatusResolved || f.Confidence != 0.8 || len(f.Tags) != 1 || len(f.Evidence) != 1 || f.Kind != "finding" {
		t.Fatalf("refinement must keep what was not given: %+v", f)
	}
	res, _ = tl.Run(ctx, json.RawMessage(`{"id":"f7","title":"given id"}`))
	if res.IsError || res.Data.(Fact).ID != "f7" || res.Data.(Fact).Confidence != 0.5 {
		t.Fatalf("%+v", res)
	}
	for _, bad := range []string{`{"title":"x","kind":"weird"}`, `{"title":""}`, `{bad`, `{"title":"x","confidence":3}`} {
		if res, _ := tl.Run(ctx, json.RawMessage(bad)); !res.IsError {
			t.Fatalf("%s accepted", bad)
		}
	}
	if res, _ := (Tool{}).Run(ctx, json.RawMessage(`{"title":"x"}`)); !res.IsError {
		t.Fatal("tool without a store ran")
	}
	limited := Tool{Store: s, Kinds: []string{"bug"}}
	if res, _ := limited.Run(ctx, json.RawMessage(`{"title":"x","kind":"finding"}`)); !res.IsError {
		t.Fatal("kind outside the allowed list accepted")
	}
	if res, _ := limited.Run(ctx, json.RawMessage(`{"title":"x","kind":"Bug"}`)); res.IsError {
		t.Fatal("allowed kind refused")
	}
}
