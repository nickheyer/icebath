package dashboard

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Assigns the gateway, update and store ownership hooks cmd/icebath/wiring.go sets, the code hermes cases run unpatched
func init() {
	GatewayCLI.NeedsSudo = selfupdate.NeedsSudo
	GatewayCLI.SudoNoninteractiveOk = selfupdate.SudoNoninteractiveOk
	GatewayCLI.RefusalLines = gatewaycli.RefusalLines
	GatewayCLI.InstalledServices = gatewayrun.InstalledServiceCount
	UpdateCLI.IsCommitBuild = selfupdate.IsCommitBuild
	UpdateCLI.CommitBuildMessage = selfupdate.CommitBuildUpdateMessage
	UpdateCLI.UpdateAdmissionRefusal = selfupdate.UpdateAdmissionRefusal
	UpdateCLI.RecordUpdateRefusal = selfupdate.RecordUpdateRefusal
	UpdateCLI.CheckForUpdates = selfupdate.CheckForUpdatesForce
	UpdateCLI.BuildMigrationPlan = gatewayrun.MigrationPlanDict
	StatusCLI.ReadLogTail = app.ReadLogTail
	StatusCLI.BuildDebugShare = selfupdate.DebugShareForDashboard
	StatusCLI.RedactDebugSupportText = selfupdate.RedactDebugSupportText
	ConsoleCLI.NewEngine = app.NewDashboardConsole
	if state.SpilloverSQLFilter == "" {
		state.MessagingPlatformLabels = gateway.MessagingPlatformLabels
		state.MessagingPlatformSource = gateway.IsMessagingPlatformSource
		state.SameOwner = gateway.SameOwner
		state.OwnersAgree = gateway.OwnersAgree
		state.ParseAuthenticatedSubject = gateway.ParseAuthenticatedSubject
		state.AdmissionMediaReferences = gateway.AdmissionMediaReferences
		state.HostedDocumentReferences = gateway.HostedDocumentReferences
		state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
		state.SpilloverSQLFilter = tools.SpilloverSQLFilter
		state.SpilloverNamesInText = tools.SpilloverNamesInText
		state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
		state.SessionKeyNamespace = gateway.SessionKeyNamespace
	}
	if state.ExclusiveMaintenance == nil {
		state.ExclusiveMaintenance = gateway.ExclusiveMaintenance
		state.IsOwnershipConflict = func(err error) bool {
			var conflict *gateway.OwnershipConflict
			return errors.As(err, &conflict)
		}
		state.LiveRuntimeOwner = gateway.LiveRuntimeOwner
		state.CanonicalHome = gateway.CanonicalHome
		state.ProfileLabelForHome = gateway.ProfileLabelForHome
		state.ReadGatewayLockRecord = gateway.ReadGatewayLockRecord
	}
}
