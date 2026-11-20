package media

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	streamingInputRate          = 16000
	defaultOpenAIStreamingModel = "gpt-live-transcribe"
	elevenLabsRealtimeModel     = "scribe_v2_realtime"
	streamingFinalizeTimeout    = 20.0
	streamingConnectTimeout     = 10 * time.Second
)

// Ordered JSON object from alternating keys and values
func jsonObject(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}
