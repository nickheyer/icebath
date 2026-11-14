package gateway

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
)

// Installs the gateway implementations of the state hooks cmd/icebath assigns at startup
func init() {
	state.MessagingPlatformLabels = MessagingPlatformLabels
	state.MessagingPlatformSource = IsMessagingPlatformSource
	state.SameOwner = SameOwner
	state.OwnersAgree = OwnersAgree
	state.ParseAuthenticatedSubject = ParseAuthenticatedSubject
	state.AdmissionMediaReferences = AdmissionMediaReferences
	state.HostedDocumentReferences = HostedDocumentReferences
	state.ReleaseUnheldMedia = ReleaseUnheldMediaRetainingHistory
	state.SessionKeyNamespace = SessionKeyNamespace
	state.ExclusiveMaintenance = ExclusiveMaintenance
	state.LiveRuntimeOwner = LiveRuntimeOwner
	state.CanonicalHome = CanonicalHome
	state.ProfileLabelForHome = ProfileLabelForHome
	state.ReadGatewayLockRecord = ReadGatewayLockRecord
	state.IsOwnershipConflict = func(err error) bool {
		var conflict *OwnershipConflict
		return errors.As(err, &conflict)
	}
}
