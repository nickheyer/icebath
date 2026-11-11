package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
)

// Admits the event on the reader, or off it behind the chat's earlier admission when a producer hook may await the reader
func admitRelayDelivery(ctx context.Context, a *RelayAdapter, runner any, event *MessageEvent) (RelayAdmission, error) {
	if !(plugins.HasHook(ctx, "pre_gateway_dispatch") || plugins.HasHook(ctx, PostAdmissionHook)) {
		return nil, relayAdmit(ctx, a, runner, event)
	}
	lane := [2]string{string(event.Source.Platform), event.Source.ChatID}
	settled := make(chan error, 1)
	a.mu.Lock()
	previous := a.admissionLanes[lane]
	ready := make(chan struct{})
	task := spawnAdapterTask(ctx, func(tctx context.Context) error {
		<-ready
		return relayAdmitAfter(tctx, previous, a, runner, event)
	})
	a.admissionLanes[lane] = task
	a.mu.Unlock()
	task.addDoneCallback(func() {
		a.mu.Lock()
		if a.admissionLanes[lane] == task {
			delete(a.admissionLanes, lane)
		}
		a.mu.Unlock()
	})
	a.trackBackgroundTask(task)
	task.addDoneCallback(func() { settled <- task.err })
	close(ready)
	return settled, nil
}

// Waits for the chat's earlier admission to finish whatever its outcome, then admits this event
func relayAdmitAfter(ctx context.Context, previous *adapterTask, a *RelayAdapter, runner any, event *MessageEvent) error {
	if previous != nil {
		select {
		case <-previous.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return relayAdmit(ctx, a, runner, event)
}

// Commits the event through the durable producer, acknowledging permanent and parked refusals and replaying the rest
func relayAdmit(ctx context.Context, a *RelayAdapter, runner any, event *MessageEvent) error {
	receipt, err := AdmitProducer(ctx, a, event)
	if err != nil {
		var parked *ParkedProfileRefusal
		if errors.As(err, &parked) {
			replyTo := ""
			if event.MessageID != nil {
				replyTo = *event.MessageID
			}
			refuseParkedRoute(ctx, a, event.Source, parked.Profile, replyTo)
			return nil
		}
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			if !relayPermanentRefusals[refusal.Reason] {
				return err
			}
			relayAdapterLog().Warn(fmt.Sprintf("relay inbound refused (%s) for %s chat=%s; acknowledged, not replayed",
				refusal.Reason, string(event.Source.Platform), event.Source.ChatID))
			return nil
		}
		return err
	}
	reply := event.ConsumerReply()
	if receipt != nil || reply == nil || *reply == "" {
		return nil
	}
	authority, ok := runner.(AuthorityRunner)
	if !ok || authority == nil {
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_session_key_for_source'", runner)}
	}
	sessionKey, err := runnerSessionKeyForSource(ctx, authority, event.Source)
	if err != nil {
		return err
	}
	text := *reply
	task := spawnAdapterTask(ctx, func(tctx context.Context) error {
		_, err := a.deliverResponse(tctx, event, sessionKey, text)
		return err
	})
	a.trackBackgroundTask(task)
	return nil
}
