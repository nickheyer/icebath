package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Keeps authority tasks alive until their last durable settlement write within the stop budget
func (r *GatewayRunner) settleGatewayRuntime(ctx context.Context, budget float64, reason *string) error {
	authorities := gateway.AllAuthorities(r)
	gateway.StopManagedTurns(r, reason)
	for _, a := range authorities {
		gateway.StopOneshots(a)
	}
	var tasks []<-chan struct{}
	for _, a := range authorities {
		tasks = append(tasks, gateway.AuthorityTasks(a)...)
	}
	wait := max(0.0, min(gateway.TurnSettleSeconds, budget-gateway.StopTailReserveSeconds))
	if len(tasks) > 0 {
		pending := len(tasks)
		if wait > 0 {
			pending = waitTasks(ctx, tasks, time.Duration(wait*float64(time.Second)))
		}
		if pending > 0 {
			runtimeLog().Warn(fmt.Sprintf("%d authority task(s) did not settle within %.0fs; left for recovery", pending, wait))
		}
	}
	for _, a := range authorities {
		err := gateway.JoinAuthorityWork(ctx, a, 0)
		var timeout *gateway.PyError
		if errors.As(err, &timeout) && timeout.Kind == "TimeoutError" {
			runtimeLog().Warn(fmt.Sprintf("Profile %s still has live writers at shutdown; its admissions stay for recovery", a.ProfileID))
		} else if err != nil {
			return err
		}
	}
	for _, a := range gateway.AllAuthorities(r) {
		if _, err := gateway.EndIdleACPSessions(ctx, a); err != nil {
			runtimeLog().Warn(fmt.Sprintf("ACP sessions of %s not ended at shutdown", a.ProfileID), "exc_info", err)
		}
	}
	if store := r.sessionTicketStore; store != nil {
		store.Revoke()
	}
	if handle := r.sessionAPI; handle != nil {
		stopGatewayAPI(ctx, handle)
	}
	return nil
}

// Waits for every task up to timeout and returns how many are still running
func waitTasks(ctx context.Context, tasks []<-chan struct{}, timeout time.Duration) int {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, task := range tasks {
		select {
		case <-task:
		case <-deadline.C:
			return countPending(tasks)
		case <-ctx.Done():
			return countPending(tasks)
		}
	}
	return 0
}

// Tasks whose done signal is still open
func countPending(tasks []<-chan struct{}) int {
	pending := 0
	for _, task := range tasks {
		select {
		case <-task:
		default:
			pending++
		}
	}
	return pending
}
