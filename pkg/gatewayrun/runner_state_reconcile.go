package gatewayrun

import (
	"sync"
)

// Class level defaults of the served profile reconcile mixin, all unset until boot records the served set
type runnerReconcileState struct {
	servedProfileSignatures    map[string][]any
	profileReconcileLockOnce   sync.Once
	profileReconcileLock       *sync.Mutex
	profileOwnGatewayWarned    map[string]bool
	profileProbeTimeoutWarned  map[string]bool
	hookConsentSignatures      map[string][]any
	singleProfileBlockerLogged *string
}
