package tuiserver

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Background process a session turn spawned, owned by the session key
func spawnOwnedProcess(registry *tools.ProcessRegistry, owner string, notify bool) *tools.ProcessSession {
	proc := tools.NewProcessSession(fmt.Sprintf("proc_%d", registry.CountRunning()), "icebath -p reviewer chat")
	proc.SessionKey, proc.OwnerTaskID, proc.StartedAt, proc.NotifyOnComplete = owner, owner, nowSeconds(), notify
	registry.InsertRunning(proc)
	return proc
}
