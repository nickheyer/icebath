package app

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
)

// AIAgent the CLI holds whose staging append and run block until the case releases them
type closeRaceAgent struct {
	*GatewayAgent
	real           *agent.AIAgent
	blockAppend    atomic.Bool
	stagingEntered chan struct{}
	releaseStaging chan struct{}
	runEntered     chan struct{}
	releaseRun     chan struct{}
	result         map[string]any
}

func (d *closeRaceAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	d.real.StagePendingCLIUserMessage(staged, func() {
		if d.blockAppend.CompareAndSwap(true, false) {
			close(d.stagingEntered)
			select {
			case <-d.releaseStaging:
			case <-time.After(5 * time.Second):
			}
		}
		appendToTranscript()
	})
}

func (d *closeRaceAgent) PersistActiveSessionBeforeClose(ctx context.Context, readCLIHistory func() []map[string]any) bool {
	return d.real.PersistActiveSessionBeforeClose(ctx, readCLIHistory)
}

func (d *closeRaceAgent) SessionID() string { return d.real.SessionID() }

func (d *closeRaceAgent) RunConversation(context.Context, any, agent.RunConversationOptions) (map[string]any, error) {
	close(d.runEntered)
	select {
	case <-d.releaseRun:
	case <-time.After(5 * time.Second):
	}
	return d.result, nil
}

// Waits for a signal up to the given time, true when it arrived
func signalled(ch chan struct{}, wait time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(wait):
		return false
	}
}
