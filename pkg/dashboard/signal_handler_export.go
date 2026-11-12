package dashboard

import (
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pysignal"
)

// Handler in the process slot of the signal, the signal.getsignal a listener start must leave unchanged
func SignalHandler(sig os.Signal) any { return pysignal.Getsignal(sig) }
