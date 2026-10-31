package harness

import (
	"context"
	"errors"
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
	if _, err := fs.Restart(context.Background(), "demo-9", w, nil, "x"); err == nil {
		t.Fatal("restarted an unknown flow")
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
	if again.Err() != nil || runs != 2 {
		t.Fatalf("restarted run %v after %d runs", again.Err(), runs)
	}
	got, ok := fs.Get("demo-1")
	if !ok || got != again || len(fs.List()) != 2 {
		t.Fatalf("registry after restart: %v %d flows", ok, len(fs.List()))
	}
	done := 0
	for _, m := range msgs {
		if _, ok := m.(FlowDoneMsg); ok {
			done++
		}
	}
	if done != 3 {
		t.Fatalf("%d done messages", done)
	}
}
