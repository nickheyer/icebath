package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the interrupt bit, reason, pending yield, thread scoped sudo password and log session tag before reuse
func resetReusedThreadState(ident tools.WorkerID) {
	tools.SetInterrupt(ident, false, "")
	tools.ForgetThreadSudoPassword(ident)
	logging.ClearSessionContext()
}
