package gateway

import (
	"sync"
	"sync/atomic"
	"time"
)

const memoryBytesToMB = 1024 * 1024

var (
	memoryMonitorMu    sync.Mutex
	memoryMonitorStop  chan struct{}
	memoryMonitorDone  chan struct{}
	memoryMonitorStart atomic.Pointer[time.Time]
)

// Process RSS high water mark in MB, false when the process memory cannot be read
var processRSSMB = readProcessRSSMB
