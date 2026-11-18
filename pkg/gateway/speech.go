package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Streaming TTS of one voice turn as the turn runner drives it
type TTSStream interface {
	Active() bool
	Start() *STTSDrainTask
	OnDelta(text *string)
	Finish()
	Abort(reason string)
	Done() bool
	SuppressWholeFile() bool
	WaitComplete(timeout float64) bool
}

// Opens the streaming TTS of a voice turn from the env's TTS config, set by the plugins tier and nil in a build without it
var OpenTTSStream func(ctx context.Context, env *tools.Env, adapter PlatformAdapter, chatID string, metadata map[string]any) (TTSStream, error)

// Drain task of one consumer with its completion channel
type STTSDrainTask struct {
	done chan struct{}
}

// Whether the drain task has ended
func (t *STTSDrainTask) Done() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}
