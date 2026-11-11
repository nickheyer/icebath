package gatewayrun

import "github.com/nickheyer/icebath/pkg/tools"

// Value of a key, the default when the event lacks it
func evtGet(evt map[string]any, key string, def any) any {
	if v, ok := evt[key]; ok {
		return v
	}
	return def
}

// Gateway owned watch events drained from the queue, async delegations put back and completions left to the watcher
func drainGatewayWatchEvents(queue *tools.EventQueue) []map[string]any {
	var watch, requeue []map[string]any
	for !queue.Empty() {
		evt, ok := queue.GetNowait()
		if !ok {
			break
		}
		switch evtGet(evt, "type", "completion") {
		case "watch_match", "watch_disabled", "watch_overflow_tripped", "watch_overflow_released", "heartbeat":
			watch = append(watch, evt)
		case "async_delegation":
			requeue = append(requeue, evt)
		}
	}
	for _, evt := range requeue {
		queue.Put(evt)
	}
	return watch
}
