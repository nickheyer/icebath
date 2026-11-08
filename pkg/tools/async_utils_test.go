package tools

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestReturnsFutureOnSuccess(t *testing.T) {
	pool := NewDaemonPool(1, "async-utils-test")
	defer pool.Shutdown(true, false)
	fut := SafeScheduleThreadsafe(context.Background(), pool, func(context.Context) (any, error) { return 42, nil }, nil, "", slog.LevelWarn)
	if fut == nil {
		t.Fatal("no future returned")
	}
	value, err := fut.Result(2 * time.Second)
	if err != nil || value != 42 {
		t.Fatalf("result %v %v", value, err)
	}
}

func TestSchedulingExceptionClosesCoroutine(t *testing.T) {
	pool := NewDaemonPool(1, "async-utils-test")
	pool.Shutdown(true, false)
	ran := false
	fut := SafeScheduleThreadsafe(context.Background(), pool, func(context.Context) (any, error) {
		ran = true
		return "ok", nil
	}, nil, "", slog.LevelWarn)
	if fut != nil {
		t.Fatalf("a refused submission returned a future %v", fut)
	}
	if ran {
		t.Fatal("the body ran after the scheduler refused it")
	}
}
