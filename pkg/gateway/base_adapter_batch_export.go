package gateway

import "context"

// Text batch timings of the hermes base adapter class
const (
	TextBatchDefaultDelayS      = textBatchDefaultDelayS
	TextBatchMaxDelayS          = textBatchMaxDelayS
	TextBatchDefaultSplitDelayS = textBatchDefaultSplitDelayS
	TextBatchMaxSplitDelayS     = textBatchMaxSplitDelayS
	SplitThreshold              = splitThreshold
)

// Flush task currently owning a text batch key, nil when none
func (b *BasePlatformAdapter) TextBatchTask(key string) *AdapterTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingTextBatchTasks[key]
}

// Drops the key's flush task slot when task still owns it
func (b *BasePlatformAdapter) ReleaseTextBatchTask(key string, task *AdapterTask) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if task != nil && b.pendingTextBatchTasks[key] == task {
		delete(b.pendingTextBatchTasks, key)
	}
}

// Every text batch flush task still registered
func (b *BasePlatformAdapter) TextBatchTasks() []*AdapterTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*AdapterTask, 0, len(b.pendingTextBatchTasks))
	for _, t := range b.pendingTextBatchTasks {
		out = append(out, t)
	}
	return out
}

// Every buffered text batch event
func (b *BasePlatformAdapter) TextBatchEvents() []*MessageEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*MessageEvent, 0, len(b.pendingTextBatches))
	for _, e := range b.pendingTextBatches {
		out = append(out, e)
	}
	return out
}

// Clears every buffered text batch and its flush task slot
func (b *BasePlatformAdapter) ClearTextBatches() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingTextBatches = map[string]*MessageEvent{}
	b.pendingTextBatchTasks = map[string]*adapterTask{}
}

// Code point length of the last chunk merged into the batch
func (e *MessageEvent) LastChunkLen() int { return e.lastChunkLen }

// Applies the runner topic recovery hook to the event source
func (b *BasePlatformAdapter) ApplyTopicRecovery(event *MessageEvent) { b.applyTopicRecovery(event) }

// Whether an event whose routing identity cannot resolve was dropped
func (b *BasePlatformAdapter) DropUnresolved(ctx context.Context, event *MessageEvent) bool {
	return b.dropUnresolved(ctx, event)
}

// Delivery scope of the source's routed profile and its release
func (b *BasePlatformAdapter) MediaDeliveryScope(ctx context.Context, source *SessionSource) (context.Context, func()) {
	return b.mediaDeliveryScope(ctx, source)
}

// Caption merge of a batched media event, a repeated caption kept once
func MergeCaption(existingText, newText string) string { return mergeCaption(existingText, newText) }
