//go:build !icebath_core && !icebath_noextras

package pm

// Binds the package manager's Python environment operations to their exported entry points
func init() {
	syncVenvEntry = managedSyncVenv
	ensurePythonToolEntry = managedEnsurePythonTool
	ensureExtraEntry = managedEnsureExtra
	extraSupportedEntry = managedExtraSupported
	payloadVenvEntry = managedPayloadVenv
	selectedVenvEntry = managedSelectedVenv
	sitePackagesEntry = managedSitePackages
	pythonToolEntry = managedPythonTool
	pinnedPythonVersionEntry = managedPinnedPythonVersion
	venvCommandEntry = managedVenvCommand
	baseVenvEntry = managedBaseVenv
	anchorImportableEntry = managedAnchorImportable
	projectPythonEntry = managedProjectPython
	selectedSitePackagesEntry = managedSelectedSitePackages
	selectedEntryPointsEntry = managedSelectedEntryPoints
	resolveModuleOriginEntry = managedResolveModuleOrigin
	runningFromSelectedEnvironmentEntry = managedRunningFromSelectedEnvironment
	distInstalledEntry = managedDistInstalled
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
	committedVenvEntry = managedCommittedVenv
	runtimeRequirementsEntry = managedRuntimeRequirements
}
