package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Installs the pkg/gateway implementations of the state hooks cmd/icebath assigns at startup
func init() {
	state.MessagingPlatformLabels = gateway.MessagingPlatformLabels
	state.MessagingPlatformSource = gateway.IsMessagingPlatformSource
	state.SameOwner = gateway.SameOwner
	state.OwnersAgree = gateway.OwnersAgree
	state.ParseAuthenticatedSubject = gateway.ParseAuthenticatedSubject
	state.AdmissionMediaReferences = gateway.AdmissionMediaReferences
	state.HostedDocumentReferences = gateway.HostedDocumentReferences
	state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
	state.SessionKeyNamespace = gateway.SessionKeyNamespace
	state.ExclusiveMaintenance = gateway.ExclusiveMaintenance
	state.LiveRuntimeOwner = gateway.LiveRuntimeOwner
	state.CanonicalHome = gateway.CanonicalHome
	state.ProfileLabelForHome = gateway.ProfileLabelForHome
	state.ReadGatewayLockRecord = gateway.ReadGatewayLockRecord
}
