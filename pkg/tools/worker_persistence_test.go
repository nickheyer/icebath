package tools

import (
	"errors"
	"sync"
	"testing"
)

func TestWorkerPersistenceErrorCarriesTheReasonAndClass(t *testing.T) {
	err := NewWorkerPersistenceError("worker_cron_registration_required")
	var wp *WorkerPersistenceError
	if !errors.As(err, &wp) || err.Error() != "worker_cron_registration_required" || wp.ExceptionName() != "WorkerPersistenceError" {
		t.Fatalf("err = %#v", err)
	}
}

func TestNonDispatcherOwnedContextFlipsAndRestoresTheGate(t *testing.T) {
	t.Setenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT", "")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	env := &Env{Values: &sync.Map{}}
	if !isDispatcherOwnedWorkerContext(env) {
		t.Fatal("fresh env should be dispatcher owned")
	}
	restore := EnterNonDispatcherOwnedContext(env)
	if isDispatcherOwnedWorkerContext(env) {
		t.Fatal("entered env should not be dispatcher owned")
	}
	inner := EnterNonDispatcherOwnedContext(env)
	inner()
	if isDispatcherOwnedWorkerContext(env) {
		t.Fatal("nested restore must keep the outer flag")
	}
	restore()
	if !isDispatcherOwnedWorkerContext(env) {
		t.Fatal("restore should return the dispatcher owned gate")
	}
}
