package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/session"
)

func TestStoreLifecycle(t *testing.T) {
	sess := session.New()
	s := New()
	s.Attach(sess)
	var seen []Change
	s.OnChange(func(c Change) { seen = append(seen, c) })
	if _, err := s.Create("  "); err == nil {
		t.Fatal("empty objective accepted")
	}
	m, err := s.Create(" Map the network ")
	if err != nil || m.ID != "m1" || m.Objective != "Map the network" || m.Status != StatusActive || m.Started.IsZero() {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := s.SetPlan("m1", []Step{{Text: "scan"}, {Text: ""}}); err == nil {
		t.Fatal("empty step accepted")
	}
	if _, err := s.SetPlan("m1", []Step{{Text: "scan", Status: "odd"}}); err == nil {
		t.Fatal("unknown step status accepted")
	}
	m, err = s.SetPlan("m1", []Step{{Text: " scan "}, {Text: "probe", Status: StepActive}, {Text: "report"}})
	if err != nil || len(m.Plan) != 3 || m.Plan[0].Text != "scan" || m.Plan[0].Status != StepPending || m.Plan[1].Status != StepActive {
		t.Fatalf("%v %+v", err, m)
	}
	if cur, ok := m.Current(); !ok || cur.Text != "probe" {
		t.Fatalf("%+v %v", cur, ok)
	}
	if m, err = s.SetStep("m1", 1, StepDone); err != nil || m.Plan[0].Status != StepDone || m.Progress() < 0.33 || m.Progress() > 0.34 {
		t.Fatalf("%v %+v", err, m)
	}
	if m, err = s.SetStep("m1", 3, StepActive); err != nil || m.Plan[1].Status != StepPending || m.Plan[2].Status != StepActive {
		t.Fatalf("activating a step must demote the active one: %v %+v", err, m)
	}
	if _, err := s.SetStep("m1", 9, StepDone); err == nil {
		t.Fatal("out of range step accepted")
	}
	if _, err := s.SetStep("m1", 1, "odd"); err == nil {
		t.Fatal("unknown status accepted")
	}
	if _, err := s.SetStep("nope", 1, StepDone); err == nil {
		t.Fatal("missing mission accepted")
	}
	if m, err = s.Assign("m1", "scout"); err != nil || len(m.Agents) != 1 {
		t.Fatalf("%v %+v", err, m)
	}
	before := len(seen)
	if m, _ = s.Assign("m1", "scout"); len(m.Agents) != 1 || len(seen) != before {
		t.Fatal("assign must not duplicate or write a record")
	}
	if m, _ = s.SetNote("m1", " halfway "); m.Note != "halfway" {
		t.Fatalf("%+v", m)
	}
	if _, err := s.Finish("m1", "weird", ""); err == nil {
		t.Fatal("non end state accepted")
	}
	if m, err = s.Finish("m1", StatusFailed, "timeout"); err != nil || !m.Ended() || m.Finished.IsZero() || m.Error != "timeout" {
		t.Fatalf("%v %+v", err, m)
	}
	if m, err = s.Resume("m1"); err != nil || m.Ended() || !m.Finished.IsZero() || m.Error != "" {
		t.Fatalf("%v %+v", err, m)
	}
	md := m.Markdown()
	for _, want := range []string{"## Map the network (active, 33%)", "- [x] scan", "- [>] report", "halfway"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	m2, _ := s.Create("second")
	if s.Len() != 2 || len(s.List()) != 2 || s.List()[1].ID != m2.ID {
		t.Fatal("list")
	}
	got, ok := s.Get("m1")
	if !ok {
		t.Fatal("get")
	}
	got.Plan[0].Text = "mutated"
	if again, _ := s.Get("m1"); again.Plan[0].Text != "scan" {
		t.Fatal("get must copy")
	}
	if len(seen) < 9 || seen[0].Op != OpAdd || seen[1].Op != OpUpdate {
		t.Fatalf("%d changes seen", len(seen))
	}
	restored := New()
	if err := restored.Restore(sess); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 2 {
		t.Fatalf("restored %d", restored.Len())
	}
	if r, _ := restored.Get("m1"); r.Note != "halfway" || len(r.Plan) != 3 || r.Status != StatusActive {
		t.Fatalf("%+v", r)
	}
	if n, _ := restored.Create("third"); n.ID != "m3" {
		t.Fatalf("restored counter must continue, got %s", n.ID)
	}
	if !s.Remove("m2") || s.Remove("m2") || s.Len() != 1 {
		t.Fatal("remove")
	}
	corrupt := session.FromHeader(session.Header{ID: "c"}, nil, []session.Record{{Seq: 1, Kind: RecordKind, Data: json.RawMessage(`{"id":`)}})
	if err := New().Restore(corrupt); err == nil {
		t.Fatal("corrupt record accepted")
	}
	noID := session.FromHeader(session.Header{ID: "c"}, nil, []session.Record{{Seq: 1, Kind: RecordKind, Data: json.RawMessage(`{"objective":"x"}`)}})
	if err := New().Restore(noID); err == nil {
		t.Fatal("record without id accepted")
	}
	if (Mission{}).Progress() != 0 {
		t.Fatal("progress without a plan")
	}
	if _, ok := (Mission{Plan: []Step{{Status: StepDone}}}).Current(); ok {
		t.Fatal("current with everything done")
	}
	if IDFrom(context.Background()) != "" || IDFrom(WithID(context.Background(), "m1")) != "m1" {
		t.Fatal("context id")
	}
}

func TestPlanTool(t *testing.T) {
	s := New()
	m, _ := s.Create("obj")
	tl := Tool{Store: s}
	if tl.Definition().Name != ToolName || !tl.Annotations().Idempotent {
		t.Fatal("definition")
	}
	ctx := WithID(context.Background(), m.ID)
	res, err := tl.Run(ctx, json.RawMessage(`{"steps":[{"text":"a","status":"done"},{"text":"b","status":"active"},{"text":"c"}],"note":"going"}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	got := res.Data.(Mission)
	if len(got.Plan) != 3 || got.Note != "going" || got.Plan[2].Status != StepPending {
		t.Fatalf("%+v", got)
	}
	for _, bad := range []string{`{bad`, `{"steps":[]}`, `{"steps":[{"text":"a","status":"active"},{"text":"b","status":"active"}]}`, `{"steps":[{"text":""}]}`} {
		if res, _ := tl.Run(ctx, json.RawMessage(bad)); !res.IsError {
			t.Fatalf("%s accepted", bad)
		}
	}
	if res, _ := tl.Run(context.Background(), json.RawMessage(`{"steps":[{"text":"a"}]}`)); !res.IsError {
		t.Fatal("run without a mission accepted")
	}
	if res, _ := tl.Run(WithID(context.Background(), "nope"), json.RawMessage(`{"steps":[{"text":"a"}]}`)); !res.IsError {
		t.Fatal("unknown mission accepted")
	}
	if res, _ := (Tool{}).Run(ctx, json.RawMessage(`{"steps":[{"text":"a"}]}`)); !res.IsError {
		t.Fatal("tool without a store ran")
	}
}
