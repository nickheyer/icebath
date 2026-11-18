//go:build !icebath_core && !icebath_noextras

package pm

// Binds the package manager's Python environment operations to their exported entry points
func init() {
	syncVenvEntry = managedSyncVenv
	ensurePythonToolEntry = managedEnsurePythonTool
	ensureExtraEntry = managedEnsureExtra
	extraSupportedEntry = managedExtraSupported
	pythonToolEntry = managedPythonTool
	venvCommandEntry = managedVenvCommand
	anchorImportableEntry = managedAnchorImportable
	requirementAppliesHereEntry = managedRequirementAppliesHere
	activationEnvironmentEntry = managedActivationEnvironment
	venvIsCurrentEntry = managedVenvIsCurrent
	candidateMembersEntry = managedCandidateMembers
	coreRequirementErrorEntry = managedCoreRequirementError
	anchorSitePackagesEntry = managedAnchorSitePackages
	availableEntry = managedAvailable
	collectGenerationsEntry = managedCollectGenerations
	collectRuntimeGenerationsEntry = managedCollectRuntimeGenerations
	ensureRuntimeEnvironmentEntry = managedEnsureRuntimeEnvironment
	extraPythonCommandEntry = managedExtraPythonCommand
	refreshDependenciesEntry = managedRefreshDependencies
	runtimeEnvironmentReadyEntry = managedRuntimeEnvironmentReady
	nemoRelayWheelEntry = managedNemoRelayWheel
	activateDependenciesEntry = managedActivateDependencies
	runtimeRequirementsEntry = managedRuntimeRequirements
}
