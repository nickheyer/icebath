package gateway

import (
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const RestartWaitReason = "restart_wait"

// DrainRunner is the runner state the restart wait checks read and the descriptor they stamp
type DrainRunner interface {
	SessionRuntimeDescriptor() map[string]any
	Draining() bool
	Running() bool
}

// Publishes the restart after turn wait as the drain reason unless a drain already named its own
func OpenRestartWait(runner DrainRunner) {
	descriptor := runner.SessionRuntimeDescriptor()
	if descriptor == nil || pyrepr.Truthy(descriptor["drain_reason"]) {
		return
	}
	if publisher, republishes := runner.(runtimeDescriptorPublisher); republishes {
		publisher.SetRuntimeDescriptorField("drain_reason", RestartWaitReason)
		return
	}
	descriptor["drain_reason"] = RestartWaitReason
}

// Runner that republishes its descriptor on a write instead of changing the map its readers hold
type runtimeDescriptorPublisher interface {
	SetRuntimeDescriptorField(key string, value any)
}

// True while the runner is draining, still running and in a restart after turn wait
func RestartWaitOpen(runner DrainRunner) bool {
	descriptor := runner.SessionRuntimeDescriptor()
	reason, _ := descriptor["drain_reason"].(string)
	return runner.Draining() && runner.Running() && reason == RestartWaitReason
}

var restartWaitWSMethods = map[string]bool{
	"ping": true, "runtime.describe": true, "session.resume": true, "session.detach": true,
	"session.info": true, "session.list": true, "prompt.submit": true, "prompt.receipt": true, "session.interrupt": true,
	"approval.respond": true, "clarify.respond": true, "clarify.lock": true,
}

// True when an interactive WebSocket verb is refused because the runner is draining
func WSMethodRefusedWhileDraining(runner DrainRunner, method string) bool {
	return runner.Draining() && !restartWaitWSMethods[method]
}

var (
	epochMu      sync.Mutex
	epochCached  *string
	procReadFile = os.ReadFile
)

// Container or VM instantiation identity of boot id and PID 1 start time, empty when neither is readable
var CurrentInstantiationEpoch = func() string {
	epochMu.Lock()
	defer epochMu.Unlock()
	if epochCached != nil {
		return *epochCached
	}
	bootID, pid1Start := "", ""
	if b, err := procReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = pystr.Strip(string(b))
	}
	if b, err := procReadFile("/proc/1/stat"); err == nil {
		if i := strings.LastIndex(string(b), ")"); i >= 0 {
			if fields := pystr.SplitWhitespace(string(b)[i+1:], -1); len(fields) > 19 {
				pid1Start = fields[19]
			}
		}
	}
	epoch := ""
	if bootID != "" || pid1Start != "" {
		epoch = bootID + ":" + pid1Start
	}
	epochCached = &epoch
	return epoch
}
