package cron

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestCronSubmitCreationCommitWaitsOffTheOwnerLoop(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local"), Model: prompt("fixture")})
	authority, db, ctx := recoveryOwner(t, s)
	if _, err := gateway.CronOwnerOperation(ctx, authority, "submit", map[string]any{"job_id": job.ID, "request_id": "warm", "extra_prompt": nil}); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(release) }) }
	holder := make(chan struct{})
	defer func() {
		releaseWriter()
		<-holder
	}()
	go func() {
		defer close(holder)
		other, err := sql.Open("sqlite", db.Path()+"?_pragma=busy_timeout(10000)")
		if err != nil {
			t.Error(err)
			return
		}
		defer other.Close()
		conn, err := other.Conn(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Error(err)
			return
		}
		close(held)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never held")
	}
	var gaps []time.Duration
	stopTicker, tickerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(tickerDone)
		last := time.Now()
		for {
			select {
			case <-stopTicker:
				return
			case <-time.After(20 * time.Millisecond):
			}
			now := time.Now()
			gaps = append(gaps, now.Sub(last))
			last = now
		}
	}()
	type submitted struct {
		receipt map[string]any
		err     error
	}
	submit := make(chan submitted, 1)
	go func() {
		receipt, err := gateway.CronOwnerOperation(ctx, authority, "submit", map[string]any{"job_id": job.ID, "request_id": "fire", "extra_prompt": nil})
		submit <- submitted{receipt, err}
	}()
	time.Sleep(time.Second)
	releaseWriter()
	var got submitted
	select {
	case got = <-submit:
	case <-time.After(30 * time.Second):
		t.Fatal("submit never answered")
	}
	close(stopTicker)
	<-tickerDone
	if got.err != nil {
		t.Fatal(got.err)
	}
	rows, err := db.ListSessionAdmissions(ctx, got.receipt["session_id"].(string), false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("admissions = %d, %v", len(rows), err)
	}
	var worst time.Duration
	for _, gap := range gaps {
		worst = max(worst, gap)
	}
	if worst >= 500*time.Millisecond {
		t.Fatalf("owner loop stalled %v", worst)
	}
}
