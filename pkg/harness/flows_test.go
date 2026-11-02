package harness

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFlowsRestartReusesTheID(t *testing.T) {
	var msgs []any
	fs := NewFlows(nil, func(msg any) { msgs = append(msgs, msg) })
	runs := 0
	w := Workflow{Name: "demo", Run: func(ctx context.Context, f *Flow, arg string) error {
		runs++
		if runs == 1 {
			return errors.New("boom")
		}
		return nil
	}}
	first, err := fs.Start(context.Background(), w, nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	<-first.Done()
	if first.Err() == nil || first.ID != "demo-1" {
		t.Fatalf("first run %s %v", first.ID, first.Err())
	}
	adopted, err := fs.Restart(context.Background(), "demo-9", w, nil, "x")
	if err != nil || adopted.ID != "demo-9" {
		t.Fatalf("restart of a flow from an earlier process: %v %v", adopted, err)
	}
	<-adopted.Done()
	if _, err := fs.Restart(context.Background(), "bad id!", w, nil, "x"); err == nil {
		t.Fatal("restarted a flow with an invalid id")
	}
	hold := make(chan struct{})
	blocker := Workflow{Name: "slow", Run: func(ctx context.Context, f *Flow, arg string) error { <-hold; return nil }}
	slow, err := fs.Start(context.Background(), blocker, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Restart(context.Background(), slow.ID, blocker, nil, ""); err == nil {
		t.Fatal("restarted a running flow")
	}
	close(hold)
	<-slow.Done()
	again, err := fs.Restart(context.Background(), "demo-1", w, nil, "y")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != "demo-1" || again.Arg != "y" {
		t.Fatalf("restarted flow %s %q", again.ID, again.Arg)
	}
	select {
	case <-again.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("restarted flow did not end")
	}
	if again.Err() != nil || runs != 3 {
		t.Fatalf("restarted run %v after %d runs", again.Err(), runs)
	}
	got, ok := fs.Get("demo-1")
	if !ok || got != again || len(fs.List()) != 3 {
		t.Fatalf("registry after restart: %v %d flows", ok, len(fs.List()))
	}
	done := 0
	for _, m := range msgs {
		if _, ok := m.(FlowDoneMsg); ok {
			done++
		}
	}
	if done != 4 {
		t.Fatalf("%d done messages", done)
	}
}

func TestFlowsStartTakesTheWorkflowID(t *testing.T) {
	fs := NewFlows(nil, func(any) {})
	n := 6
	w := Workflow{Name: "named", NextID: func(*App) (string, error) { n++; return "named-" + strconv.Itoa(n), nil }, Run: func(ctx context.Context, f *Flow, arg string) error { return nil }}
	f, err := fs.Start(context.Background(), w, nil, "")
	if err != nil || f.ID != "named-7" {
		t.Fatalf("flow %v %v", f, err)
	}
	<-f.Done()
	n = 6
	if _, err := fs.Start(context.Background(), w, nil, ""); err == nil {
		t.Fatal("started a second flow under an id that already ran")
	}
	bad := Workflow{Name: "bad", NextID: func(*App) (string, error) { return "no spaces here", nil }, Run: w.Run}
	if _, err := fs.Start(context.Background(), bad, nil, ""); err == nil {
		t.Fatal("started a flow with an invalid id")
	}
	failing := Workflow{Name: "failing", NextID: func(*App) (string, error) { return "", errors.New("disk gone") }, Run: w.Run}
	if _, err := fs.Start(context.Background(), failing, nil, ""); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("id failure not reported: %v", err)
	}
}
