package media

import (
	"time"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	streamingInputRate          = 16000
	defaultOpenAIStreamingModel = "gpt-live-transcribe"
	elevenLabsRealtimeModel     = "scribe_v2_realtime"
	streamingFinalizeTimeout    = 20.0
	streamingConnectTimeout     = 10 * time.Second
)

// Ordered JSON object from alternating keys and values
func jsonObject(pairs ...any) *jsonutil.Object {
	o := jsonutil.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}
